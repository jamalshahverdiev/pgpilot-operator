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
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

// mutation changes the spec of an otherwise valid object.
type mutation func(spec map[string]any)

// These specs prove, through the real API server, which objects the CRD
// schemas admit. Every rule has cases on both sides of its boundary.
var _ = Describe("CRD validation rules", func() {
	var ns string

	BeforeEach(func() {
		created := &corev1.Namespace{}
		created.GenerateName = "test-rules-"
		Expect(k8sClient.Create(context.TODO(), created)).To(Succeed())
		ns = created.Name
	})

	// validMetric is a metric definition every rule accepts.
	validMetric := func(name string) map[string]any {
		return map[string]any{
			"name": name,
			"sqls": map[string]any{"14": "SELECT 1 AS value"},
		}
	}

	// monitor returns a valid PgpilotMonitor with the mutations applied.
	monitor := func(name string, mutations ...mutation) *unstructured.Unstructured {
		spec := map[string]any{
			"database": map[string]any{
				"host":              "pg.example.com",
				"database":          "appdb",
				"credentialsSecret": map[string]any{"name": "pg-creds"},
			},
			"metrics": map[string]any{"preset": "basic"},
			"sinks":   map[string]any{"prometheus": map[string]any{"enabled": true}},
		}
		for _, m := range mutations {
			m(spec)
		}
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": pgpilotv1.GroupVersion.String(),
			"kind":       "PgpilotMonitor",
			"metadata":   map[string]any{"name": name, "namespace": ns},
			"spec":       spec,
		}}
	}

	// library returns a PgpilotMetricLibrary holding the given metrics.
	library := func(name string, metrics ...map[string]any) *unstructured.Unstructured {
		items := make([]any, 0, len(metrics))
		for _, m := range metrics {
			items = append(items, m)
		}
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": pgpilotv1.GroupVersion.String(),
			"kind":       "PgpilotMetricLibrary",
			"metadata":   map[string]any{"name": name, "namespace": ns},
			"spec":       map[string]any{"metrics": items},
		}}
	}

	set := func(value any, path ...string) mutation {
		return func(spec map[string]any) {
			Expect(unstructured.SetNestedField(spec, value, path...)).To(Succeed())
		}
	}
	remove := func(path ...string) mutation {
		return func(spec map[string]any) { unstructured.RemoveNestedField(spec, path...) }
	}

	// expectAccepted asks the API server to validate the object without
	// storing it: hundreds of stored monitors would keep the manager of the
	// suite busy for nothing.
	expectAccepted := func(obj *unstructured.Unstructured) {
		GinkgoHelper()
		Expect(k8sClient.Create(context.TODO(), obj, client.DryRunAll)).To(Succeed())
	}
	// expectRejected asserts the API server refuses the object and that its
	// answer mentions every given fragment, so the user can tell what to fix.
	expectRejected := func(obj *unstructured.Unstructured, fragments ...string) {
		GinkgoHelper()
		err := k8sClient.Create(context.TODO(), obj)
		Expect(err).To(HaveOccurred(), "the API server must reject this object")
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "want an Invalid error, got: %v", err)
		for _, f := range fragments {
			Expect(err.Error()).To(ContainSubstring(f))
		}
	}

	// stored creates the object for real and returns what the API server
	// kept, defaults included.
	stored := func(obj *unstructured.Unstructured) *unstructured.Unstructured {
		GinkgoHelper()
		createForSpec(obj)
		out := obj.DeepCopy()
		Expect(k8sClient.Get(context.TODO(), client.ObjectKeyFromObject(obj), out)).To(Succeed())
		return out
	}

	// ------------------------------------------------------------------
	Describe("sinks", func() {
		const noSink = "at least one sink must be enabled"

		// pgwatch exits with "no sinks specified for measurements" when it
		// is started without one, so such a monitor can only crash-loop.
		DescribeTable("rejects a monitor without an enabled sink",
			func(sinks map[string]any) {
				expectRejected(monitor("no-sink", set(sinks, "sinks")), noSink)
			},
			Entry("prometheus disabled, no grpc", map[string]any{
				"prometheus": map[string]any{"enabled": false},
			}),
			Entry("both disabled", map[string]any{
				"prometheus": map[string]any{"enabled": false},
				"grpc":       map[string]any{"enabled": false, "endpoint": "receiver:9000"},
			}),
		)

		DescribeTable("rejects an enabled gRPC sink without a usable endpoint",
			func(grpc map[string]any, fragment string) {
				expectRejected(monitor("grpc-endpoint", set(grpc, "sinks", "grpc")), fragment)
			},
			Entry("no endpoint", map[string]any{"enabled": true},
				"endpoint is required when the gRPC sink is enabled"),
			Entry("empty endpoint", map[string]any{"enabled": true, "endpoint": ""},
				"endpoint is required when the gRPC sink is enabled"),
			Entry("endpoint with whitespace", map[string]any{"enabled": true, "endpoint": "receiver 9000"},
				"spec.sinks.grpc.endpoint"),
		)

		It("limits the gRPC endpoint to 512 characters", func() {
			long := "receiver." + strings.Repeat("a", 500) + ":9000"
			Expect(long).To(HaveLen(514))
			expectRejected(monitor("grpc-long", set(map[string]any{
				"enabled": true, "endpoint": long,
			}, "sinks", "grpc")), "spec.sinks.grpc.endpoint")
			expectAccepted(monitor("grpc-max", set(map[string]any{
				"enabled": true, "endpoint": long[:512],
			}, "sinks", "grpc")))
		})

		It("accepts a disabled gRPC sink without an endpoint", func() {
			expectAccepted(monitor("grpc-off", set(map[string]any{"enabled": false}, "sinks", "grpc")))
		})

		// Templated manifests render the unused field as an empty string.
		It("accepts a disabled gRPC sink with an empty endpoint", func() {
			expectAccepted(monitor("grpc-off-empty",
				set(map[string]any{"enabled": false, "endpoint": ""}, "sinks", "grpc")))
		})

		It("accepts the gRPC sink on its own", func() {
			expectAccepted(monitor("grpc-only", set(map[string]any{
				"prometheus": map[string]any{"enabled": false},
				"grpc":       map[string]any{"enabled": true, "endpoint": "receiver.pgpilot:9000"},
			}, "sinks")))
		})

		It("accepts both sinks together", func() {
			expectAccepted(monitor("both-sinks", set(map[string]any{
				"enabled": true, "endpoint": "receiver.pgpilot:9000",
			}, "sinks", "grpc")))
		})

		// The documentation promises Prometheus by default; before the
		// default existed `sinks: {}` produced a pod without any sink.
		DescribeTable("enables the Prometheus sink when it is left out",
			func(sinks map[string]any) {
				got := stored(monitor("prom-default", set(sinks, "sinks")))
				enabled, found, err := unstructured.NestedBool(got.Object, "spec", "sinks", "prometheus", "enabled")
				Expect(err).NotTo(HaveOccurred())
				Expect(found).To(BeTrue(), "spec.sinks.prometheus.enabled must be defaulted")
				Expect(enabled).To(BeTrue())
				port, found, err := unstructured.NestedInt64(got.Object, "spec", "sinks", "prometheus", "port")
				Expect(err).NotTo(HaveOccurred())
				Expect(found).To(BeTrue(), "spec.sinks.prometheus.port must be defaulted")
				Expect(port).To(BeEquivalentTo(9187))
			},
			Entry("empty sinks", map[string]any{}),
			Entry("only a gRPC sink given", map[string]any{
				"grpc": map[string]any{"enabled": true, "endpoint": "receiver:9000"},
			}),
		)
	})

	// ------------------------------------------------------------------
	Describe("metrics selection", func() {
		const noMetrics = "at least one of preset, fromLibraries or custom must be set"

		DescribeTable("rejects a monitor that selects no metrics",
			func(metrics map[string]any) {
				expectRejected(monitor("no-metrics", set(metrics, "metrics")), noMetrics)
			},
			Entry("empty object", map[string]any{}),
			Entry("empty lists", map[string]any{"fromLibraries": []any{}, "custom": []any{}}),
		)

		DescribeTable("accepts each way of selecting metrics on its own",
			func(metrics map[string]any) {
				expectAccepted(monitor("some-metrics", set(metrics, "metrics")))
			},
			Entry("a preset", map[string]any{"preset": "exhaustive_no_python"}),
			Entry("a library", map[string]any{"fromLibraries": []any{map[string]any{"name": "shared"}}}),
			Entry("a custom metric", map[string]any{"custom": []any{map[string]any{
				"name": "one", "sqls": map[string]any{"14": "SELECT 1"},
			}}}),
		)

		DescribeTable("rejects preset names pgwatch cannot have",
			func(preset string) {
				expectRejected(monitor("unusable", set(preset, "metrics", "preset")), "spec.metrics.preset")
			},
			Entry("upper case", "Basic"),
			Entry("with a space", "basic preset"),
			Entry("with a dash", "exhaustive-no-python"),
			Entry("starting with a digit", "1basic"),
			Entry("longer than 63 characters", strings.Repeat("a", 64)),
		)

		// "Leave empty to rely entirely on libraries and custom metrics."
		It("accepts an empty preset next to other metrics", func() {
			expectAccepted(monitor("empty-preset", set(map[string]any{
				"preset": "", "custom": []any{validMetric("one")},
			}, "metrics")))
			expectAccepted(monitor("empty-preset-lib", set(map[string]any{
				"preset": "", "fromLibraries": []any{map[string]any{"name": "shared"}},
			}, "metrics")))
		})

		It("rejects an empty preset when nothing else is selected", func() {
			expectRejected(monitor("only-empty-preset", set(map[string]any{"preset": ""}, "metrics")), noMetrics)
		})

		It("limits a library reference to a valid object name length", func() {
			expectRejected(monitor("long-lib-ref", set(map[string]any{"fromLibraries": []any{
				map[string]any{"name": strings.Repeat("l", 254)},
			}}, "metrics")), "spec.metrics.fromLibraries[0].name")
			expectRejected(monitor("empty-lib-ref", set(map[string]any{"fromLibraries": []any{
				map[string]any{"name": ""},
			}}, "metrics")), "spec.metrics.fromLibraries[0].name")
			expectAccepted(monitor("max-lib-ref", set(map[string]any{"fromLibraries": []any{
				map[string]any{"name": strings.Repeat("l", 253)},
			}}, "metrics")))
		})

		It("rejects the same library listed twice", func() {
			expectRejected(monitor("dup-lib", set(map[string]any{"fromLibraries": []any{
				map[string]any{"name": "shared"}, map[string]any{"name": "shared"},
			}}, "metrics")), "a library can be listed only once")
		})

		// Duplicates used to be accepted and silently collapsed: the last
		// definition won and the others were ignored.
		It("rejects two custom metrics with the same name", func() {
			expectRejected(monitor("dup-custom", set(map[string]any{"custom": []any{
				validMetric("same"), validMetric("same"),
			}}, "metrics")), "metric names must be unique")
		})

		It("rejects two library metrics with the same name", func() {
			expectRejected(library("dup-metric", validMetric("same"), validMetric("same")), "metric names must be unique")
			expectRejected(library("dup-far-apart",
				validMetric("same"), validMetric("other"), validMetric("third"), validMetric("same")),
				"metric names must be unique")
		})

		// The operator applies the rule itself to lists that were stored
		// before the API server did; see builder.ValidateMetricDefinitions.
		It("is refused by the operator's own check as well", func() {
			same := pgpilotv1.MetricDefinition{Name: "same", SQLs: map[string]string{"14": "SELECT 1 AS value"}}
			other := pgpilotv1.MetricDefinition{Name: "other", SQLs: map[string]string{"14": "SELECT 1 AS value"}}
			Expect(builder.ValidateMetricDefinitions([]pgpilotv1.MetricDefinition{same, other, same})).
				To(MatchError(builder.ErrInvalidMetric))
			Expect(builder.ValidateMetricDefinitions([]pgpilotv1.MetricDefinition{same, other})).To(Succeed())
		})

		It("accepts distinct metric names", func() {
			expectAccepted(library("distinct", validMetric("one"), validMetric("two")))
			expectAccepted(monitor("distinct", set(map[string]any{"custom": []any{
				validMetric("one"), validMetric("two"),
			}}, "metrics")))
		})
	})

	// ------------------------------------------------------------------
	// A MetricDefinition appears in libraries and inline in monitors; every
	// rule on it is checked in both places.
	Describe("metric definition", func() {
		withMetric := func(name string, metric map[string]any) []*unstructured.Unstructured {
			return []*unstructured.Unstructured{
				library(name, metric),
				monitor(name, set(map[string]any{"custom": []any{metric}}, "metrics")),
			}
		}
		metricWith := func(key string, value any) map[string]any {
			m := validMetric("probe_metric")
			m[key] = value
			return m
		}

		// The API server applies these rules to what is written. The operator
		// applies them itself to definitions that were stored before a rule
		// existed (builder.ValidateMetricDefinitions). The two must not come
		// apart: the operator would render what the API server calls invalid,
		// or refuse a monitor for a definition that can be applied as it is.
		DescribeTable("the operator's own check of a definition agrees with the API server",
			func(metric map[string]any, valid bool) {
				raw, err := json.Marshal(metric)
				Expect(err).NotTo(HaveOccurred())
				var typed pgpilotv1.MetricDefinition
				Expect(json.Unmarshal(raw, &typed)).To(Succeed())
				verdict := builder.ValidateMetricDefinitions([]pgpilotv1.MetricDefinition{typed})

				for _, obj := range withMetric("parity", metric) {
					By(obj.GetKind())
					err := k8sClient.Create(context.TODO(), obj, client.DryRunAll)
					if valid {
						Expect(err).NotTo(HaveOccurred(), "the API server must accept it")
					} else {
						Expect(apierrors.IsInvalid(err)).To(BeTrue(), "the API server must refuse it, got: %v", err)
					}
				}
				if valid {
					Expect(verdict).To(Succeed(), "the operator refuses what the API server accepts")
				} else {
					Expect(verdict).To(MatchError(builder.ErrInvalidMetric), "the operator accepts what the API server refuses")
				}
			},
			Entry("a plain definition", validMetric("orders"), true),
			Entry("sqls key: two zeros", metricWith("sqls", map[string]any{"00": "SELECT 1"}), false),
			Entry("sqls key: three zeros", metricWith("sqls", map[string]any{"000": "SELECT 1"}), false),
			Entry("sqls key: a digit that is not ASCII", metricWith("sqls", map[string]any{"1\u0664": "SELECT 1"}), false),
			Entry("sqls: a statement of white space", metricWith("sqls", map[string]any{"14": " \n\t"}), true),
			Entry("name: trailing newline", validMetric("orders\n"), false),
			Entry("name: a second line", validMetric("orders\nqueue"), false),
			Entry("name: upper case in the tail only", validMetric("ordersQueue"), false),
			Entry("name: upper case at the head only", validMetric("Orders"), false),
			Entry("name: a letter that is not ASCII", validMetric("ord\u00e9rs"), false),
			Entry("interval: 1000ms", metricWith("interval", "1000ms"), true),
			Entry("interval: 1.5s", metricWith("interval", "1.5s"), true),
			Entry("interval: 0.5m", metricWith("interval", "0.5m"), true),
			Entry("interval: 999999us", metricWith("interval", "999999us"), false),
			Entry("isInstanceLevel", metricWith("isInstanceLevel", true), true),
			Entry("both roles named and false", map[string]any{
				"name": "probe_metric", "sqls": map[string]any{"14": "SELECT 1"}, "masterOnly": false, "standbyOnly": false,
			}, true),
			Entry("name: underscores and digits", validMetric("orders_queue_depth_2"), true),
			Entry("name: leading underscore", validMetric("_internal"), true),
			Entry("name: 63 characters", validMetric(strings.Repeat("m", 63)), true),
			Entry("name: 64 characters", validMetric(strings.Repeat("m", 64)), false),
			Entry("name: upper case", validMetric("OrdersQueue"), false),
			Entry("name: dash", validMetric("orders-queue"), false),
			Entry("name: leading digit", validMetric("1orders"), false),
			Entry("name: dot", validMetric("orders.queue"), false),
			Entry("name: space", validMetric("orders queue"), false),
			Entry("name: empty", validMetric(""), false),
			Entry("sqls key: zero, the catch-all", metricWith("sqls", map[string]any{"0": "SELECT 1"}), true),
			Entry("sqls key: single digit", metricWith("sqls", map[string]any{"9": "SELECT 1"}), true),
			Entry("sqls key: three digits", metricWith("sqls", map[string]any{"100": "SELECT 1"}), true),
			Entry("sqls key: largest", metricWith("sqls", map[string]any{"999": "SELECT 1"}), true),
			Entry("sqls: several versions",
				metricWith("sqls", map[string]any{"0": "SELECT 1", "14": "SELECT 2", "17": "SELECT 3"}), true),
			Entry("sqls key: wildcard", metricWith("sqls", map[string]any{"*": "SELECT 1"}), false),
			Entry("sqls key: wildcard next to a version", metricWith("sqls", map[string]any{"14": "SELECT 1", "*": "SELECT 1"}), false),
			Entry("sqls key: minor version", metricWith("sqls", map[string]any{"14.2": "SELECT 1"}), false),
			Entry("sqls key: v prefix", metricWith("sqls", map[string]any{"v14": "SELECT 1"}), false),
			Entry("sqls key: plus sign", metricWith("sqls", map[string]any{"+14": "SELECT 1"}), false),
			Entry("sqls key: leading zero", metricWith("sqls", map[string]any{"014": "SELECT 1"}), false),
			Entry("sqls key: negative", metricWith("sqls", map[string]any{"-1": "SELECT 1"}), false),
			Entry("sqls key: leading space", metricWith("sqls", map[string]any{" 14": "SELECT 1"}), false),
			Entry("sqls key: trailing newline", metricWith("sqls", map[string]any{"14\n": "SELECT 1"}), false),
			Entry("sqls key: empty", metricWith("sqls", map[string]any{"": "SELECT 1"}), false),
			Entry("sqls key: word", metricWith("sqls", map[string]any{"latest": "SELECT 1"}), false),
			Entry("sqls key: four digits", metricWith("sqls", map[string]any{"1000": "SELECT 1"}), false),
			Entry("sqls key: server_version_num form", metricWith("sqls", map[string]any{"170000": "SELECT 1"}), false),
			Entry("sqls: an empty statement", metricWith("sqls", map[string]any{"14": ""}), false),
			Entry("sqls: an empty statement next to a good one", metricWith("sqls", map[string]any{"14": "SELECT 1", "15": ""}), false),
			Entry("sqls: no statement at all", metricWith("sqls", map[string]any{}), false),
			Entry("primary only", metricWith("masterOnly", true), true),
			Entry("standby only", metricWith("standbyOnly", true), true),
			Entry("both roles", map[string]any{
				"name": "probe_metric", "sqls": map[string]any{"14": "SELECT 1"}, "masterOnly": true, "standbyOnly": true,
			}, false),
			Entry("both roles named, one of them off", map[string]any{
				"name": "probe_metric", "sqls": map[string]any{"14": "SELECT 1"}, "masterOnly": true, "standbyOnly": false,
			}, true),
			Entry("interval: one second", metricWith("interval", "1s"), true),
			Entry("interval: a week", metricWith("interval", "168h"), true),
			Entry("interval: just below one second", metricWith("interval", "999ms"), false),
			Entry("interval: zero", metricWith("interval", "0s"), false),
			Entry("interval: negative", metricWith("interval", "-30s"), false),
			Entry("the ignored statementTimeoutSeconds", metricWith("statementTimeoutSeconds", int64(5)), true),
			Entry("gauges and a description", map[string]any{
				"name": "probe_metric", "sqls": map[string]any{"14": "SELECT 1"},
				"gauges": []any{"*"}, "description": "Anything at all: 14.2, *, OrdersQueue",
			}, true),
		)

		// The name becomes a key in pgwatch's metrics.yaml and the prefix of
		// every Prometheus series of the metric.
		DescribeTable("rejects metric names that are not lower-case identifiers",
			func(name string) {
				for _, obj := range withMetric("unusable", validMetric(name)) {
					By(obj.GetKind())
					expectRejected(obj, "].name")
				}
			},
			Entry("upper case", "OrdersQueue"),
			Entry("dash", "orders-queue"),
			Entry("leading digit", "1orders"),
			Entry("dot", "orders.queue"),
			Entry("space", "orders queue"),
			Entry("empty", ""),
			Entry("longer than 63 characters", strings.Repeat("m", 64)),
		)

		DescribeTable("accepts lower-case identifier metric names",
			func(name string) {
				for _, obj := range withMetric("good-metric-name", validMetric(name)) {
					By(obj.GetKind())
					expectAccepted(obj)
				}
			},
			Entry("plain", "orders"),
			Entry("with underscores and digits", "orders_queue_depth_2"),
			Entry("leading underscore", "_internal"),
			Entry("63 characters", strings.Repeat("m", 63)),
		)

		// "*", "14.2" and "v14" used to be dropped without a word: the metric
		// was scheduled with no SQL and never produced data. "014" and "+14"
		// were read as 14; they now have to be written "14". A
		// server_version_num such as 170000 never matches a server: pgwatch
		// compares keys with the major version.
		DescribeTable("rejects sqls keys that are not plain version numbers",
			func(key string) {
				for _, obj := range withMetric("bad-sqls-key", metricWith("sqls", map[string]any{key: "SELECT 1"})) {
					By(obj.GetKind())
					expectRejected(obj, "sqls keys must be PostgreSQL major versions")
				}
			},
			Entry("wildcard", "*"),
			Entry("minor version", "14.2"),
			Entry("v prefix", "v14"),
			Entry("plus sign", "+14"),
			Entry("leading zero", "014"),
			Entry("negative", "-1"),
			Entry("leading space", " 14"),
			Entry("empty", ""),
			Entry("word", "latest"),
			Entry("four digits", "1000"),
			Entry("server_version_num form", "170000"),
		)

		DescribeTable("accepts integer sqls keys",
			func(key string) {
				for _, obj := range withMetric("good-sqls-key", metricWith("sqls", map[string]any{key: "SELECT 1"})) {
					By(obj.GetKind())
					expectAccepted(obj)
				}
			},
			Entry("zero, the catch-all", "0"),
			Entry("single digit", "9"),
			Entry("two digits", "14"),
			Entry("three digits", "100"),
			Entry("largest key", "999"),
		)

		It("accepts several versions for one metric", func() {
			for _, obj := range withMetric("multi-version", metricWith("sqls", map[string]any{
				"0": "SELECT 1", "14": "SELECT 2", "17": "SELECT 3",
			})) {
				expectAccepted(obj)
			}
		})

		It("rejects an empty SQL statement", func() {
			for _, obj := range withMetric("empty-sql", metricWith("sqls", map[string]any{"14": ""})) {
				By(obj.GetKind())
				expectRejected(obj, "sqls statements must not be empty")
			}
		})

		It("rejects a metric without any SQL", func() {
			for _, obj := range withMetric("no-sql", metricWith("sqls", map[string]any{})) {
				By(obj.GetKind())
				expectRejected(obj, "sqls")
			}
		})

		// Both set used to mean "primary only", silently.
		It("rejects masterOnly together with standbyOnly", func() {
			m := metricWith("masterOnly", true)
			m["standbyOnly"] = true
			for _, obj := range withMetric("both-roles", m) {
				By(obj.GetKind())
				expectRejected(obj, "masterOnly and standbyOnly are mutually exclusive")
			}
		})

		DescribeTable("accepts a single role restriction",
			func(masterOnly, standbyOnly bool) {
				m := metricWith("masterOnly", masterOnly)
				m["standbyOnly"] = standbyOnly
				for _, obj := range withMetric("one-role", m) {
					By(obj.GetKind())
					expectAccepted(obj)
				}
			},
			Entry("primary only", true, false),
			Entry("standby only", false, true),
			Entry("neither", false, false),
		)

		// The field is deprecated and ignored, but existing manifests that
		// set it must keep applying.
		It("still accepts statementTimeoutSeconds", func() {
			for _, obj := range withMetric("with-timeout", metricWith("statementTimeoutSeconds", int64(5))) {
				By(obj.GetKind())
				expectAccepted(obj)
			}
		})
	})

	// ------------------------------------------------------------------
	Describe("database", func() {
		// host and database are put into a postgresql:// URL.
		DescribeTable("rejects hosts with URL metacharacters",
			func(host string) {
				expectRejected(monitor("unusable", set(host, "database", "host")), "spec.database.host")
			},
			Entry("space", "db host"),
			Entry("slash", "db/host"),
			Entry("query", "host?sslmode=disable"),
			Entry("userinfo", "user@host"),
			Entry("fragment", "host#frag"),
			Entry("percent escape", "host%2f"),
			Entry("empty", ""),
			Entry("longer than 253 characters", strings.Repeat("a", 254)),
			// The port has a field of its own; in host it would be read as
			// part of an IPv6 address.
			Entry("port typed into the host", "pg.example.com:6432"),
			Entry("bracketed host name", "[pg.example.com]"),
			Entry("IPv6 zone id", "fe80::1%eth0"),
		)

		DescribeTable("accepts host names and addresses",
			func(host string) {
				expectAccepted(monitor("good-host", set(host, "database", "host")))
			},
			Entry("DNS name", "db.prod.example.com"),
			Entry("service name", "postgres"),
			Entry("with underscore", "my_db-1.internal"),
			Entry("IPv4", "10.0.0.5"),
			Entry("fully qualified with trailing dot", "db.prod.example.com."),
			Entry("IPv6", "fd00::10"),
			Entry("bracketed IPv6", "[fd00::10]"),
			Entry("full IPv6", "2001:db8:0:0:0:0:0:1"),
			Entry("IPv6 loopback", "::1"),
			Entry("IPv4-mapped IPv6", "::ffff:10.0.0.5"),
		)

		It("limits the database name to PostgreSQL's 63 bytes", func() {
			expectRejected(monitor("long-name", set(strings.Repeat("d", 64), "database", "database")),
				"spec.database.database")
			expectAccepted(monitor("max-db", set(strings.Repeat("d", 63), "database", "database")))
		})

		// These three cannot work: with an empty or "__" key pgwatch fails to
		// export every series of the database, and dbname is overwritten
		// with the monitor name.
		DescribeTable("rejects customTags keys that cannot work",
			func(key string) {
				expectRejected(monitor("bad-tag", set(map[string]any{key: "x"}, "database", "customTags")),
					"customTags keys must not be empty, must not start with __ and must not be dbname")
			},
			Entry("empty", ""),
			Entry("reserved prefix", "__name__"),
			Entry("two underscores only", "__"),
			Entry("the label pgwatch sets itself", "dbname"),
		)

		// Anything else is exported; on the Prometheus sink characters
		// outside [a-zA-Z0-9_] become "_". Such keys work today and must
		// keep working.
		It("accepts every other customTags key", func() {
			expectAccepted(monitor("good-tags", set(map[string]any{
				"env": "prod", "Team_1": "data platform", "_x": "", "instance": "primary",
				"team-name": "data", "team.name": "data", "1abc": "x", "my tag": "x",
				"app.kubernetes.io/team": "x", "DBNAME": "x", "db_name": "x", "_": "x",
			}, "database", "customTags")))
		})

		DescribeTable("rejects Secret keys that cannot exist",
			func(field string) {
				expectRejected(monitor("unusable", set("bad key", "database", "credentialsSecret", field)),
					"spec.database.credentialsSecret."+field)
			},
			Entry("usernameKey", "usernameKey"),
			Entry("passwordKey", "passwordKey"),
		)

		DescribeTable("limits Secret keys to 253 characters",
			func(field string) {
				expectRejected(monitor("too-long",
					set(strings.Repeat("k", 254), "database", "credentialsSecret", field)),
					"spec.database.credentialsSecret."+field)
				expectAccepted(monitor("max-secret-key",
					set(strings.Repeat("k", 253), "database", "credentialsSecret", field)))
			},
			Entry("usernameKey", "usernameKey"),
			Entry("passwordKey", "passwordKey"),
		)

		// An explicitly empty key means "use the default name", like a
		// missing one; the builder has always treated it that way.
		It("accepts empty Secret keys", func() {
			expectAccepted(monitor("empty-keys",
				set("", "database", "credentialsSecret", "usernameKey"),
				set("", "database", "credentialsSecret", "passwordKey")))
		})

		It("accepts Secret keys and defaults them", func() {
			got := stored(monitor("secret-keys", set("db.user-name_1", "database", "credentialsSecret", "usernameKey")))
			user, _, _ := unstructured.NestedString(got.Object, "spec", "database", "credentialsSecret", "usernameKey")
			pass, _, _ := unstructured.NestedString(got.Object, "spec", "database", "credentialsSecret", "passwordKey")
			Expect(user).To(Equal("db.user-name_1"))
			Expect(pass).To(Equal("password"), "passwordKey must be defaulted")
		})

		// The Secrets that certificates are mounted from are named by the
		// rules of the credentials Secret: what could not be the name of a
		// Secret, or of a key in one, is refused when it is written.
		verified := set("verify-full", "database", "sslmode")

		DescribeTable("rejects a certificate Secret name that is not an object name",
			func(path ...string) {
				for _, name := range []string{"other-namespace/victim", "..", "Upper", "a_b", "", strings.Repeat("a", 254)} {
					expectRejected(monitor("unusable", verified, set(map[string]any{"name": name}, path...)),
						"spec."+strings.Join(path, ".")+".name")
				}
				// Without a name there is nothing to mount.
				expectRejected(monitor("unusable", verified, set(map[string]any{}, path...)), "spec."+strings.Join(path, ".")+".name")
			},
			Entry("the database's authority", "database", "tls", "caSecretRef"),
			Entry("the client certificate", "database", "tls", "clientCertSecretRef"),
		)

		DescribeTable("rejects certificate keys that cannot exist, and limits them to 253 characters",
			func(field string, path ...string) {
				at := "spec." + strings.Join(path, ".") + "." + field
				expectRejected(monitor("unusable", verified, set(map[string]any{"name": "certs", field: "bad key"}, path...)), at)
				expectRejected(monitor("too-long", verified, set(map[string]any{"name": "certs", field: strings.Repeat("k", 254)}, path...)), at)
				expectAccepted(monitor("max-certificate-key", verified,
					set(map[string]any{"name": "certs", field: strings.Repeat("k", 253)}, path...)))
			},
			Entry("key of the authority", "key", "database", "tls", "caSecretRef"),
			Entry("certKey of the client certificate", "certKey", "database", "tls", "clientCertSecretRef"),
			Entry("keyKey of the client certificate", "keyKey", "database", "tls", "clientCertSecretRef"),
		)

		It("accepts certificates and defaults their keys", func() {
			got := stored(monitor("with-certificates", verified, set(map[string]any{
				"caSecretRef":         map[string]any{"name": "pg-ca"},
				"clientCertSecretRef": map[string]any{"name": "pg-client", "certKey": "client.pem"},
			}, "database", "tls")))
			for path, want := range map[string]string{
				"caSecretRef.key": "ca.crt", "clientCertSecretRef.certKey": "client.pem", "clientCertSecretRef.keyKey": "tls.key",
			} {
				value, _, _ := unstructured.NestedString(got.Object,
					append([]string{"spec", "database", "tls"}, strings.Split(path, ".")...)...)
				Expect(value).To(Equal(want), path)
			}
		})

		// An empty block is a monitor without certificates, as one that has
		// no block at all.
		It("accepts a tls block with nothing in it", func() {
			expectAccepted(monitor("empty-tls", verified, set(map[string]any{}, "database", "tls")))
		})

		// sslmode is "disable" unless it is set, and with it, as with allow
		// and prefer, nothing checks the server against an authority. A
		// monitor that names one and is left on such a mode would look
		// protected and not be.
		DescribeTable("refuses certificates with an sslmode that leaves them unused",
			func(sslmode string, wantCA, wantClient bool) {
				mode := func(map[string]any) {}
				if sslmode != "" {
					mode = set(sslmode, "database", "sslmode")
				}
				ca := monitor("ca-"+sslmode, mode, set(map[string]any{"name": "pg-ca"}, "database", "tls", "caSecretRef"))
				client := monitor("client-"+sslmode, mode,
					set(map[string]any{"name": "pg-client"}, "database", "tls", "clientCertSecretRef"))
				if wantCA {
					expectAccepted(ca)
				} else {
					expectRejected(ca, "tls.caSecretRef needs sslmode require, verify-ca or verify-full")
				}
				if wantClient {
					expectAccepted(client)
				} else {
					expectRejected(client, "tls.clientCertSecretRef needs an sslmode other than disable")
				}
			},
			Entry("the default", "", false, false),
			Entry("disable", "disable", false, false),
			Entry("allow", "allow", false, true),
			Entry("prefer", "prefer", false, true),
			Entry("require", "require", true, true),
			Entry("verify-ca", "verify-ca", true, true),
			Entry("verify-full", "verify-full", true, true),
		)

		It("refuses turning TLS off under certificates that are named", func() {
			obj := stored(monitor("tls-then-off", verified,
				set(map[string]any{"name": "pg-ca"}, "database", "tls", "caSecretRef")))
			Expect(unstructured.SetNestedField(obj.Object, "disable", "spec", "database", "sslmode")).To(Succeed())
			err := k8sClient.Update(context.TODO(), obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("tls.caSecretRef needs sslmode"))
		})

		// The field of the gRPC sink's authority was stored, and did
		// nothing, before it had a meaning. A rule added to it now would
		// refuse writes to monitors that hold a value it does not like; what
		// cannot be the name of a Secret is the operator's to report.
		It("puts no rule on the name of the gRPC sink's authority", func() {
			grpc := func(name string) mutation {
				return set(map[string]any{
					"enabled": true, "endpoint": "receiver:9000", "tls": map[string]any{"caSecretRef": name},
				}, "sinks", "grpc")
			}
			for i, name := range []string{"receiver-ca", "", "monitoring/receiver-ca", "Receiver_CA", strings.Repeat("a", 300)} {
				expectAccepted(monitor("grpc-tls-"+strconv.Itoa(i), grpc(name)))
			}
		})
	})

	// ------------------------------------------------------------------
	// Labels and annotations are copied onto the generated pod and Service;
	// an invalid one was accepted and then failed every reconcile.
	Describe("metadata overrides", func() {
		for _, target := range []string{"podMetadata", "serviceMetadata"} {
			Context(target, func() {
				DescribeTable("rejects invalid label keys",
					func(key, fragment string) {
						expectRejected(monitor("bad-label-key",
							set(map[string]any{key: "x"}, target, "labels")), fragment)
					},
					Entry("space", "bad key", "label keys must be valid Kubernetes label keys"),
					Entry("leading dash", "-team", "label keys must be valid Kubernetes label keys"),
					Entry("two slashes", "a/b/c", "label keys must be valid Kubernetes label keys"),
					Entry("upper-case prefix", "Example.com/team", "label keys must be valid Kubernetes label keys"),
					Entry("empty label in the prefix", "example..com/team", "label keys must be valid Kubernetes label keys"),
					Entry("dash next to a dot in the prefix", "a.-b/team", "label keys must be valid Kubernetes label keys"),
					Entry("prefix longer than 253 characters", strings.Repeat("a", 254)+"/team",
						"label keys must be valid Kubernetes label keys"),
					Entry("empty name after prefix", "example.com/", "label keys must be valid Kubernetes label keys"),
					Entry("name longer than 63 characters", strings.Repeat("a", 64),
						"label keys must be valid Kubernetes label keys"),
					Entry("reserved for the operator", "pgpilot.io/monitor",
						"label keys under pgpilot.io/ are reserved for the operator"),
				)

				DescribeTable("rejects invalid label values",
					func(value string) {
						expectRejected(monitor("bad-label-value",
							set(map[string]any{"team": value}, target, "labels")), "labels")
					},
					Entry("space", "data platform"),
					Entry("leading dash", "-data"),
					Entry("trailing dot", "data."),
					Entry("slash", "data/platform"),
					Entry("longer than 63 characters", strings.Repeat("a", 64)),
				)

				It("accepts valid labels", func() {
					expectAccepted(monitor("good-labels", set(map[string]any{
						"team":                      "data",
						"example.com/cost-center":   "cc_1.2-x",
						"app.kubernetes.io/part-of": "platform",
						"empty":                     "",
						strings.Repeat("k", 63):     strings.Repeat("v", 63),
					}, target, "labels")))
				})

				DescribeTable("rejects invalid annotation keys",
					func(key, fragment string) {
						expectRejected(monitor("bad-annotation",
							set(map[string]any{key: "x"}, target, "annotations")), fragment)
					},
					Entry("space", "bad key", "annotation keys must be valid Kubernetes annotation keys"),
					Entry("two slashes", "a/b/c", "annotation keys must be valid Kubernetes annotation keys"),
					Entry("empty label in the prefix", "example..com/owner",
						"annotation keys must be valid Kubernetes annotation keys"),
					Entry("prefix longer than 253 characters", strings.Repeat("a", 254)+"/owner",
						"annotation keys must be valid Kubernetes annotation keys"),
					Entry("reserved for the operator", "pgpilot.io/config-hash",
						"annotation keys under pgpilot.io/ are reserved for the operator"),
				)

				It("accepts annotations with free-form values", func() {
					expectAccepted(monitor("good-annotations", set(map[string]any{
						"prometheus.io/scrape": "true",
						"prometheus.io/port":   "9187",
						// Kubernetes compares annotation prefixes without
						// regard to case, unlike label prefixes.
						"Example.com/Owner": "data",
						"example.com/notes": "any text, with spaces / slashes: and " + strings.Repeat("x", 300),
					}, target, "annotations")))
				})
			})
		}
	})

	// ------------------------------------------------------------------
	// ------------------------------------------------------------------
	// Where the collector pod runs and what it is given. The fields of a
	// pod are passed on as they are, and what only a pod is refused for is
	// reported when the collector is rolled out; what the monitor itself
	// can be held to is refused here.
	Describe("the collector pod", func() {
		It("accepts scheduling, a ServiceAccount, pull secrets and environment variables", func() {
			got := stored(monitor("placed",
				set(map[string]any{
					"nodeSelector": map[string]any{"pool": "monitoring", "node.kubernetes.io/instance-type": "", "disk": "ssd"},
					"tolerations": []any{
						map[string]any{"key": "dedicated", "operator": "Equal", "value": "monitoring", "effect": "NoSchedule"},
						map[string]any{"operator": "Exists"},
					},
					"affinity": map[string]any{"nodeAffinity": map[string]any{
						"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{"nodeSelectorTerms": []any{
							map[string]any{"matchExpressions": []any{map[string]any{
								"key": "topology.kubernetes.io/zone", "operator": "In", "values": []any{"a", "b"},
							}}},
						}},
					}},
					"topologySpreadConstraints": []any{map[string]any{
						"maxSkew": int64(1), "topologyKey": "topology.kubernetes.io/zone", "whenUnsatisfiable": "ScheduleAnyway",
						"labelSelector": map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/managed-by": "pgpilot-operator"}},
					}},
					"priorityClassName": "monitoring",
				}, "scheduling"),
				set("collector", "serviceAccountName"),
				set([]any{map[string]any{"name": "registry-a"}, map[string]any{"name": "registry.b"}}, "image", "pullSecrets"),
				set([]any{
					map[string]any{"name": "PGAPPNAME", "value": "pgwatch"},
					map[string]any{"name": "https_proxy", "value": ""},
					map[string]any{"name": "_UNDERSCORE1", "valueFrom": map[string]any{
						"secretKeyRef": map[string]any{"name": "proxy", "key": "url"},
					}},
					map[string]any{"name": "PW_MIN_DB_SIZE_MB", "value": "8"},
					// Not the operator's: only the names are, not what begins like them.
					map[string]any{"name": "PGUSERNAME", "value": "x"},
					map[string]any{"name": "PW_SINKS", "value": "x"},
					map[string]any{"name": "pguser", "value": "x"},
				}, "env"),
			))
			spec, _, _ := unstructured.NestedMap(got.Object, "spec")
			Expect(spec).To(HaveKey("scheduling"))
			Expect(spec["serviceAccountName"]).To(Equal("collector"))
			// A pull secret brings an image block, and with it the default
			// pull policy, which is what a monitor without one gets as well.
			policy, _, _ := unstructured.NestedString(got.Object, "spec", "image", "pullPolicy")
			Expect(policy).To(Equal("IfNotPresent"))
		})

		It("accepts the blocks empty", func() {
			expectAccepted(monitor("empty-blocks",
				set(map[string]any{}, "scheduling"), set("", "serviceAccountName"), set([]any{}, "env"),
				set([]any{}, "image", "pullSecrets"), set("", "scheduling", "priorityClassName")))
		})

		DescribeTable("rejects what could not be the name of the object it names",
			func(value any, at string, path ...string) {
				expectRejected(monitor("unusable", set(value, path...)), at)
			},
			Entry("a ServiceAccount of another namespace", "other/collector", "spec.serviceAccountName", "serviceAccountName"),
			Entry("a ServiceAccount in upper case", "Collector", "spec.serviceAccountName", "serviceAccountName"),
			Entry("a ServiceAccount name of 254 characters", strings.Repeat("a", 254), "spec.serviceAccountName", "serviceAccountName"),
			Entry("a priority class in upper case", "High", "spec.scheduling.priorityClassName", "scheduling", "priorityClassName"),
			Entry("a priority class of 254 characters", strings.Repeat("a", 254), "spec.scheduling.priorityClassName",
				"scheduling", "priorityClassName"),
			Entry("a pull secret of another namespace", []any{map[string]any{"name": "other/registry"}},
				"spec.image.pullSecrets[0].name", "image", "pullSecrets"),
			Entry("a pull secret without a name", []any{map[string]any{}}, "spec.image.pullSecrets[0].name", "image", "pullSecrets"),
			Entry("a pull secret with an empty name", []any{map[string]any{"name": ""}},
				"spec.image.pullSecrets[0].name", "image", "pullSecrets"),
			Entry("a pull secret name of 254 characters", []any{map[string]any{"name": strings.Repeat("a", 254)}},
				"spec.image.pullSecrets[0].name", "image", "pullSecrets"),
			Entry("a node selector value that is no label value", map[string]any{"pool": "not a label!"},
				"spec.scheduling.nodeSelector", "scheduling", "nodeSelector"),
			Entry("a node selector value of 64 characters", map[string]any{"pool": strings.Repeat("a", 64)},
				"spec.scheduling.nodeSelector", "scheduling", "nodeSelector"),
		)

		It("limits how much of each there can be", func() {
			many := func(n int, item func(i int) any) []any {
				out := make([]any, 0, n)
				for i := range n {
					out = append(out, item(i))
				}
				return out
			}
			secret := func(i int) any { return map[string]any{"name": "registry-" + strconv.Itoa(i)} }
			toleration := func(i int) any { return map[string]any{"key": "k" + strconv.Itoa(i), "operator": "Exists"} }
			spread := func(i int) any {
				return map[string]any{"maxSkew": int64(1), "topologyKey": "k" + strconv.Itoa(i), "whenUnsatisfiable": "ScheduleAnyway"}
			}
			env := func(i int) any { return map[string]any{"name": "VAR_" + strconv.Itoa(i), "value": "x"} }

			expectAccepted(monitor("most", set(many(8, secret), "image", "pullSecrets"),
				set(many(32, toleration), "scheduling", "tolerations"),
				set(many(8, spread), "scheduling", "topologySpreadConstraints"),
				set(stringMap(32), "scheduling", "nodeSelector"),
				set(many(64, env), "env")))
			expectRejected(monitor("too-many", set(many(9, secret), "image", "pullSecrets")), "spec.image.pullSecrets")
			expectRejected(monitor("too-many", set(many(33, toleration), "scheduling", "tolerations")), "spec.scheduling.tolerations")
			expectRejected(monitor("too-many", set(many(9, spread), "scheduling", "topologySpreadConstraints")),
				"spec.scheduling.topologySpreadConstraints")
			expectRejected(monitor("too-many", set(stringMap(33), "scheduling", "nodeSelector")), "spec.scheduling.nodeSelector")
			expectRejected(monitor("too-many", set(many(65, env), "env")), "spec.env")
		})

		It("accepts names as long and as dotted as an object can have", func() {
			long := strings.Repeat("a", 253)
			expectAccepted(monitor("longest-names",
				set(long, "serviceAccountName"), set(long, "scheduling", "priorityClassName"),
				set([]any{map[string]any{"name": long}, map[string]any{"name": "registry.example.com-pull"}}, "image", "pullSecrets")))
			expectAccepted(monitor("dotted-names",
				set("team.collector-1", "serviceAccountName"), set("system-cluster-critical", "scheduling", "priorityClassName")))
		})

		// A pod has one constraint for each topology key and each way of
		// dealing with what cannot be satisfied. Two for the same pair
		// are refused here; passed on, the API server would not say that
		// the Deployment is invalid but fail on it, again and again.
		It("takes one spread constraint for each topology key and whenUnsatisfiable", func() {
			constraint := func(key, when string, skew int64) any {
				return map[string]any{"maxSkew": skew, "topologyKey": key, "whenUnsatisfiable": when}
			}
			expectRejected(monitor("twice", set([]any{
				constraint("topology.kubernetes.io/zone", "ScheduleAnyway", 1),
				constraint("topology.kubernetes.io/zone", "ScheduleAnyway", 2),
			}, "scheduling", "topologySpreadConstraints")), "spec.scheduling.topologySpreadConstraints[1]", "Duplicate value")
			expectAccepted(monitor("each-once", set([]any{
				constraint("topology.kubernetes.io/zone", "ScheduleAnyway", 1),
				constraint("topology.kubernetes.io/zone", "DoNotSchedule", 2),
				constraint("kubernetes.io/hostname", "ScheduleAnyway", 1),
			}, "scheduling", "topologySpreadConstraints")))
		})

		It("lists a pull secret once", func() {
			expectRejected(monitor("twice",
				set([]any{map[string]any{"name": "registry"}, map[string]any{"name": "registry"}}, "image", "pullSecrets")),
				"spec.image.pullSecrets[1]", "Duplicate value")
		})

		// The operator passes these to pgwatch itself: the credentials, and
		// where the collector finds its configuration and sends what it
		// collects. One of them given here would be ignored, or worse, used.
		DescribeTable("keeps what the operator sets out of env",
			func(name string) {
				expectRejected(monitor("reserved", set([]any{
					map[string]any{"name": "PGAPPNAME", "value": "pgwatch"}, map[string]any{"name": name, "value": "x"},
				}, "env")), "PGUSER, PGPASSWORD, PW_SOURCES, PW_METRICS, PW_SINK and PW_WEBDISABLE are set by the operator")
				expectRejected(monitor("reserved", set([]any{map[string]any{
					"name": name, "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "other", "key": "k"}},
				}}, "env")), "are set by the operator")
			},
			Entry("PGUSER", "PGUSER"), Entry("PGPASSWORD", "PGPASSWORD"), Entry("PW_SOURCES", "PW_SOURCES"),
			Entry("PW_METRICS", "PW_METRICS"), Entry("PW_SINK", "PW_SINK"), Entry("PW_WEBDISABLE", "PW_WEBDISABLE"),
		)

		DescribeTable("rejects an environment variable that could not be one",
			func(variable map[string]any, fragment string) {
				expectRejected(monitor("unusable", set([]any{variable}, "env")), fragment)
			},
			Entry("no name", map[string]any{"value": "x"}, "spec.env[0].name"),
			Entry("an empty name", map[string]any{"name": "", "value": "x"}, "spec.env[0].name"),
			Entry("a name that starts with a digit", map[string]any{"name": "1ST", "value": "x"}, "spec.env[0].name"),
			Entry("a name with an equals sign", map[string]any{"name": "A=B", "value": "x"}, "spec.env[0].name"),
			Entry("a name with a space", map[string]any{"name": "MY VAR", "value": "x"}, "spec.env[0].name"),
			Entry("a name with a dash", map[string]any{"name": "MY-VAR", "value": "x"}, "spec.env[0].name"),
			Entry("a name of 129 characters", map[string]any{"name": strings.Repeat("A", 129), "value": "x"}, "spec.env[0].name"),
			Entry("a value of 4097 characters", map[string]any{"name": "A", "value": strings.Repeat("x", 4097)}, "spec.env[0].value"),
			Entry("a value and a source for it", map[string]any{
				"name": "A", "value": "x", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "s", "key": "k"}},
			}, "set value or valueFrom, not both"),
		)

		It("accepts the longest name and value, and an empty value next to a source", func() {
			expectAccepted(monitor("longest", set([]any{
				map[string]any{"name": strings.Repeat("A", 128), "value": strings.Repeat("x", 4096)},
				map[string]any{"name": "B", "value": "", "valueFrom": map[string]any{
					"configMapKeyRef": map[string]any{"name": "settings", "key": "b"},
				}},
			}, "env")))
		})

		It("names a variable once", func() {
			expectRejected(monitor("twice", set([]any{
				map[string]any{"name": "PGAPPNAME", "value": "a"}, map[string]any{"name": "PGAPPNAME", "value": "b"},
			}, "env")), "spec.env[1]", "Duplicate value")
		})
	})

	Describe("status", func() {
		// With omitempty a false value was never written, so the READY
		// column was blank for exactly the monitors that needed attention.
		It("always writes status.ready on a monitor", func() {
			obj := monitor("not-ready")
			createForSpec(obj)

			Eventually(func(g Gomega) {
				got := obj.DeepCopy()
				g.Expect(k8sClient.Get(context.TODO(), client.ObjectKeyFromObject(obj), got)).To(Succeed())
				_, hasConditions, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
				g.Expect(hasConditions).To(BeTrue(), "the controller has not written status yet")

				ready, found, err := unstructured.NestedBool(got.Object, "status", "ready")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(found).To(BeTrue(), "status.ready must be present even when false")
				g.Expect(ready).To(BeFalse())
			}, timeout, interval).Should(Succeed())
		})

		// The "Metrics" column pointed at a JSONPath that can never yield an
		// integer; it now reads this field.
		It("reports the number of metrics of a library", func() {
			obj := library("counted", validMetric("one"), validMetric("two"), validMetric("three"))
			createForSpec(obj)

			Eventually(func(g Gomega) {
				got := obj.DeepCopy()
				g.Expect(k8sClient.Get(context.TODO(), client.ObjectKeyFromObject(obj), got)).To(Succeed())
				count, found, err := unstructured.NestedInt64(got.Object, "status", "metricCount")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(found).To(BeTrue())
				g.Expect(count).To(BeEquivalentTo(3))
			}, timeout, interval).Should(Succeed())
		})

		It("follows the library when metrics are removed", func() {
			obj := library("shrinking", validMetric("one"), validMetric("two"))
			createForSpec(obj)

			Eventually(func(g Gomega) {
				got := obj.DeepCopy()
				g.Expect(k8sClient.Get(context.TODO(), client.ObjectKeyFromObject(obj), got)).To(Succeed())
				count, _, _ := unstructured.NestedInt64(got.Object, "status", "metricCount")
				g.Expect(count).To(BeEquivalentTo(2))
			}, timeout, interval).Should(Succeed())

			Eventually(func(g Gomega) {
				got := obj.DeepCopy()
				g.Expect(k8sClient.Get(context.TODO(), client.ObjectKeyFromObject(obj), got)).To(Succeed())
				g.Expect(unstructured.SetNestedSlice(got.Object, []any{validMetric("one")}, "spec", "metrics")).To(Succeed())
				g.Expect(k8sClient.Update(context.TODO(), got)).To(Succeed())
			}, timeout, interval).Should(Succeed())

			Eventually(func(g Gomega) {
				got := obj.DeepCopy()
				g.Expect(k8sClient.Get(context.TODO(), client.ObjectKeyFromObject(obj), got)).To(Succeed())
				count, _, _ := unstructured.NestedInt64(got.Object, "status", "metricCount")
				g.Expect(count).To(BeEquivalentTo(1))
			}, timeout, interval).Should(Succeed())
		})
	})

	// ------------------------------------------------------------------
	// Exactly one way of supplying credentials: an inline username and
	// password, or a Secret reference.
	Describe("credentials", func() {
		const (
			exactlyOne   = "set either inline username+password or credentialsSecret, not both (and not neither)"
			needPassword = "password is required when username is set inline"
			needUsername = "username is required when password is set inline"
		)
		secretRef := map[string]any{"name": "pg-creds"}

		DescribeTable("rejects incomplete or conflicting credentials",
			func(username, password any, secret map[string]any, fragment string) {
				mutations := []mutation{remove("database", "credentialsSecret")}
				if username != nil {
					mutations = append(mutations, set(username, "database", "username"))
				}
				if password != nil {
					mutations = append(mutations, set(password, "database", "password"))
				}
				if secret != nil {
					mutations = append(mutations, set(secret, "database", "credentialsSecret"))
				}
				expectRejected(monitor("bad-creds", mutations...), fragment)
			},
			Entry("nothing at all", nil, nil, nil, exactlyOne),
			Entry("inline and Secret together", "pgwatch", "secret", secretRef, exactlyOne),
			Entry("username without password", "pgwatch", nil, nil, needPassword),
			Entry("password without username", nil, "secret", nil, needUsername),
			// With a Secret present the "exactly one" rule is satisfied by
			// the Secret alone; the half-set inline pair must still be caught.
			Entry("username without password, next to a Secret", "pgwatch", nil, secretRef, needPassword),
			Entry("password without username, next to a Secret", nil, "secret", secretRef, needUsername),
			Entry("empty username", "", "secret", nil, "username"),
			Entry("empty password", "pgwatch", "", nil, "password"),
			Entry("Secret reference without a name", nil, nil, map[string]any{"name": ""}, "name"),
		)

		It("accepts inline credentials", func() {
			expectAccepted(monitor("inline",
				remove("database", "credentialsSecret"),
				set("pgwatch", "database", "username"),
				set("secret", "database", "password"),
			))
		})

		It("accepts a Secret reference", func() {
			expectAccepted(monitor("by-secret"))
		})

		// A name the API server could never have for a Secret used to be
		// accepted and then failed on every reconcile as a read error.
		DescribeTable("rejects a Secret name that is not an object name",
			func(name string) {
				expectRejected(monitor("unusable", set(name, "database", "credentialsSecret", "name")),
					"spec.database.credentialsSecret.name")
			},
			Entry("another namespace", "other-namespace/victim"),
			Entry("path traversal", "../../other/secrets/victim"),
			Entry("dots only", ".."),
			Entry("upper case", "DB-Creds"),
			Entry("underscore", "db_creds"),
			Entry("space", "db creds"),
			Entry("percent escape", "a%2Fb"),
			Entry("leading dash", "-creds"),
			Entry("trailing dot", "creds."),
			Entry("empty label", "db..creds"),
			Entry("longer than 253 characters", strings.Repeat("a", 254)),
		)

		DescribeTable("accepts valid Secret names",
			func(name string) {
				expectAccepted(monitor("usable", set(name, "database", "credentialsSecret", "name")))
			},
			Entry("simple", "db-creds"),
			Entry("with dots", "orders.db.credentials"),
			Entry("digits", "0db1"),
			Entry("single character", "a"),
			Entry("253 characters", strings.Repeat("a", 253)),
		)
	})
})
