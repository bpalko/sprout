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
	"net/url"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dbv1alpha1 "github.com/bpalko/sprout/api/v1alpha1"
	"github.com/bpalko/sprout/internal/provisioner"
)

const (
	connTestSecretName = "admin"
	connTestHost       = "db.example"
	connTestSSLRequire = "require"
)

func newResolver(t *testing.T, secret *corev1.Secret) *SproutReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := dbv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &SproutReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(),
		Scheme: scheme,
	}
}

func adminSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: connTestSecretName, Namespace: "ns"},
		Data: map[string][]byte{
			corev1.BasicAuthUsernameKey: []byte("postgres"),
			corev1.BasicAuthPasswordKey: []byte("s3cret"),
		},
	}
}

func TestResolveConnectionDefaults(t *testing.T) {
	r := newResolver(t, adminSecret())
	sprout := &dbv1alpha1.Sprout{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
		Spec: dbv1alpha1.SproutSpec{
			Connection: dbv1alpha1.ConnectionSpec{
				Host:           connTestHost,
				Port:           5432,
				AdminSecretRef: corev1.LocalObjectReference{Name: connTestSecretName},
			},
		},
	}

	conn, err := r.resolveConnection(context.Background(), sprout)
	if err != nil {
		t.Fatal(err)
	}
	if conn.AdminDatabase != provisioner.DefaultAdminDatabase {
		t.Fatalf("AdminDatabase = %q, want %q", conn.AdminDatabase, provisioner.DefaultAdminDatabase)
	}
	if conn.SSLMode != provisioner.DefaultSSLMode {
		t.Fatalf("SSLMode = %q, want %q", conn.SSLMode, provisioner.DefaultSSLMode)
	}
}

func TestResolveConnectionOverrides(t *testing.T) {
	r := newResolver(t, adminSecret())
	sprout := &dbv1alpha1.Sprout{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
		Spec: dbv1alpha1.SproutSpec{
			Connection: dbv1alpha1.ConnectionSpec{
				Host:           connTestHost,
				Port:           5432,
				AdminSecretRef: corev1.LocalObjectReference{Name: connTestSecretName},
				AdminDatabase:  "app",
				SSLMode:        connTestSSLRequire,
			},
		},
	}

	conn, err := r.resolveConnection(context.Background(), sprout)
	if err != nil {
		t.Fatal(err)
	}
	if conn.AdminDatabase != "app" {
		t.Fatalf("AdminDatabase = %q, want app", conn.AdminDatabase)
	}
	if conn.SSLMode != connTestSSLRequire {
		t.Fatalf("SSLMode = %q, want %s", conn.SSLMode, connTestSSLRequire)
	}
}

func TestBuildSecretIncludesSSLMode(t *testing.T) {
	sprout := &dbv1alpha1.Sprout{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
	}
	secret := buildSecret(sprout, "s", provisioner.ConnectionConfig{
		Host: connTestHost, Port: 5432, SSLMode: connTestSSLRequire,
	}, "mydb", provisioner.Credentials{Username: "u", Password: "p"})

	if got := secret.StringData[secretKeyPGSSLMode]; got != connTestSSLRequire {
		t.Fatalf("PGSSLMODE = %q, want %s", got, connTestSSLRequire)
	}
	parsed, err := url.Parse(secret.StringData[secretKeyDatabaseURL])
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Query().Get("sslmode"); got != connTestSSLRequire {
		t.Fatalf("DATABASE_URL sslmode = %q, want %s", got, connTestSSLRequire)
	}
}

func TestBuildSecretDefaultsSSLMode(t *testing.T) {
	sprout := &dbv1alpha1.Sprout{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
	}
	secret := buildSecret(sprout, "s", provisioner.ConnectionConfig{
		Host: "h", Port: 5432,
	}, "db", provisioner.Credentials{Username: "u", Password: "p"})

	if got := secret.StringData[secretKeyPGSSLMode]; got != provisioner.DefaultSSLMode {
		t.Fatalf("PGSSLMODE = %q, want %q", got, provisioner.DefaultSSLMode)
	}
	parsed, err := url.Parse(secret.StringData[secretKeyDatabaseURL])
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Query().Get("sslmode"); got != provisioner.DefaultSSLMode {
		t.Fatalf("DATABASE_URL sslmode = %q, want %q", got, provisioner.DefaultSSLMode)
	}
}

func TestSecretDataDriftedOnSSLMode(t *testing.T) {
	sprout := &dbv1alpha1.Sprout{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"}}
	prefer := buildSecret(sprout, "s", provisioner.ConnectionConfig{
		Host: connTestHost, Port: 5432, SSLMode: provisioner.DefaultSSLMode,
	}, "mydb", provisioner.Credentials{Username: "u", Password: "tenant-pass"})
	require := buildSecret(sprout, "s", provisioner.ConnectionConfig{
		Host: connTestHost, Port: 5432, SSLMode: connTestSSLRequire,
	}, "mydb", provisioner.Credentials{Username: "u", Password: "tenant-pass"})

	existing := &corev1.Secret{Data: stringDataToData(prefer.StringData)}
	if !secretDataDrifted(existing, require.StringData) {
		t.Fatal("expected sslMode change to count as drift")
	}
	if secretDataDrifted(existing, prefer.StringData) {
		t.Fatal("expected identical connection fields not to count as drift")
	}
}

func TestPublishCredentialsSecretUpdatesSSLModeWithoutRotatingPassword(t *testing.T) {
	sprout := &dbv1alpha1.Sprout{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"}}
	const tenantPass = "tenant-pass"
	existing := buildSecret(sprout, "s", provisioner.ConnectionConfig{
		Host: connTestHost, Port: 5432, SSLMode: provisioner.DefaultSSLMode,
	}, "mydb", provisioner.Credentials{Username: "u", Password: tenantPass})
	existing.Data = stringDataToData(existing.StringData)
	existing.StringData = nil

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := dbv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	r := &SproutReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing.DeepCopy()).Build(),
		Scheme: scheme,
	}

	creds := provisioner.Credentials{Username: "u", Password: tenantPass}
	conn := provisioner.ConnectionConfig{Host: connTestHost, Port: 5432, SSLMode: connTestSSLRequire}
	if err := r.publishCredentialsSecret(context.Background(), sprout, "s", conn, "mydb", creds, existing, false); err != nil {
		t.Fatal(err)
	}

	got := &corev1.Secret{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "s", Namespace: "ns"}, got); err != nil {
		t.Fatal(err)
	}
	if got := secretValue(got, secretKeyPGSSLMode); got != connTestSSLRequire {
		t.Fatalf("PGSSLMODE = %q, want %s", got, connTestSSLRequire)
	}
	if got := secretValue(got, secretKeyPGPassword); got != tenantPass {
		t.Fatalf("PGPASSWORD = %q, want %s", got, tenantPass)
	}
	parsed, err := url.Parse(secretValue(got, secretKeyDatabaseURL))
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Query().Get("sslmode"); got != connTestSSLRequire {
		t.Fatalf("DATABASE_URL sslmode = %q, want %s", got, connTestSSLRequire)
	}
}

func stringDataToData(in map[string]string) map[string][]byte {
	out := make(map[string][]byte, len(in))
	for k, v := range in {
		out[k] = []byte(v)
	}
	return out
}

func secretValue(s *corev1.Secret, key string) string {
	if s.StringData != nil {
		if v, ok := s.StringData[key]; ok {
			return v
		}
	}
	if s.Data != nil {
		return string(s.Data[key])
	}
	return ""
}
