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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	pgpilotv1 "github.com/jamalshahverdiyev/pgpilot-operator/api/v1"
)

var _ = Describe("PgpilotMetricLibrary Controller", func() {
	var (
		ns  *corev1.Namespace
		lib *pgpilotv1.PgpilotMetricLibrary
	)

	BeforeEach(func() {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-lib-ns-",
			},
		}
		Expect(k8sClient.Create(context.TODO(), ns)).To(Succeed())

		lib = &pgpilotv1.PgpilotMetricLibrary{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-lib",
				Namespace: ns.Name,
			},
			Spec: pgpilotv1.PgpilotMetricLibrarySpec{
				Metrics: []pgpilotv1.MetricDefinition{
					{
						Name: "test_metric",
						SQLs: map[string]string{"13": "SELECT 1"},
					},
				},
			},
		}
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(context.TODO(), ns)).To(Succeed())
	})

	It("should set Valid condition to True for a library with metrics", func() {
		Expect(k8sClient.Create(context.TODO(), lib)).To(Succeed())

		Eventually(func() bool {
			var l pgpilotv1.PgpilotMetricLibrary
			if err := k8sClient.Get(context.TODO(), types.NamespacedName{Name: "test-lib", Namespace: ns.Name}, &l); err != nil {
				return false
			}
			for _, c := range l.Status.Conditions {
				if c.Type == "Valid" && c.Status == metav1.ConditionTrue {
					return true
				}
			}
			return false
		}, timeout, interval).Should(BeTrue())
	})

	It("should set ObservedGeneration on status", func() {
		Expect(k8sClient.Create(context.TODO(), lib)).To(Succeed())

		Eventually(func() int64 {
			var l pgpilotv1.PgpilotMetricLibrary
			if err := k8sClient.Get(context.TODO(), types.NamespacedName{Name: "test-lib", Namespace: ns.Name}, &l); err != nil {
				return 0
			}
			return l.Status.ObservedGeneration
		}, timeout, interval).Should(BeNumerically(">", 0))
	})
})
