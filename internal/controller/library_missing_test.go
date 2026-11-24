/*
Copyright 2026.

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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

var _ = Describe("A monitor that refers to a metric library that does not exist", func() {
	It("says so in its status, and starts collecting once the library appears", func() {
		ns := testNamespace("test-library-")
		secret := &corev1.Secret{StringData: map[string]string{"username": "pgwatch", "password": "secret"}}
		secret.Name, secret.Namespace = "pg-creds", ns
		createForSpec(secret)

		monitor := withSpec(minimalMonitor(ns, "needs-library"),
			map[string]any{"fromLibraries": []any{map[string]any{"name": "shared"}}}, "metrics")
		createForSpec(monitor)
		key := client.ObjectKeyFromObject(monitor)

		Eventually(func(g Gomega) {
			var got pgpilotv1.PgpilotMonitor
			g.Expect(k8sClient.Get(context.TODO(), key, &got)).To(Succeed())
			g.Expect(got.Status.Conditions).To(ContainElement(SatisfyAll(
				HaveField("Type", "Ready"),
				HaveField("Status", metav1.ConditionFalse),
				HaveField("Reason", "LibraryResolveFailed"),
				HaveField("Message", ContainSubstring(`library "shared"`)),
			)))
		}, timeout, interval).Should(Succeed())

		Eventually(func(g Gomega) {
			var events corev1.EventList
			g.Expect(k8sClient.List(context.TODO(), &events, client.InNamespace(ns))).To(Succeed())
			g.Expect(events.Items).To(ContainElement(SatisfyAll(
				HaveField("InvolvedObject.Name", key.Name),
				HaveField("Type", corev1.EventTypeWarning),
				HaveField("Reason", "LibraryResolveFailed"),
			)))
		}, timeout, interval).Should(Succeed())
		err := k8sClient.Get(context.TODO(),
			types.NamespacedName{Name: builder.ResourceName(key.Name), Namespace: ns}, &appsv1.Deployment{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no collector without its metric definitions, got: %v", err)

		library := libraryOf(ns, "shared", []any{metricNamed("queue_depth")})
		createForSpec(library)
		Eventually(collectorImage(k8sClient, key), timeout, interval).Should(Equal(currentImage))

		// The library controller reports through the same client as the
		// monitor controller: its status carries the operator's name too.
		Eventually(func(g Gomega) []string {
			var got pgpilotv1.PgpilotMetricLibrary
			g.Expect(k8sClient.Get(context.TODO(), client.ObjectKeyFromObject(library), &got)).To(Succeed())
			var statusWriters []string
			for _, entry := range got.ManagedFields {
				if entry.Subresource == "status" {
					statusWriters = append(statusWriters, entry.Manager)
				}
			}
			return statusWriters
		}, timeout, interval).Should(Equal([]string{"pgpilot-operator"}))
	})
})
