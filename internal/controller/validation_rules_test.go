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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
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

	expectAccepted := func(obj *unstructured.Unstructured) {
		GinkgoHelper()
		Expect(k8sClient.Create(context.TODO(), obj)).To(Succeed())
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

	stored := func(obj *unstructured.Unstructured) *unstructured.Unstructured {
		GinkgoHelper()
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
				obj := monitor("prom-default", set(sinks, "sinks"))
				expectAccepted(obj)

				got := stored(obj)
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
			obj := monitor("secret-keys", set("db.user-name_1", "database", "credentialsSecret", "usernameKey"))
			expectAccepted(obj)
			got := stored(obj)
			user, _, _ := unstructured.NestedString(got.Object, "spec", "database", "credentialsSecret", "usernameKey")
			pass, _, _ := unstructured.NestedString(got.Object, "spec", "database", "credentialsSecret", "passwordKey")
			Expect(user).To(Equal("db.user-name_1"))
			Expect(pass).To(Equal("password"), "passwordKey must be defaulted")
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
	Describe("status", func() {
		// With omitempty a false value was never written, so the READY
		// column was blank for exactly the monitors that needed attention.
		It("always writes status.ready on a monitor", func() {
			obj := monitor("not-ready")
			expectAccepted(obj)

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
			expectAccepted(obj)

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
			expectAccepted(obj)

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
	})
})
