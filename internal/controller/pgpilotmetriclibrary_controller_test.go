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

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

var _ = Describe("PgpilotMetricLibrary Controller", func() {
	var (
		ns  *corev1.Namespace
		lib *pgpilotv1.PgpilotMetricLibrary
	)

	BeforeEach(func() {
		ns = &corev1.Namespace{
			GenerateName: "test-lib-ns-",
		}
		Expect(k8sClient.Create(context.TODO(), ns)).To(Succeed())

		lib = &pgpilotv1.PgpilotMetricLibrary{
			Name:      "test-lib",
			Namespace: ns.Name,
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
		createForSpec(lib)

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

	// With the permissions of the shipped role, and through the manager's
	// cache: the status is a patch of the status subresource.
	It("should say how many definitions there are, and follow a change of the library", func() {
		createForSpec(lib)
		key := types.NamespacedName{Name: "test-lib", Namespace: ns.Name}
		reported := func(g Gomega) pgpilotv1.PgpilotMetricLibraryStatus {
			var l pgpilotv1.PgpilotMetricLibrary
			g.Expect(k8sClient.Get(context.TODO(), key, &l)).To(Succeed())
			g.Expect(l.Status.ObservedGeneration).To(Equal(l.Generation))
			return l.Status
		}
		valid := func(message string) OmegaMatcher {
			return ContainElement(SatisfyAll(
				HaveField("Type", "Valid"),
				HaveField("Status", metav1.ConditionTrue),
				HaveField("Reason", "Valid"),
				HaveField("Message", message),
			))
		}

		Eventually(func(g Gomega) {
			status := reported(g)
			g.Expect(status.MetricCount).To(BeEquivalentTo(1))
			g.Expect(status.Conditions).To(valid("Metric definitions that can be used: 1"))
		}, timeout, interval).Should(Succeed())

		Eventually(func(g Gomega) {
			var l pgpilotv1.PgpilotMetricLibrary
			g.Expect(k8sClient.Get(context.TODO(), key, &l)).To(Succeed())
			l.Spec.Metrics = append(l.Spec.Metrics, pgpilotv1.MetricDefinition{
				Name: "another_metric", SQLs: map[string]string{"0": "SELECT 2"},
			})
			g.Expect(k8sClient.Update(context.TODO(), &l)).To(Succeed())
		}, timeout, interval).Should(Succeed())

		Eventually(func(g Gomega) {
			status := reported(g)
			g.Expect(status.ObservedGeneration).To(BeEquivalentTo(2))
			g.Expect(status.MetricCount).To(BeEquivalentTo(2))
			g.Expect(status.Conditions).To(valid("Metric definitions that can be used: 2"))
		}, timeout, interval).Should(Succeed())
	})

	It("should set ObservedGeneration on status", func() {
		createForSpec(lib)

		Eventually(func() int64 {
			var l pgpilotv1.PgpilotMetricLibrary
			if err := k8sClient.Get(context.TODO(), types.NamespacedName{Name: "test-lib", Namespace: ns.Name}, &l); err != nil {
				return 0
			}
			return l.Status.ObservedGeneration
		}, timeout, interval).Should(BeNumerically(">", 0))
	})
})
