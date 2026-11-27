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
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

// The API server accepts a namespace key among the custom tags, and the
// operator sets the tag itself. A value that is not used has to be said
// somewhere the user looks.
func TestReconcile_ANamespaceTheMonitorClaimsIsReportedAsNotUsed(t *testing.T) {
	replaced := func(h *reconcileHarness) []string {
		var out []string
		for _, e := range drain(h.recorder) {
			if strings.HasPrefix(e, "Warning NamespaceTagReplaced ") {
				out = append(out, e)
			}
		}
		return out
	}

	monitor := unitMonitor()
	monitor.Spec.Database.CustomTags = map[string]string{"namespace": "billing", "env": "prod"}
	h := newReconcileHarness(t, monitor, unitSecret())
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	events := replaced(h)
	if len(events) != 1 || !strings.Contains(events[0], `spec.database.customTags.namespace is "billing", and is not used`) ||
		!strings.Contains(events[0], `the namespace of the monitor, "team"`) {
		t.Errorf("events %q", events)
	}
	// Once for a configuration, not at every look.
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	if again := replaced(h); len(again) != 0 {
		t.Errorf("said again: %q", again)
	}

	for name, tags := range map[string]map[string]string{
		"no such key":             {"env": "prod"},
		"the monitor's namespace": {"namespace": "team"},
		"no tags":                 nil,
	} {
		monitor := unitMonitor()
		monitor.Spec.Database.CustomTags = tags
		h := newReconcileHarness(t, monitor, unitSecret())
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if events := replaced(h); len(events) != 0 {
			t.Errorf("%s: events %q", name, events)
		}
	}
}

var _ = Describe("The collector's configuration", func() {
	var ns string

	BeforeEach(func() {
		ns = testNamespace("test-config-")
		secret := &corev1.Secret{StringData: map[string]string{"username": "pgwatch", "password": "secret"}}
		secret.Name, secret.Namespace = "pg-creds", ns
		createForSpec(secret)
	})

	// generated returns the ConfigMap and the arguments of the collector.
	generated := func(g Gomega, key types.NamespacedName) (map[string]string, []string) {
		config, err := mountedConfigMap(k8sClient, key)
		g.Expect(err).NotTo(HaveOccurred())
		var dep appsv1.Deployment
		g.Expect(k8sClient.Get(context.TODO(),
			types.NamespacedName{Name: "pgpilot-" + key.Name, Namespace: key.Namespace}, &dep)).To(Succeed())
		g.Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
		// Both from one reconcile: the pod template names the hash of the
		// ConfigMap it was made for, which is the one the status reports.
		var stored pgpilotv1.PgpilotMonitor
		g.Expect(k8sClient.Get(context.TODO(), key, &stored)).To(Succeed())
		g.Expect(stored.Status.ConfigHash).NotTo(BeEmpty())
		g.Expect(dep.Spec.Template.Annotations[builder.AnnotationConfigHash]).To(Equal(stored.Status.ConfigHash))
		return config.Data, dep.Spec.Template.Spec.Containers[0].Args
	}

	// A metrics.yaml takes the place of the registry inside the image. It
	// has to come and go with the monitor's own metrics, in the ConfigMap
	// that is stored and in the arguments of the pod, not only in what the
	// operator applies.
	It("holds a metrics.yaml only while the monitor has metrics of its own", func() {
		monitor := minimalMonitor(ns, "registry")
		createForSpec(monitor)
		key := client.ObjectKeyFromObject(monitor)

		presetOnly := func(g Gomega) {
			data, args := generated(g, key)
			g.Expect(data).To(HaveLen(1))
			g.Expect(data).To(HaveKey(builder.SourcesKey))
			g.Expect(data[builder.SourcesKey]).To(ContainSubstring("preset_metrics: basic"))
			g.Expect(args).NotTo(ContainElement("--metrics"))
		}
		Eventually(presetOnly, timeout, interval).Should(Succeed())

		// The operator writes the status of the monitor after it has
		// applied the Deployment; an update that meets that write is
		// refused and made again.
		setCustom := func(custom []any) {
			GinkgoHelper()
			Eventually(func(g Gomega) {
				stored := &unstructured.Unstructured{}
				stored.SetGroupVersionKind(pgpilotv1.GroupVersion.WithKind("PgpilotMonitor"))
				g.Expect(k8sClient.Get(context.TODO(), key, stored)).To(Succeed())
				if custom == nil {
					unstructured.RemoveNestedField(stored.Object, "spec", "metrics", "custom")
				} else {
					g.Expect(unstructured.SetNestedSlice(stored.Object, custom, "spec", "metrics", "custom")).To(Succeed())
				}
				g.Expect(k8sClient.Update(context.TODO(), stored)).To(Succeed())
			}, timeout, interval).Should(Succeed())
		}

		setCustom([]any{map[string]any{"name": "queue_depth", "sqls": map[string]any{"14": "SELECT 1 AS depth"}}})
		Eventually(func(g Gomega) {
			data, args := generated(g, key)
			g.Expect(data).To(HaveLen(2))
			// The monitor's own metric, and the built-in ones its preset
			// needs: the file replaces the registry of the image.
			g.Expect(data[builder.MetricsKey]).To(ContainSubstring("queue_depth"))
			g.Expect(data[builder.MetricsKey]).To(ContainSubstring("db_size"))
			g.Expect(data[builder.SourcesKey]).NotTo(ContainSubstring("preset_metrics"))
			g.Expect(args).To(ContainElements("--metrics", "/etc/pgwatch/metrics.yaml"))
		}, timeout, interval).Should(Succeed())

		setCustom(nil)
		Eventually(presetOnly, timeout, interval).Should(Succeed())
	})

	It("names the namespace of the monitor in its source", func() {
		monitor := withSpec(minimalMonitor(ns, "tagged"),
			map[string]any{"env": "prod", "namespace": "somebody-elses"}, "database", "customTags")
		createForSpec(monitor)
		Eventually(func(g Gomega) string {
			data, _ := generated(g, client.ObjectKeyFromObject(monitor))
			return data[builder.SourcesKey]
		}, timeout, interval).Should(And(
			ContainSubstring("namespace: "+ns),
			ContainSubstring("env: prod"),
			ContainSubstring("name: tagged"),
			Not(ContainSubstring("somebody-elses")),
		))
	})
})
