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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

// These specs exercise the CRD schema through the real API server. Objects
// are sent as unstructured data because the typed structs cannot even hold
// the invalid values we need to prove are rejected.
var _ = Describe("CRD validation", func() {
	var ns string

	BeforeEach(func() {
		created := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "test-validation-"}}
		Expect(k8sClient.Create(context.TODO(), created)).To(Succeed())
		ns = created.Name
	})

	// metric returns one metric definition; a nil interval omits the field.
	metric := func(interval any) map[string]any {
		m := map[string]any{
			"name": "probe_metric",
			"sqls": map[string]any{"14": "SELECT 1 AS value"},
		}
		if interval != nil {
			m["interval"] = interval
		}
		return m
	}

	newLibrary := func(name string, interval any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": pgpilotv1.GroupVersion.String(),
			"kind":       "PgpilotMetricLibrary",
			"metadata":   map[string]any{"name": name, "namespace": ns},
			"spec":       map[string]any{"metrics": []any{metric(interval)}},
		}}
	}

	newMonitor := func(name string, interval any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": pgpilotv1.GroupVersion.String(),
			"kind":       "PgpilotMonitor",
			"metadata":   map[string]any{"name": name, "namespace": ns},
			"spec": map[string]any{
				"database": map[string]any{
					"host":              "pg.example.com",
					"database":          "appdb",
					"credentialsSecret": map[string]any{"name": "pg-creds"},
				},
				"metrics": map[string]any{"custom": []any{metric(interval)}},
				"sinks":   map[string]any{"prometheus": map[string]any{"enabled": true}},
			},
		}}
	}

	// kinds lets every interval case run against both places a
	// MetricDefinition can appear.
	kinds := map[string]func(string, any) *unstructured.Unstructured{
		"PgpilotMetricLibrary.spec.metrics[]":  newLibrary,
		"PgpilotMonitor.spec.metrics.custom[]": newMonitor,
	}

	expectRejected := func(obj *unstructured.Unstructured) {
		GinkgoHelper()
		err := k8sClient.Create(context.TODO(), obj)
		Expect(err).To(HaveOccurred(), "the API server must reject this object")
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "want an Invalid error, got: %v", err)
		Expect(err.Error()).To(ContainSubstring("interval"))
	}

	Describe("metric interval", func() {
		// A value Go cannot parse as a duration used to be stored as-is and
		// then broke decoding of the whole kind for the operator: no monitor
		// reconciled any more and the manager crash-looped after a restart.
		DescribeTable("rejects values that are not a duration of at least one second",
			func(interval any) {
				for where, build := range kinds {
					By(where)
					expectRejected(build("bad-interval", interval))
				}
			},
			Entry("bare number as string", "60"),
			Entry("unit Go does not know", "1d"),
			Entry("words", "1 minute"),
			Entry("empty string", ""),
			Entry("garbage", "soon"),
			Entry("ISO 8601 form", "PT1M"),
			Entry("overflowing value", "99999999999999h"),
			Entry("sub-second", "500ms"),
			Entry("just below one second", "999ms"),
			Entry("zero", "0s"),
			Entry("negative", "-30s"),
		)

		DescribeTable("accepts durations of one second or more",
			func(interval string) {
				for where, build := range kinds {
					By(where)
					Expect(k8sClient.Create(context.TODO(), build("good-interval", interval))).To(Succeed())
				}
			},
			Entry("exactly one second", "1s"),
			Entry("seconds", "30s"),
			Entry("minutes", "5m"),
			Entry("hours", "1h"),
			Entry("a day", "24h"),
			Entry("a week", "168h"),
			Entry("compound", "1m30s"),
			Entry("fractional minutes", "1.5m"),
			Entry("milliseconds adding up to more than a second", "1500ms"),
		)

		It("accepts a metric without an interval", func() {
			for where, build := range kinds {
				By(where)
				Expect(k8sClient.Create(context.TODO(), build("no-interval", nil))).To(Succeed())
			}
		})

		It("rejects a non-string interval", func() {
			for where, build := range kinds {
				By(where)
				err := k8sClient.Create(context.TODO(), build("numeric-interval", int64(60)))
				Expect(err).To(HaveOccurred())
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "want an Invalid error, got: %v", err)
			}
		})

		It("rejects an update that turns a valid interval into an invalid one", func() {
			for where, build := range kinds {
				By(where)
				obj := build("update-interval", "30s")
				Expect(k8sClient.Create(context.TODO(), obj)).To(Succeed())

				Eventually(func(g Gomega) {
					current := obj.DeepCopy()
					g.Expect(k8sClient.Get(context.TODO(), client.ObjectKeyFromObject(obj), current)).To(Succeed())

					path := []string{"spec", "metrics"}
					if current.GetKind() == "PgpilotMonitor" {
						path = []string{"spec", "metrics", "custom"}
					}
					metrics, found, err := unstructured.NestedSlice(current.Object, path...)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(found).To(BeTrue())
					metrics[0].(map[string]any)["interval"] = "1d"
					g.Expect(unstructured.SetNestedSlice(current.Object, metrics, path...)).To(Succeed())

					err = k8sClient.Update(context.TODO(), current)
					g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), "want an Invalid error, got: %v", err)
				}, timeout, interval).Should(Succeed())
			}
		})

		It("keeps every stored object decodable by the typed client", func() {
			for _, bad := range []string{"60", "1d", ""} {
				for _, build := range kinds {
					Expect(k8sClient.Create(context.TODO(), build("poison", bad))).NotTo(Succeed())
				}
			}
			Expect(k8sClient.Create(context.TODO(), newLibrary("healthy", "45s"))).To(Succeed())
			Expect(k8sClient.Create(context.TODO(), newMonitor("healthy", "45s"))).To(Succeed())

			// A typed List decodes every object of the kind in the
			// namespace; one undecodable interval would fail the whole call.
			var libs pgpilotv1.PgpilotMetricLibraryList
			Expect(k8sClient.List(context.TODO(), &libs, client.InNamespace(ns))).To(Succeed())
			Expect(libs.Items).To(HaveLen(1))
			Expect(libs.Items[0].Spec.Metrics[0].Interval.Duration.Seconds()).To(BeEquivalentTo(45))

			var monitors pgpilotv1.PgpilotMonitorList
			Expect(k8sClient.List(context.TODO(), &monitors, client.InNamespace(ns))).To(Succeed())
			Expect(monitors.Items).To(HaveLen(1))
			Expect(monitors.Items[0].Spec.Metrics.Custom[0].Interval.Duration.Seconds()).To(BeEquivalentTo(45))

			// The operator's own cache must still serve the kind as well.
			Eventually(func(g Gomega) {
				var lib pgpilotv1.PgpilotMetricLibrary
				g.Expect(k8sClient.Get(context.TODO(),
					types.NamespacedName{Name: "healthy", Namespace: ns}, &lib)).To(Succeed())
				g.Expect(lib.Status.ObservedGeneration).To(BeNumerically(">", 0),
					"library controller must still reconcile after rejected objects")
			}, timeout, interval).Should(Succeed())
		})
	})
})
