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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbv1alpha1 "github.com/bpalko/sprout/api/v1alpha1"
	"github.com/bpalko/sprout/internal/provisioner"
)

const tenantPassword = "initial-pass"

type recordingProvisioner struct {
	lastConn provisioner.ConnectionConfig
}

func (p *recordingProvisioner) EnsureDatabase(_ context.Context, conn provisioner.ConnectionConfig, _ provisioner.DatabaseSpec) error {
	p.lastConn = conn
	return nil
}

func (p *recordingProvisioner) EnsureRole(_ context.Context, conn provisioner.ConnectionConfig, spec provisioner.RoleSpec, rotate bool) (provisioner.Credentials, error) {
	p.lastConn = conn
	if rotate {
		return provisioner.Credentials{Username: spec.RoleName, Password: tenantPassword}, nil
	}
	return provisioner.Credentials{Username: spec.RoleName}, nil
}

func (p *recordingProvisioner) Teardown(_ context.Context, _ provisioner.ConnectionConfig, _ provisioner.DatabaseSpec, _ string) error {
	return nil
}

var _ = Describe("credentials Secret", func() {
	const sproutName = "sprout-secret-ssl"

	var (
		fake       *recordingProvisioner
		reconciler *SproutReconciler
		sproutKey  types.NamespacedName
	)

	BeforeEach(func() {
		fake = &recordingProvisioner{}
		reconciler = &SproutReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Provisioners: map[string]provisioner.Provisioner{
				sampleProvider: fake,
			},
		}
		sproutKey = types.NamespacedName{Name: sproutName, Namespace: sampleNamespace}

		admin := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: sampleAdminSecret, Namespace: sampleNamespace},
			Data: map[string][]byte{
				corev1.BasicAuthUsernameKey: []byte("postgres"),
				corev1.BasicAuthPasswordKey: []byte("s3cret"),
			},
		}
		err := k8sClient.Create(ctx, admin)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	})

	AfterEach(func() {
		sprout := &dbv1alpha1.Sprout{}
		err := k8sClient.Get(ctx, sproutKey, sprout)
		if err == nil {
			Expect(k8sClient.Delete(ctx, sprout)).To(Succeed())
		}
		secret := &corev1.Secret{}
		err = k8sClient.Get(ctx, sproutKey, secret)
		if err == nil {
			Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
		}
	})

	It("publishes default sslMode and updates it without rotating the password", func() {
		sprout := &dbv1alpha1.Sprout{
			ObjectMeta: metav1.ObjectMeta{Name: sproutName, Namespace: sampleNamespace},
			Spec: dbv1alpha1.SproutSpec{
				Provider: sampleProvider,
				Connection: dbv1alpha1.ConnectionSpec{
					Host:           sampleHost,
					Port:           5432,
					AdminSecretRef: corev1.LocalObjectReference{Name: sampleAdminSecret},
				},
			},
		}
		Expect(k8sClient.Create(ctx, sprout)).To(Succeed())

		req := reconcile.Request{NamespacedName: sproutKey}
		_, err := reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
		_, err = reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, sproutKey, secret)).To(Succeed())
		Expect(secretValue(secret, secretKeyPGSSLMode)).To(Equal(provisioner.DefaultSSLMode))
		Expect(secretValue(secret, secretKeyPGPassword)).To(Equal(tenantPassword))
		parsed, err := url.Parse(secretValue(secret, secretKeyDatabaseURL))
		Expect(err).NotTo(HaveOccurred())
		Expect(parsed.Query().Get("sslmode")).To(Equal(provisioner.DefaultSSLMode))
		Expect(fake.lastConn.SSLMode).To(Equal(provisioner.DefaultSSLMode))

		current := &dbv1alpha1.Sprout{}
		Expect(k8sClient.Get(ctx, sproutKey, current)).To(Succeed())
		current.Spec.Connection.SSLMode = connTestSSLRequire
		Expect(k8sClient.Update(ctx, current)).To(Succeed())

		_, err = reconciler.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		updated := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, sproutKey, updated)).To(Succeed())
		Expect(secretValue(updated, secretKeyPGSSLMode)).To(Equal(connTestSSLRequire))
		Expect(secretValue(updated, secretKeyPGPassword)).To(Equal(tenantPassword))
		parsed, err = url.Parse(secretValue(updated, secretKeyDatabaseURL))
		Expect(err).NotTo(HaveOccurred())
		Expect(parsed.Query().Get("sslmode")).To(Equal(connTestSSLRequire))
		Expect(fake.lastConn.SSLMode).To(Equal(connTestSSLRequire))
	})
})
