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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	dbv1alpha1 "github.com/bpalko/sprout/api/v1alpha1"
	"github.com/bpalko/sprout/internal/provisioner"
)

var _ = Describe("ConnectionSpec", func() {
	It("defaults adminDatabase and sslMode when omitted", func() {
		sprout := &dbv1alpha1.Sprout{
			ObjectMeta: metav1.ObjectMeta{Name: "conn-defaults", Namespace: sampleNamespace},
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
		DeferCleanup(func() {
			Expect(k8sClient.Delete(ctx, sprout)).To(Succeed())
		})

		created := &dbv1alpha1.Sprout{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sprout.Name, Namespace: sprout.Namespace}, created)).To(Succeed())
		Expect(created.Spec.Connection.AdminDatabase).To(Equal(provisioner.DefaultAdminDatabase))
		Expect(created.Spec.Connection.SSLMode).To(Equal(provisioner.DefaultSSLMode))
	})

	It("rejects an unknown sslMode", func() {
		sprout := &dbv1alpha1.Sprout{
			ObjectMeta: metav1.ObjectMeta{Name: "conn-bad-ssl", Namespace: sampleNamespace},
			Spec: dbv1alpha1.SproutSpec{
				Provider: sampleProvider,
				Connection: dbv1alpha1.ConnectionSpec{
					Host:           sampleHost,
					Port:           5432,
					AdminSecretRef: corev1.LocalObjectReference{Name: sampleAdminSecret},
					SSLMode:        "banana",
				},
			},
		}
		err := k8sClient.Create(ctx, sprout)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("sslMode"))
	})
})
