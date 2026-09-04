/*
Copyright 2026 bpalko.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	dbv1alpha1 "github.com/bpalko/sprout/api/v1alpha1"
	"github.com/bpalko/sprout/internal/provisioner"
)

// defaultConnectionLimit is applied when a Sprout doesn't set
// spec.connectionLimit, keeping a single preview environment from being
// able to exhaust a shared instance's max_connections by default.
const defaultConnectionLimit int32 = 20

// resyncInterval is how often a Ready Sprout is re-reconciled even without
// a Kubernetes-side event, so drift that happens directly against the
// target database (e.g. someone manually drops the role) gets noticed and
// self-healed instead of going undetected until the next unrelated event.
const resyncInterval = 5 * time.Minute

// Secret data keys published for the app Deployment to consume.
const (
	secretKeyDatabaseURL = "DATABASE_URL"
	secretKeyPGHost      = "PGHOST"
	secretKeyPGPort      = "PGPORT"
	secretKeyPGUser      = "PGUSER"
	secretKeyPGPassword  = "PGPASSWORD"
	secretKeyPGDatabase  = "PGDATABASE"
)

// SproutReconciler reconciles a Sprout object.
type SproutReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Provisioners maps a Sprout's spec.provider value (e.g. "postgres")
	// to the Provisioner implementation that handles it. Adding a new
	// backend means adding a new entry here, not changing this
	// reconciler.
	Provisioners map[string]provisioner.Provisioner
}

// +kubebuilder:rbac:groups=db.sproutdb.dev,resources=sprouts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=db.sproutdb.dev,resources=sprouts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=db.sproutdb.dev,resources=sprouts/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives a Sprout towards having an isolated logical database,
// role, and credentials Secret that match its spec: creating what's
// missing, rotating credentials if the Secret disappeared, and tearing
// everything down when the Sprout itself is deleted.
func (r *SproutReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	sprout := &dbv1alpha1.Sprout{}
	if err := r.Get(ctx, req.NamespacedName, sprout); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	databaseName, nameConflict := r.resolveDatabaseName(sprout)
	roleName := deriveRoleName(databaseName)
	ownerID := string(sprout.UID)

	prov, provErr := r.provisionerFor(sprout)

	if !sprout.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, sprout, prov, provErr, databaseName, roleName, ownerID)
	}

	if !controllerutil.ContainsFinalizer(sprout, dbv1alpha1.SproutFinalizer) {
		controllerutil.AddFinalizer(sprout, dbv1alpha1.SproutFinalizer)
		if err := r.Update(ctx, sprout); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		// Requeue rather than continue in this pass: the Update above
		// changed .metadata.resourceVersion, and continuing to write
		// .status against the stale object risks a conflict. The Update
		// itself also generates a watch event for this Sprout (the
		// controller watches it directly via For()), so this could rely
		// on that alone. Requesting a requeue explicitly just keeps this
		// robust to watch-event delivery being delayed or coalesced.
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil
	}

	if nameConflict {
		return r.failStatus(ctx, sprout, "DatabaseNameImmutable", fmt.Sprintf(
			"spec.databaseName no longer matches the database already provisioned for this Sprout (%q); refusing to abandon it and provision a different one",
			databaseName,
		))
	}

	if provErr != nil {
		return r.failStatus(ctx, sprout, "UnknownProvider", provErr.Error())
	}

	conn, err := r.resolveConnection(ctx, sprout)
	if err != nil {
		log.Error(err, "resolving admin connection", "sprout", req.NamespacedName)
		if cerr := r.recordFailure(ctx, sprout, "AdminSecretUnavailable", err.Error()); cerr != nil {
			return ctrl.Result{}, cerr
		}
		// The admin Secret may simply not exist yet; retry rather than
		// give up permanently.
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	dbSpec := provisioner.DatabaseSpec{DatabaseName: databaseName, OwnerID: ownerID}
	if err := prov.EnsureDatabase(ctx, conn, dbSpec); err != nil {
		return r.failStatus(ctx, sprout, "DatabaseProvisionFailed", err.Error())
	}
	r.setCondition(sprout, dbv1alpha1.ConditionDatabaseProvisioned, metav1.ConditionTrue, "Provisioned", "database exists and is owned by this Sprout")

	secretName := sprout.Name
	existing := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{Namespace: sprout.Namespace, Name: secretName}, existing)
	rotate := apierrors.IsNotFound(err)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("checking for existing credentials secret: %w", err)
	}

	connLimit := defaultConnectionLimit
	if sprout.Spec.ConnectionLimit != nil {
		connLimit = *sprout.Spec.ConnectionLimit
	}
	roleSpec := provisioner.RoleSpec{
		DatabaseName:    databaseName,
		RoleName:        roleName,
		OwnerID:         ownerID,
		ConnectionLimit: connLimit,
	}
	creds, err := prov.EnsureRole(ctx, conn, roleSpec, rotate)
	if err != nil {
		return r.failStatus(ctx, sprout, "RoleProvisionFailed", err.Error())
	}

	if rotate {
		secret := buildSecret(sprout, secretName, conn, databaseName, creds)
		if err := controllerutil.SetControllerReference(sprout, secret, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("setting owner reference on secret: %w", err)
		}
		if err := r.Create(ctx, secret); err != nil {
			return ctrl.Result{}, fmt.Errorf("creating credentials secret: %w", err)
		}
		log.Info("credentials secret (re)created", "secret", secretName, "reason", "missing on reconcile")
	}
	r.setCondition(sprout, dbv1alpha1.ConditionSecretReady, metav1.ConditionTrue, "Published", "credentials secret exists and matches the current role")

	sprout.Status.Phase = dbv1alpha1.SproutPhaseReady
	sprout.Status.DatabaseName = databaseName
	sprout.Status.RoleName = roleName
	sprout.Status.SecretName = secretName
	sprout.Status.ObservedGeneration = sprout.Generation
	r.setCondition(sprout, dbv1alpha1.ConditionReady, metav1.ConditionTrue, "Provisioned", "database, role, and secret are all ready")
	if err := r.Status().Update(ctx, sprout); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

func (r *SproutReconciler) reconcileDelete(
	ctx context.Context,
	sprout *dbv1alpha1.Sprout,
	prov provisioner.Provisioner,
	provErr error,
	databaseName, roleName, ownerID string,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(sprout, dbv1alpha1.SproutFinalizer) {
		return ctrl.Result{}, nil
	}

	if provErr == nil {
		conn, err := r.resolveConnection(ctx, sprout)
		switch {
		case err != nil && apierrors.IsNotFound(err):
			// Admin credentials are already gone too; nothing we can do
			// to reach the target instance. Proceed with removing the
			// finalizer so the Sprout doesn't get stuck forever. This
			// mirrors how a Secret's owner reference works in that cleanup is
			// best effort once the admin path itself is gone.
			logf.FromContext(ctx).Info("admin secret gone during teardown; skipping remote cleanup", "sprout", client.ObjectKeyFromObject(sprout))
		case err != nil:
			return ctrl.Result{}, fmt.Errorf("resolving admin connection for teardown: %w", err)
		default:
			dbSpec := provisioner.DatabaseSpec{DatabaseName: databaseName, OwnerID: ownerID}
			if err := prov.Teardown(ctx, conn, dbSpec, roleName); err != nil {
				return ctrl.Result{}, fmt.Errorf("tearing down database: %w", err)
			}
		}
	}

	controllerutil.RemoveFinalizer(sprout, dbv1alpha1.SproutFinalizer)
	if err := r.Update(ctx, sprout); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// resolveDatabaseName returns the database name this Sprout is (or will be)
// provisioned under, and whether spec.databaseName conflicts with a name
// already recorded in status.
//
// TODO: I might wanna rethink this...
// Thoughts:
// It prefers status.DatabaseName once set, since that's what was
// actually created, over recomputing from spec. This matters because
// spec.databaseName's CEL immutability rule (self == oldSelf) can't be
// enforced across the transition from unset to set: the field has no
// oldSelf to compare against on that first write, so the API server has
// nothing to reject. Falling back to status here means a later change to
// spec.databaseName can never cause the controller to silently abandon the
// database it already created and start provisioning a different one
// under the same Sprout. The caller surfaces conflict as a Failed
// condition instead of acting on it.
func (r *SproutReconciler) resolveDatabaseName(sprout *dbv1alpha1.Sprout) (name string, conflict bool) {
	if sprout.Status.DatabaseName != "" {
		if sprout.Spec.DatabaseName != "" && sprout.Spec.DatabaseName != sprout.Status.DatabaseName {
			return sprout.Status.DatabaseName, true
		}
		return sprout.Status.DatabaseName, false
	}
	if sprout.Spec.DatabaseName != "" {
		return sprout.Spec.DatabaseName, false
	}
	return deriveDatabaseName(sprout.Namespace, sprout.Name), false
}

// provisionerFor looks up the Provisioner for sprout.Spec.Provider. The
// error it returns is a spec problem (unknown provider), not a transient
// one, so callers should surface it as a Failed condition rather than
// retrying.
func (r *SproutReconciler) provisionerFor(sprout *dbv1alpha1.Sprout) (provisioner.Provisioner, error) {
	prov, ok := r.Provisioners[sprout.Spec.Provider]
	if !ok {
		return nil, fmt.Errorf("no provisioner registered for provider %q", sprout.Spec.Provider)
	}
	return prov, nil
}

// resolveConnection resolves a Sprout's spec.connection plus its
// adminSecretRef into a provisioner.ConnectionConfig.
func (r *SproutReconciler) resolveConnection(ctx context.Context, sprout *dbv1alpha1.Sprout) (provisioner.ConnectionConfig, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: sprout.Namespace, Name: sprout.Spec.Connection.AdminSecretRef.Name}
	if err := r.Get(ctx, key, secret); err != nil {
		return provisioner.ConnectionConfig{}, err
	}

	user, ok := secret.Data[corev1.BasicAuthUsernameKey]
	if !ok {
		return provisioner.ConnectionConfig{}, fmt.Errorf("admin secret %s missing key %q", key, corev1.BasicAuthUsernameKey)
	}
	pass, ok := secret.Data[corev1.BasicAuthPasswordKey]
	if !ok {
		return provisioner.ConnectionConfig{}, fmt.Errorf("admin secret %s missing key %q", key, corev1.BasicAuthPasswordKey)
	}

	return provisioner.ConnectionConfig{
		Host:          sprout.Spec.Connection.Host,
		Port:          sprout.Spec.Connection.Port,
		AdminUser:     string(user),
		AdminPassword: string(pass),
		AdminDatabase: "postgres",
	}, nil
}

func buildSecret(sprout *dbv1alpha1.Sprout, name string, conn provisioner.ConnectionConfig, databaseName string, creds provisioner.Credentials) *corev1.Secret {
	dsn := (&url.URL{
		Scheme: "postgresql",
		User:   url.UserPassword(creds.Username, creds.Password),
		Host:   fmt.Sprintf("%s:%d", conn.Host, conn.Port),
		Path:   "/" + databaseName,
	}).String()

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: sprout.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "sprout",
				"db.sproutdb.dev/sprout":       sprout.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			secretKeyDatabaseURL: dsn,
			secretKeyPGHost:      conn.Host,
			secretKeyPGPort:      strconv.Itoa(int(conn.Port)),
			secretKeyPGUser:      creds.Username,
			secretKeyPGPassword:  creds.Password,
			secretKeyPGDatabase:  databaseName,
		},
	}
}

func (r *SproutReconciler) setCondition(sprout *dbv1alpha1.Sprout, condType string, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&sprout.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: sprout.Generation,
	})
}

// recordFailure records a Failed phase and a False Ready condition, then
// persists status. Unlike failStatus, its returned error reflects only
// whether that persistence itself succeeded (nil on success). Callers
// that need to decide their own ctrl.Result based on whether recording
// the failure worked (rather than always treating the underlying
// condition as terminal) should use this instead of failStatus.
func (r *SproutReconciler) recordFailure(ctx context.Context, sprout *dbv1alpha1.Sprout, reason, message string) error {
	sprout.Status.Phase = dbv1alpha1.SproutPhaseFailed
	r.setCondition(sprout, dbv1alpha1.ConditionReady, metav1.ConditionFalse, reason, message)
	if err := r.Status().Update(ctx, sprout); err != nil {
		return fmt.Errorf("updating failed status: %w", err)
	}
	return nil
}

// failStatus records a Failed phase and a False Ready condition via
// recordFailure, then always returns a non-nil error for callers
// that want this condition to unconditionally end Reconcile with an
// error (see controller-runtime's default). It returns
// the result/error pair the caller should return from Reconcile directly.
func (r *SproutReconciler) failStatus(ctx context.Context, sprout *dbv1alpha1.Sprout, reason, message string) (ctrl.Result, error) {
	if err := r.recordFailure(ctx, sprout, reason, message); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, fmt.Errorf("%s: %s", reason, message)
}

// SetupWithManager sets up the controller with the Manager.
func (r *SproutReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbv1alpha1.Sprout{}).
		Owns(&corev1.Secret{}).
		Named("sprout").
		Complete(r)
}
