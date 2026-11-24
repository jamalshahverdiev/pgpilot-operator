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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

// testNamespace creates a namespace for one spec and returns its name.
func testNamespace(prefix string) string {
	GinkgoHelper()
	ns := &corev1.Namespace{}
	ns.GenerateName = prefix
	Expect(k8sClient.Create(context.TODO(), ns)).To(Succeed())
	return ns.Name
}

// createForSpec creates an object and deletes it again when the spec ends.
//
// The manager of the suite reconciles everything that exists. A monitor left
// behind by an earlier spec - most have no Secret, so they are retried again
// and again - takes its share of the single reconcile worker and of the
// client's request budget, and later specs wait behind it.
func createForSpec(obj client.Object) {
	GinkgoHelper()
	Expect(k8sClient.Create(context.TODO(), obj)).To(Succeed())
	DeferCleanup(func() {
		_ = k8sClient.Delete(context.TODO(), obj)
	})
}

// minimalMonitor is the smallest PgpilotMonitor the API accepts: nothing that
// has a default is set.
func minimalMonitor(ns, name string) *unstructured.Unstructured {
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
			"metrics": map[string]any{"preset": "basic"},
			"sinks":   map[string]any{},
		},
	}}
}

func metricNamed(name string) map[string]any {
	return map[string]any{"name": name, "sqls": map[string]any{"14": "SELECT 1"}}
}

func metricsNamed(n int) []any {
	out := make([]any, 0, n)
	for i := range n {
		out = append(out, metricNamed(fmt.Sprintf("metric_%03d", i)))
	}
	return out
}

func libraryOf(ns, name string, metrics []any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": pgpilotv1.GroupVersion.String(),
		"kind":       "PgpilotMetricLibrary",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec":       map[string]any{"metrics": metrics},
	}}
}

// stringMap returns n entries key_000..key_NNN.
func stringMap(n int) map[string]any {
	out := make(map[string]any, n)
	for i := range n {
		out[fmt.Sprintf("key_%03d", i)] = "x"
	}
	return out
}

func withSpec(obj *unstructured.Unstructured, value any, path ...string) *unstructured.Unstructured {
	GinkgoHelper()
	full := append([]string{"spec"}, path...)
	Expect(unstructured.SetNestedField(obj.Object, value, full...)).To(Succeed())
	return obj
}

func expectInvalid(obj *unstructured.Unstructured, fragments ...string) {
	GinkgoHelper()
	err := k8sClient.Create(context.TODO(), obj)
	Expect(err).To(HaveOccurred(), "the API server must reject this object")
	Expect(apierrors.IsInvalid(err)).To(BeTrue(), "want an Invalid error, got: %v", err)
	for _, f := range fragments {
		Expect(err.Error()).To(ContainSubstring(f))
	}
}

// The limits keep the cost of the validation rules bounded; each is checked
// at its edge: the largest size is accepted, one more is not.
var _ = Describe("CRD validation size limits", func() {
	var ns string
	BeforeEach(func() { ns = testNamespace("test-limits-") })

	type limit struct {
		max   int
		path  []string
		value func(n int) any
		where string
	}

	libraryRefs := func(n int) any {
		out := make([]any, 0, n)
		for i := range n {
			out = append(out, map[string]any{"name": fmt.Sprintf("lib-%03d", i)})
		}
		return out
	}
	sqls := func(n int) any {
		out := make(map[string]any, n)
		for i := range n {
			out[fmt.Sprintf("%d", i)] = "SELECT 1"
		}
		return []any{map[string]any{"name": "many_versions", "sqls": out}}
	}

	DescribeTable("on a PgpilotMonitor",
		func(l limit) {
			By(fmt.Sprintf("accepting %d", l.max))
			Expect(k8sClient.Create(context.TODO(),
				withSpec(minimalMonitor(ns, "at-the-limit"), l.value(l.max), l.path...), client.DryRunAll)).To(Succeed())

			By(fmt.Sprintf("rejecting %d", l.max+1))
			expectInvalid(withSpec(minimalMonitor(ns, "over-the-limit"), l.value(l.max+1), l.path...),
				l.where, "Too many")
		},
		Entry("custom metrics", limit{max: 256, path: []string{"metrics", "custom"},
			value: func(n int) any { return metricsNamed(n) }, where: "spec.metrics.custom"}),
		Entry("library references", limit{max: 32, path: []string{"metrics", "fromLibraries"},
			value: libraryRefs, where: "spec.metrics.fromLibraries"}),
		Entry("sqls of a custom metric", limit{max: 32, path: []string{"metrics", "custom"},
			value: sqls, where: "spec.metrics.custom[0].sqls"}),
		Entry("customTags", limit{max: 32, path: []string{"database", "customTags"},
			value: func(n int) any { return stringMap(n) }, where: "spec.database.customTags"}),
		Entry("pod labels", limit{max: 32, path: []string{"podMetadata", "labels"},
			value: func(n int) any { return stringMap(n) }, where: "spec.podMetadata.labels"}),
		Entry("pod annotations", limit{max: 64, path: []string{"podMetadata", "annotations"},
			value: func(n int) any { return stringMap(n) }, where: "spec.podMetadata.annotations"}),
		Entry("service labels", limit{max: 32, path: []string{"serviceMetadata", "labels"},
			value: func(n int) any { return stringMap(n) }, where: "spec.serviceMetadata.labels"}),
		Entry("service annotations", limit{max: 64, path: []string{"serviceMetadata", "annotations"},
			value: func(n int) any { return stringMap(n) }, where: "spec.serviceMetadata.annotations"}),
	)

	It("allows a library 256 metrics and no more", func() {
		Expect(k8sClient.Create(context.TODO(), libraryOf(ns, "full", metricsNamed(256)), client.DryRunAll)).To(Succeed())
		expectInvalid(libraryOf(ns, "overfull", metricsNamed(257)), "spec.metrics", "Too many")
	})

	It("allows a library metric 32 sqls entries and no more", func() {
		Expect(k8sClient.Create(context.TODO(),
			libraryOf(ns, "versions", sqls(32).([]any)), client.DryRunAll)).To(Succeed())
		expectInvalid(libraryOf(ns, "too-many-versions", sqls(33).([]any)), "spec.metrics[0].sqls", "Too many")
	})

	It("requires a library to hold at least one metric", func() {
		expectInvalid(libraryOf(ns, "empty", []any{}), "spec.metrics")
	})
})

// Defaults are part of the API: the builder and the documentation both rely
// on what the API server fills in.
var _ = Describe("CRD defaults and enumerations", func() {
	var ns string
	BeforeEach(func() { ns = testNamespace("test-defaults-") })

	stored := func(obj *unstructured.Unstructured) map[string]any {
		GinkgoHelper()
		createForSpec(obj)
		got := obj.DeepCopy()
		Expect(k8sClient.Get(context.TODO(), client.ObjectKeyFromObject(obj), got)).To(Succeed())
		spec, _, _ := unstructured.NestedMap(got.Object, "spec")
		return spec
	}
	field := func(spec map[string]any, path ...string) any {
		GinkgoHelper()
		v, found, err := unstructured.NestedFieldNoCopy(spec, path...)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue(), "spec.%v is not set", path)
		return v
	}

	It("fills in every documented default of a minimal monitor", func() {
		spec := stored(minimalMonitor(ns, "minimal"))

		Expect(field(spec, "database", "port")).To(BeEquivalentTo(5432))
		Expect(field(spec, "database", "sslmode")).To(Equal("disable"))
		Expect(field(spec, "database", "credentialsSecret", "usernameKey")).To(Equal("username"))
		Expect(field(spec, "database", "credentialsSecret", "passwordKey")).To(Equal("password"))
		Expect(field(spec, "sinks", "prometheus", "enabled")).To(BeTrue())
		Expect(field(spec, "sinks", "prometheus", "port")).To(BeEquivalentTo(9187))
	})

	// A repository or tag written by the API server stays in the object for
	// good and would keep the monitor on that pgwatch release after the
	// operator has moved to a newer one. The operator fills them in itself.
	It("stores no image repository or tag of its own", func() {
		spec := stored(withSpec(minimalMonitor(ns, "image-block"), map[string]any{}, "image"))

		image, found, err := unstructured.NestedMap(spec, "image")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(image).To(Equal(map[string]any{"pullPolicy": "IfNotPresent"}))
	})

	// `prometheus: {port: N}` is how one changes only the port; it must not
	// turn the sink off.
	DescribeTable("keeps the Prometheus sink enabled when only part of it is given",
		func(prometheus map[string]any, wantPort int) {
			spec := stored(withSpec(minimalMonitor(ns, "partial-prometheus"), prometheus, "sinks", "prometheus"))
			Expect(field(spec, "sinks", "prometheus", "enabled")).To(BeTrue())
			Expect(field(spec, "sinks", "prometheus", "port")).To(BeEquivalentTo(wantPort))
		},
		Entry("empty block", map[string]any{}, 9187),
		Entry("only a port", map[string]any{"port": int64(9200)}, 9200),
	)

	It("leaves the gRPC sink off unless it is enabled", func() {
		spec := stored(withSpec(minimalMonitor(ns, "grpc-default"),
			map[string]any{"endpoint": "receiver:9000"}, "sinks", "grpc"))
		Expect(field(spec, "sinks", "grpc", "enabled")).To(BeFalse())
	})

	DescribeTable("accepts every sslmode libpq knows",
		func(mode string) {
			spec := stored(withSpec(minimalMonitor(ns, "sslmode-"+mode), mode, "database", "sslmode"))
			Expect(field(spec, "database", "sslmode")).To(Equal(mode))
		},
		Entry("disable", "disable"), Entry("allow", "allow"), Entry("prefer", "prefer"),
		Entry("require", "require"), Entry("verify-ca", "verify-ca"), Entry("verify-full", "verify-full"),
	)

	DescribeTable("rejects values outside an enumeration or range",
		func(value any, where string, path ...string) {
			expectInvalid(withSpec(minimalMonitor(ns, "out-of-range"), value, path...), where)
		},
		Entry("unknown sslmode", "verify", "spec.database.sslmode", "database", "sslmode"),
		Entry("sslmode in upper case", "REQUIRE", "spec.database.sslmode", "database", "sslmode"),
		Entry("database port 0", int64(0), "spec.database.port", "database", "port"),
		Entry("database port 65536", int64(65536), "spec.database.port", "database", "port"),
		Entry("prometheus port 0", int64(0), "spec.sinks.prometheus.port", "sinks", "prometheus", "port"),
		Entry("prometheus port 65536", int64(65536), "spec.sinks.prometheus.port", "sinks", "prometheus", "port"),
		Entry("unknown pull policy", "Sometimes", "spec.image.pullPolicy", "image", "pullPolicy"),
		Entry("empty database name", "", "spec.database.database", "database", "database"),
		Entry("empty host", "", "spec.database.host", "database", "host"),
	)

	DescribeTable("accepts every image pull policy",
		func(policy string) {
			spec := stored(withSpec(minimalMonitor(ns, "pull-"+strings.ToLower(policy)),
				map[string]any{"pullPolicy": policy}, "image"))
			Expect(field(spec, "image", "pullPolicy")).To(Equal(policy))
		},
		Entry("Always", "Always"), Entry("IfNotPresent", "IfNotPresent"), Entry("Never", "Never"),
	)

	// Deprecated and ignored, but still part of the schema.
	It("keeps statementTimeoutSeconds non-negative", func() {
		negative := metricNamed("with_timeout")
		negative["statementTimeoutSeconds"] = int64(-1)
		expectInvalid(libraryOf(ns, "negative-timeout", []any{negative}), "statementTimeoutSeconds")
		expectInvalid(withSpec(minimalMonitor(ns, "negative-timeout"), []any{negative}, "metrics", "custom"),
			"statementTimeoutSeconds")

		zero := metricNamed("with_timeout")
		zero["statementTimeoutSeconds"] = int64(0)
		createForSpec(libraryOf(ns, "zero-timeout", []any{zero}))
	})

	DescribeTable("accepts the edges of the port ranges",
		func(port int64, path ...string) {
			Expect(k8sClient.Create(context.TODO(),
				withSpec(minimalMonitor(ns, fmt.Sprintf("port-%d", port)), port, path...), client.DryRunAll)).To(Succeed())
		},
		Entry("database port 1", int64(1), "database", "port"),
		Entry("database port 65535", int64(65535), "database", "port"),
		Entry("prometheus port 1", int64(1), "sinks", "prometheus", "port"),
		Entry("prometheus port 65535", int64(65535), "sinks", "prometheus", "port"),
	)

	DescribeTable("requires the fields that have no default",
		func(path ...string) {
			obj := minimalMonitor(ns, "missing-field")
			unstructured.RemoveNestedField(obj.Object, append([]string{"spec"}, path...)...)
			expectInvalid(obj, "Required value")
		},
		Entry("database", "database"),
		Entry("database.host", "database", "host"),
		Entry("database.database", "database", "database"),
		Entry("metrics", "metrics"),
		Entry("sinks", "sinks"),
	)

	It("requires `enabled` once a sink block carries other settings only by default", func() {
		// The field is required in the schema but has a default, so leaving
		// it out is fine; this pins that the default satisfies the schema.
		Expect(k8sClient.Create(context.TODO(), withSpec(minimalMonitor(ns, "grpc-no-enabled"),
			map[string]any{"endpoint": "receiver:9000"}, "sinks", "grpc"), client.DryRunAll)).To(Succeed())
	})

	It("requires a metric to have a name and SQL", func() {
		expectInvalid(libraryOf(ns, "nameless", []any{map[string]any{"sqls": map[string]any{"14": "SELECT 1"}}}),
			"Required value")
		expectInvalid(libraryOf(ns, "sqlless", []any{map[string]any{"name": "no_sql"}}), "Required value")
	})
})

// `kubectl get` shows what the printer columns of the CRD select. A column
// that points at the wrong field is simply empty, which no other test sees.
var _ = Describe("CRD printer columns", func() {
	var ns string
	BeforeEach(func() { ns = testNamespace("test-columns-") })

	// table asks the API server for the representation kubectl prints.
	table := func(resource string, wide bool) *metav1.Table {
		GinkgoHelper()
		transport, err := rest.TransportFor(cfg)
		Expect(err).NotTo(HaveOccurred())

		base, _, err := rest.DefaultServerUrlFor(cfg)
		Expect(err).NotTo(HaveOccurred())
		url := fmt.Sprintf("%s/apis/%s/namespaces/%s/%s",
			strings.TrimRight(base.String(), "/"), pgpilotv1.GroupVersion.String(), ns, resource)
		req, err := http.NewRequestWithContext(context.TODO(), http.MethodGet, url, nil)
		Expect(err).NotTo(HaveOccurred())
		req.Header.Set("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io")

		resp, err := (&http.Client{Transport: transport}).Do(req)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(http.StatusOK), string(body))

		var out metav1.Table
		Expect(json.Unmarshal(body, &out)).To(Succeed())
		if !wide {
			// Without -o wide kubectl hides the columns with a priority.
			var names []metav1.TableColumnDefinition
			keep := make([]int, 0, len(out.ColumnDefinitions))
			for i, c := range out.ColumnDefinitions {
				if c.Priority == 0 {
					names = append(names, c)
					keep = append(keep, i)
				}
			}
			for r := range out.Rows {
				cells := make([]any, 0, len(keep))
				for _, i := range keep {
					cells = append(cells, out.Rows[r].Cells[i])
				}
				out.Rows[r].Cells = cells
			}
			out.ColumnDefinitions = names
		}
		return &out
	}
	columns := func(t *metav1.Table) []string {
		names := make([]string, 0, len(t.ColumnDefinitions))
		for _, c := range t.ColumnDefinitions {
			names = append(names, c.Name)
		}
		return names
	}
	cell := func(t *metav1.Table, column string) any {
		GinkgoHelper()
		Expect(t.Rows).To(HaveLen(1))
		for i, c := range t.ColumnDefinitions {
			if c.Name == column {
				return t.Rows[0].Cells[i]
			}
		}
		Fail("no column " + column)
		return nil
	}

	// The column used to select `.spec.metrics[*].name` as an integer and
	// was always empty.
	It("shows the number of metrics of a library", func() {
		createForSpec(libraryOf(ns, "three", metricsNamed(3)))

		Eventually(func(g Gomega) {
			t := table("pgpilotmetriclibraries", false)
			g.Expect(columns(t)).To(Equal([]string{"Name", "Metrics", "Age"}))
			g.Expect(cell(t, "Metrics")).To(BeEquivalentTo(3))
		}, timeout, interval).Should(Succeed())
	})

	// With `omitempty` on status.ready the cell was empty for every monitor
	// that was not ready.
	It("shows READY false for a monitor that is not ready", func() {
		createForSpec(minimalMonitor(ns, "waiting"))

		Eventually(func(g Gomega) {
			t := table("pgpilotmonitors", false)
			g.Expect(columns(t)).To(Equal([]string{"Name", "Ready", "Age"}))
			// Until the controller has written status the cell is empty.
			ready, written := cell(t, "Ready").(bool)
			g.Expect(written).To(BeTrue(), "the READY cell is still empty")
			g.Expect(ready).To(BeFalse())
		}, timeout, interval).Should(Succeed())
	})

	It("adds pod, host and database with -o wide", func() {
		createForSpec(minimalMonitor(ns, "wide"))

		Eventually(func(g Gomega) {
			t := table("pgpilotmonitors", true)
			g.Expect(columns(t)).To(Equal([]string{"Name", "Ready", "Pod", "Host", "DB", "Age"}))
			g.Expect(cell(t, "Host")).To(Equal("pg.example.com"))
			g.Expect(cell(t, "DB")).To(Equal("appdb"))
		}, timeout, interval).Should(Succeed())
	})
})
