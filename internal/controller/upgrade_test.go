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
	"path/filepath"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

// A cluster that upgrades the CRDs already holds objects, and some of them
// break a rule that did not exist when they were stored. This spec starts an
// API server with the CRDs of v1.0.2, stores such objects, installs the
// current CRDs over them and checks what the operator can still do with them.
//
// It runs in an API server of its own, without a manager: the reconciler is
// called directly, so nothing but the step under test touches the objects.
var _ = Describe("Upgrading the CRDs over existing objects", Ordered, func() {
	var (
		env        *envtest.Environment
		restConfig *rest.Config
		c          client.Client
		reconciler *PgpilotMonitorReconciler
		ns         string
		// imageNS holds the monitors of the specs about the image an
		// earlier release wrote into the spec.
		imageNS string
		// releaseNS holds a second set of the stale monitors, and one that
		// breaks no rule, for the spec about the finalizer release.
		releaseNS string
		// libraryNS holds a library that breaks a new rule and a monitor,
		// itself in order, that uses it, and monitors whose own definitions
		// break one. All of them carry the status v1.0.2 gave them.
		libraryNS string
		// What the API server tolerates for an object whose stored spec
		// breaks a rule, as long as the offending value is not changed:
		// from Kubernetes 1.30 updates of the object itself, from 1.33 also
		// writes to its status subresource. Older servers validate the
		// whole object on every write.
		ratchetsUpdates, ratchetsStatus bool
	)

	// stale lists monitors that were valid under the old CRDs. Each breaks
	// one of the new rules, written the way a user would have written it.
	stale := map[string]func(spec map[string]any){
		"sqls-wildcard": func(spec map[string]any) {
			spec["metrics"] = map[string]any{"custom": []any{map[string]any{
				"name": "queue_depth", "sqls": map[string]any{"14": "SELECT 1", "*": "SELECT 1"},
			}}}
		},
		"both-roles": func(spec map[string]any) {
			spec["metrics"] = map[string]any{"custom": []any{map[string]any{
				"name": "lag", "sqls": map[string]any{"14": "SELECT 1"},
				"masterOnly": true, "standbyOnly": true, "isInstanceLevel": false,
			}}}
		},
		// Explicit zero values are what makes a full-object update differ
		// from the stored object: the Go struct drops them.
		"no-sink": func(spec map[string]any) {
			spec["sinks"] = map[string]any{
				"prometheus": map[string]any{"enabled": false},
				"grpc":       map[string]any{"enabled": false, "endpoint": ""},
			}
		},
		"no-metrics": func(spec map[string]any) {
			spec["metrics"] = map[string]any{"preset": "", "custom": []any{}}
		},
		"reserved-label": func(spec map[string]any) {
			spec["podMetadata"] = map[string]any{"labels": map[string]any{"pgpilot.io/team": "data"}}
		},
		"reserved-tag": func(spec map[string]any) {
			spec["database"].(map[string]any)["customTags"] = map[string]any{"dbname": "mine"}
		},
		"duplicate-metrics": func(spec map[string]any) {
			metric := map[string]any{"name": "twice", "sqls": map[string]any{"14": "SELECT 1"}}
			spec["metrics"] = map[string]any{"custom": []any{metric, metric}}
		},
	}

	staleMonitor := func(name string) *unstructured.Unstructured {
		spec := map[string]any{
			"database": map[string]any{
				"host": "pg.example.com", "database": "appdb",
				"credentialsSecret": map[string]any{"name": "pg-creds"},
			},
			"metrics": map[string]any{"preset": "basic"},
			"sinks":   map[string]any{"prometheus": map[string]any{"enabled": true}},
		}
		stale[name](spec)
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": pgpilotv1.GroupVersion.String(),
			"kind":       "PgpilotMonitor",
			"metadata": map[string]any{
				"name": name, "namespace": ns,
				// As left behind by the operator that reconciled it.
				"finalizers": []any{finalizerName},
			},
			"spec": spec,
		}}
	}

	BeforeAll(func() {
		env = &envtest.Environment{
			CRDDirectoryPaths:     []string{filepath.Join("testdata", "crds-v1.0.2")},
			ErrorIfCRDPathMissing: true,
		}
		if dir := envtestBinaryDir(envtestAssetsFromEnv(), filepath.Join("..", "..", "bin", "k8s")); dir != "" {
			env.BinaryAssetsDirectory = dir
		}
		var err error
		restConfig, err = env.Start()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { Expect(env.Stop()).To(Succeed()) })

		c, err = client.New(restConfig, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
		reconciler = &PgpilotMonitorReconciler{
			Client:   c,
			Scheme:   scheme.Scheme,
			Recorder: record.NewFakeRecorder(1000),
		}

		dc, err := discovery.NewDiscoveryClientForConfig(restConfig)
		Expect(err).NotTo(HaveOccurred())
		version, err := dc.ServerVersion()
		Expect(err).NotTo(HaveOccurred())
		minor, err := strconv.Atoi(strings.TrimRight(version.Minor, "+"))
		Expect(err).NotTo(HaveOccurred())
		ratchetsUpdates = minor >= 30
		ratchetsStatus = minor >= 33

		created := &corev1.Namespace{}
		created.GenerateName = "test-upgrade-"
		Expect(c.Create(context.TODO(), created)).To(Succeed())
		ns = created.Name

		By("storing objects under the v1.0.2 CRDs")
		for name := range stale {
			Expect(c.Create(context.TODO(), staleMonitor(name))).To(Succeed(),
				"%s must be valid under the old CRDs, otherwise it proves nothing", name)
		}

		forRelease := &corev1.Namespace{}
		forRelease.GenerateName = "test-upgrade-release-"
		Expect(c.Create(context.TODO(), forRelease)).To(Succeed())
		releaseNS = forRelease.Name
		for name := range stale {
			monitor := staleMonitor(name)
			monitor.SetNamespace(releaseNS)
			Expect(c.Create(context.TODO(), monitor)).To(Succeed())
		}
		valid := minimalMonitor(releaseNS, "valid")
		valid.SetFinalizers([]string{finalizerName})
		Expect(c.Create(context.TODO(), valid)).To(Succeed())

		forLibrary := &corev1.Namespace{}
		forLibrary.GenerateName = "test-upgrade-library-"
		Expect(c.Create(context.TODO(), forLibrary)).To(Succeed())
		libraryNS = forLibrary.Name
		librarySecret := &corev1.Secret{StringData: map[string]string{"username": "pgwatch", "password": "secret"}}
		librarySecret.Name, librarySecret.Namespace = "pg-creds", libraryNS
		Expect(c.Create(context.TODO(), librarySecret)).To(Succeed())
		// The usual one: a statement for PostgreSQL 14 and one "for the
		// rest", which no collector ever ran.
		Expect(c.Create(context.TODO(), libraryOf(libraryNS, "old-rules", []any{map[string]any{
			"name": "queue_depth", "sqls": map[string]any{"14": "SELECT 1 AS depth", "*": "SELECT 0 AS depth"},
		}}))).To(Succeed(), "must be valid under the old CRDs, otherwise it proves nothing")
		Expect(c.Create(context.TODO(), withSpec(minimalMonitor(libraryNS, "uses-old-rules"),
			map[string]any{"fromLibraries": []any{map[string]any{"name": "old-rules"}}}, "metrics"))).To(Succeed())
		// What v1.0.2 said of every library that had an entry.
		oldRules := libraryOf(libraryNS, "old-rules", nil)
		Expect(c.Status().Patch(context.TODO(), oldRules, client.RawPatch(types.MergePatchType, []byte(
			`{"status":{"observedGeneration":1,"metricCount":1,"conditions":[{"type":"Valid","status":"True",`+
				`"reason":"Valid","message":"Library contains metric definitions",`+
				`"lastTransitionTime":"2026-01-01T00:00:00Z"}]}}`)))).To(Succeed())
		for name, custom := range map[string][]any{
			"inline-old-rules": {map[string]any{
				"name": "queue_depth", "sqls": map[string]any{"14": "SELECT 1 AS depth", "*": "SELECT 0 AS depth"},
			}},
			"inline-duplicate": {
				map[string]any{"name": "twice", "sqls": map[string]any{"14": "SELECT 1 AS depth"}},
				map[string]any{"name": "twice", "sqls": map[string]any{"14": "SELECT 2 AS depth"}},
			},
		} {
			inline := withSpec(minimalMonitor(libraryNS, name), map[string]any{"custom": custom}, "metrics")
			Expect(c.Create(context.TODO(), inline)).To(Succeed(), "%s must be valid under the old CRDs", name)
			// Collected and reported as running, as these were.
			Expect(c.Status().Patch(context.TODO(), inline, client.RawPatch(types.MergePatchType, []byte(
				`{"status":{"ready":true,"conditions":[{"type":"Ready","status":"True","reason":"PodRunning",`+
					`"message":"pgwatch pod is running","lastTransitionTime":"2026-01-01T00:00:00Z"}]}}`)))).To(Succeed())
		}

		By("letting the v1.0.2 operator reconcile monitors that name no image, or pin one")
		images := &corev1.Namespace{}
		images.GenerateName = "test-upgrade-image-"
		Expect(c.Create(context.TODO(), images)).To(Succeed())
		imageNS = images.Name
		secret := &corev1.Secret{StringData: map[string]string{"username": "pgwatch", "password": "secret"}}
		secret.Name, secret.Namespace = "pg-creds", imageNS
		Expect(c.Create(context.TODO(), secret)).To(Succeed())

		Expect(c.Create(context.TODO(), minimalMonitor(imageNS, "no-image"))).To(Succeed())
		Expect(c.Create(context.TODO(),
			withSpec(minimalMonitor(imageNS, "pinned"), map[string]any{"tag": "5.1.0"}, "image"))).To(Succeed())
		// What that release did on its first reconcile: read the object,
		// add the finalizer and send the whole object back.
		oldOperator := legacyOperatorClient(restConfig)
		for _, name := range []string{"no-image", "pinned"} {
			var monitor pgpilotv1.PgpilotMonitor
			Expect(oldOperator.Get(context.TODO(), types.NamespacedName{Name: name, Namespace: imageNS}, &monitor)).To(Succeed())
			monitor.Finalizers = append(monitor.Finalizers, finalizerName)
			Expect(oldOperator.Update(context.TODO(), &monitor)).To(Succeed())
		}

		By("installing the current CRDs over them")
		_, err = envtest.InstallCRDs(restConfig, envtest.CRDInstallOptions{
			Paths:              []string{filepath.Join("..", "..", "config", "crd", "bases")},
			ErrorIfPathMissing: true,
		})
		Expect(err).NotTo(HaveOccurred())

		// The API server swaps schemas asynchronously; wait until the new
		// rules are in force.
		Eventually(func(g Gomega) {
			probe := staleMonitor("sqls-wildcard")
			probe.SetName("probe")
			err := c.Create(context.TODO(), probe, client.DryRunAll)
			g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), "new rules not active yet: %v", err)
		}, timeout, interval).Should(Succeed())
	})

	It("rejects the same objects when they are created anew", func() {
		for name := range stale {
			fresh := staleMonitor(name)
			fresh.SetName("new-" + name)
			err := c.Create(context.TODO(), fresh)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%s: want an Invalid error, got: %v", name, err)
		}
	})

	It("still reads them with the typed client", func() {
		var list pgpilotv1.PgpilotMonitorList
		Expect(c.List(context.TODO(), &list, client.InNamespace(ns))).To(Succeed())
		Expect(list.Items).To(HaveLen(len(stale)))
	})

	// The operator writes the status whenever it changes, the way this does:
	// a merge patch of the status subresource against the version it read.
	// Before Kubernetes 1.33 that write is refused for an object breaking a
	// rule, even though the spec did not change.
	It("writes their status where the API server ratchets status updates", func() {
		for name := range stale {
			var monitor pgpilotv1.PgpilotMonitor
			Expect(c.Get(context.TODO(), types.NamespacedName{Name: name, Namespace: ns}, &monitor)).To(Succeed())
			stored := monitor.DeepCopy()
			monitor.Status.ConfigHash = "after-upgrade"
			err := c.Status().Patch(context.TODO(), &monitor,
				client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{}))

			if ratchetsStatus {
				Expect(err).NotTo(HaveOccurred(), "%s: status update must be accepted", name)
			} else {
				Expect(apierrors.IsInvalid(err)).To(BeTrue(),
					"%s: this Kubernetes version is expected to refuse the status update, got: %v", name, err)
			}
		}
	})

	// When it starts, the operator takes the finalizer of earlier releases
	// off every monitor, with a patch of metadata alone.
	It("takes the old finalizer off them when the operator starts, where the API server lets it", func() {
		finalizersOf := func(name string) []string {
			GinkgoHelper()
			var monitor pgpilotv1.PgpilotMonitor
			Expect(c.Get(context.TODO(), types.NamespacedName{Name: name, Namespace: releaseNS}, &monitor)).To(Succeed())
			return monitor.Finalizers
		}
		release := &FinalizerRelease{Reader: client.NewNamespacedClient(c, releaseNS), Client: c}
		released, err := release.Run(context.TODO())

		Expect(finalizersOf("valid")).To(BeEmpty(), "a monitor that breaks no rule is released by every API server")
		if !ratchetsUpdates {
			// Kubernetes 1.28 and 1.29 validate the whole object whatever
			// the write changes. These monitors keep the finalizer until
			// their spec is corrected, which the upgrade notes say.
			Expect(released).To(Equal(1))
			Expect(err).To(HaveOccurred())
			for name := range stale {
				Expect(err.Error()).To(ContainSubstring(releaseNS+"/"+name+": remove the finalizer: "),
					"the error has to name each monitor that is left")
				Expect(finalizersOf(name)).To(Equal([]string{finalizerName}))
			}
			Expect(err.Error()).To(ContainSubstring("is invalid"), "and say why the API server refused")
			return
		}
		Expect(err).NotTo(HaveOccurred(), "releasing the finalizer must not be refused")
		Expect(released).To(Equal(len(stale) + 1))
		for name := range stale {
			Expect(finalizersOf(name)).To(BeEmpty(), name)
		}
	})

	// A full Update of the object re-serialises the spec, which the API
	// server sees as a change and validates again: the finalizer could not
	// be removed and the object stayed in Terminating for good.
	It("lets every one of them be deleted", func() {
		for name := range stale {
			By(name)
			key := types.NamespacedName{Name: name, Namespace: ns}
			var monitor pgpilotv1.PgpilotMonitor
			Expect(c.Get(context.TODO(), key, &monitor)).To(Succeed())
			Expect(c.Delete(context.TODO(), &monitor)).To(Succeed())

			Expect(c.Get(context.TODO(), key, &monitor)).To(Succeed())
			Expect(monitor.DeletionTimestamp.IsZero()).To(BeFalse(), "the finalizer must hold the object")

			_, err := reconciler.Reconcile(context.TODO(), ctrl.Request{NamespacedName: key})
			if !ratchetsUpdates {
				// Kubernetes 1.28 and 1.29 refuse every write to such an
				// object. It stays in Terminating until its spec is fixed,
				// which the upgrade notes say.
				Expect(apierrors.IsInvalid(err)).To(BeTrue(),
					"this Kubernetes version is expected to refuse the finalizer removal, got: %v", err)
				continue
			}
			Expect(err).NotTo(HaveOccurred(), "releasing the finalizer must not be refused")

			err = c.Get(context.TODO(), key, &monitor)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the object must be gone, got: %v", err)
		}
	})

	// The API server does not look at a stored object again when a rule is
	// added. What the operator would render from a definition that breaks
	// one is not what its author meant, so it says which one it is instead.
	Describe("a metric definition stored before the rules", Ordered, func() {
		var monitorKey, libraryKey types.NamespacedName

		conditionOf := func(conditions []metav1.Condition, condType string) metav1.Condition {
			GinkgoHelper()
			for _, cond := range conditions {
				if cond.Type == condType {
					return cond
				}
			}
			Fail("no " + condType + " condition")
			return metav1.Condition{}
		}

		BeforeAll(func() {
			monitorKey = types.NamespacedName{Name: "uses-old-rules", Namespace: libraryNS}
			libraryKey = types.NamespacedName{Name: "old-rules", Namespace: libraryNS}
		})

		// The monitor itself breaks no rule, so every API server takes its
		// status.
		It("is named by the monitor that uses it, and nothing is generated from it", func() {
			_, err := reconciler.Reconcile(context.TODO(), ctrl.Request{NamespacedName: monitorKey})
			Expect(err).NotTo(HaveOccurred())

			var monitor pgpilotv1.PgpilotMonitor
			Expect(c.Get(context.TODO(), monitorKey, &monitor)).To(Succeed())
			ready := conditionOf(monitor.Status.Conditions, "Ready")
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal("InvalidMetric"))
			Expect(ready.Message).To(ContainSubstring(
				`PgpilotMetricLibrary old-rules cannot be used: metric "queue_depth": sqls key "*" is not a PostgreSQL major version`))

			err = c.Get(context.TODO(), types.NamespacedName{Name: "pgpilot-uses-old-rules-config", Namespace: libraryNS},
				&corev1.ConfigMap{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a ConfigMap rendered from that definition: %v", err)
		})

		It("is reported by the library itself where the API server lets its status be written", func() {
			libraries := &PgpilotMetricLibraryReconciler{Client: c, Scheme: scheme.Scheme}
			_, err := libraries.Reconcile(context.TODO(), ctrl.Request{NamespacedName: libraryKey})
			var library pgpilotv1.PgpilotMetricLibrary
			Expect(c.Get(context.TODO(), libraryKey, &library)).To(Succeed())
			valid := conditionOf(library.Status.Conditions, "Valid")
			if !ratchetsStatus {
				// Before Kubernetes 1.33 no status can be written to an
				// object that breaks a rule: the library keeps what the
				// earlier release said of it, and the monitor's is where
				// to look.
				Expect(apierrors.IsInvalid(err)).To(BeTrue(),
					"this Kubernetes version is expected to refuse the status of the library, got: %v", err)
				Expect(valid.Status).To(Equal(metav1.ConditionTrue))
				Expect(valid.Message).To(Equal("Library contains metric definitions"))
				return
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(valid.Status).To(Equal(metav1.ConditionFalse))
			Expect(valid.Reason).To(Equal("InvalidMetric"))
			Expect(valid.Message).To(ContainSubstring(`metric "queue_depth": sqls key "*"`))
			Expect(library.Status.MetricCount).To(BeEquivalentTo(1))
		})

		// A monitor whose own definitions break a rule is itself an object
		// the API server refuses the status of, before Kubernetes 1.33. It
		// keeps saying what the earlier release left there.
		It("is named for a monitor's own definitions, by an event where the status cannot be written", func() {
			recorder := reconciler.Recorder.(*record.FakeRecorder)
			for name, problem := range map[string]string{
				"inline-old-rules": `metric "queue_depth": sqls key "*" is not a PostgreSQL major version`,
				"inline-duplicate": `metric "twice" is defined more than once`,
			} {
				key := types.NamespacedName{Name: name, Namespace: libraryNS}
				drain(recorder)
				_, err := reconciler.Reconcile(context.TODO(), ctrl.Request{NamespacedName: key})
				Expect(drain(recorder)).To(
					ConsistOf(HavePrefix("Warning InvalidMetric spec.metrics.custom cannot be used: "+problem)), name)
				err2 := c.Get(context.TODO(), types.NamespacedName{Name: "pgpilot-" + name + "-config", Namespace: libraryNS},
					&corev1.ConfigMap{})
				Expect(apierrors.IsNotFound(err2)).To(BeTrue(), "%s: a ConfigMap rendered from that definition: %v", name, err2)

				var monitor pgpilotv1.PgpilotMonitor
				Expect(c.Get(context.TODO(), key, &monitor)).To(Succeed())
				ready := conditionOf(monitor.Status.Conditions, "Ready")
				if !ratchetsStatus {
					Expect(apierrors.IsInvalid(err)).To(BeTrue(),
						"%s: this Kubernetes version is expected to refuse the status of the monitor, got: %v", name, err)
					Expect(ready.Reason).To(Equal("PodRunning"), name)
					Expect(monitor.Status.Ready).To(BeTrue(), name)
					continue
				}
				Expect(err).NotTo(HaveOccurred(), name)
				Expect(ready.Status).To(Equal(metav1.ConditionFalse), name)
				Expect(ready.Reason).To(Equal("InvalidMetric"), name)
				Expect(ready.Message).To(ContainSubstring(problem), name)
				Expect(monitor.Status.Ready).To(BeFalse(), name)
			}
		})

		// Correcting what breaks the rule is accepted by every API server.
		It("is used again once it is corrected", func() {
			library := libraryOf(libraryNS, "old-rules", nil)
			Expect(c.Get(context.TODO(), libraryKey, library)).To(Succeed())
			Expect(unstructured.SetNestedSlice(library.Object, []any{map[string]any{
				"name": "queue_depth", "sqls": map[string]any{"14": "SELECT 1 AS depth", "0": "SELECT 0 AS depth"},
			}}, "spec", "metrics")).To(Succeed())
			Expect(c.Update(context.TODO(), library)).To(Succeed())

			_, err := reconciler.Reconcile(context.TODO(), ctrl.Request{NamespacedName: monitorKey})
			Expect(err).NotTo(HaveOccurred())

			var monitor pgpilotv1.PgpilotMonitor
			Expect(c.Get(context.TODO(), monitorKey, &monitor)).To(Succeed())
			Expect(conditionOf(monitor.Status.Conditions, "ConfigGenerated").Status).To(Equal(metav1.ConditionTrue))
			Expect(conditionOf(monitor.Status.Conditions, "Ready").Reason).NotTo(Equal("InvalidMetric"))
			var config corev1.ConfigMap
			Expect(c.Get(context.TODO(), types.NamespacedName{Name: "pgpilot-uses-old-rules-config", Namespace: libraryNS},
				&config)).To(Succeed())
			Expect(config.Data["metrics.yaml"]).To(ContainSubstring("queue_depth"))
		})
	})

	// The monitor that named no image has one in its stored spec all the
	// same, written by the old operator's update and the old CRD's defaults.
	// Left there it would pin the collector to pgwatch 5.1.0 for good.
	Describe("the pgwatch image an earlier release wrote into the spec", Ordered, func() {
		key := func(name string) types.NamespacedName {
			return types.NamespacedName{Name: name, Namespace: imageNS}
		}
		reconciled := func(name string) string {
			GinkgoHelper()
			_, err := reconciler.Reconcile(context.TODO(), ctrl.Request{NamespacedName: key(name)})
			Expect(err).NotTo(HaveOccurred())
			return collectorImage(c, key(name))(Default)
		}

		It("is in every monitor that release reconciled", func() {
			Expect(storedImage(c, key("no-image"))).To(Equal(legacyImageBlock()))
			Expect(storedImage(c, key("pinned"))).To(Equal(legacyImageBlock()))
			Expect(reconciled("no-image")).To(Equal(legacyImage), "without the migration the stored tag decides")
		})

		It("is removed where nobody asked for it, and kept where the user did", func() {
			migration := &StoredImageMigration{Reader: c, Client: c, Recorder: record.NewFakeRecorder(10)}
			result, err := migration.Run(context.TODO())
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Migrated).To(Equal(1))
			Expect(result.Kept).To(ConsistOf(HavePrefix(imageNS + "/pinned (tag set by ")))

			Expect(storedImage(c, key("no-image"))).To(Equal(map[string]any{"pullPolicy": "IfNotPresent"}))
			Expect(reconciled("no-image")).To(Equal(currentImage))

			// The user wrote the tag; the repository next to it came from
			// the old CRD's default, in the user's own request.
			Expect(storedImage(c, key("pinned"))).To(Equal(legacyImageBlock()))
			Expect(reconciled("pinned")).To(Equal(legacyImage))
		})

		// A rollback to the old release and a second upgrade. Under the old
		// CRD the API server fills the tag in again, and the old operator's
		// next status update stores it - this time owned by nobody.
		It("is not reported as removed while the old CRD is served, and is removed after the CRDs are upgraded", func() {
			installCRDs := func(dir ...string) {
				GinkgoHelper()
				_, err := envtest.InstallCRDs(restConfig, envtest.CRDInstallOptions{
					Paths: []string{filepath.Join(dir...)}, ErrorIfPathMissing: true,
				})
				Expect(err).NotTo(HaveOccurred())
			}
			newRulesActive := func() bool {
				probe := staleMonitor("sqls-wildcard")
				probe.SetName("probe")
				return apierrors.IsInvalid(c.Create(context.TODO(), probe, client.DryRunAll))
			}
			Expect(storedImage(c, key("no-image"))).To(Equal(map[string]any{"pullPolicy": "IfNotPresent"}),
				"the spec before starts from the migrated state")

			By("rolling back to the old CRDs and letting the old operator write status")
			installCRDs("testdata", "crds-v1.0.2")
			Eventually(newRulesActive, timeout, interval).Should(BeFalse())
			Eventually(func() map[string]any { return storedImage(c, key("no-image")) }, timeout, interval).
				Should(HaveKeyWithValue("tag", "5.1.0"), "the old CRD defaults the tag whenever the object is read")
			oldOperator := legacyOperatorClient(restConfig)
			var monitor pgpilotv1.PgpilotMonitor
			Expect(oldOperator.Get(context.TODO(), key("no-image"), &monitor)).To(Succeed())
			monitor.Status.ConfigHash = "written-after-the-rollback"
			Expect(oldOperator.Status().Update(context.TODO(), &monitor)).To(Succeed())

			By("running the migration while the old CRD is still served")
			recorder := record.NewFakeRecorder(10)
			migration := &StoredImageMigration{Reader: c, Client: c, Recorder: recorder}
			result, err := migration.Run(context.TODO())
			Expect(err).To(MatchError(errTagStillDefaulted))
			Expect(result.Migrated).To(BeZero(), "nothing changed, so nothing may be counted")
			Expect(drain(recorder)).To(BeEmpty(), "and no event may claim otherwise")
			Expect(storedImage(c, key("no-image"))).To(HaveKeyWithValue("tag", "5.1.0"))

			By("upgrading the CRDs again")
			installCRDs("..", "..", "config", "crd", "bases")
			Eventually(newRulesActive, timeout, interval).Should(BeTrue())
			Expect(storedImage(c, key("no-image"))).To(HaveKeyWithValue("tag", "5.1.0"),
				"the status update under the old CRD stored the default")
			var stored pgpilotv1.PgpilotMonitor
			Expect(c.Get(context.TODO(), key("no-image"), &stored)).To(Succeed())
			owned, err := legacyOwnedImageFields(&stored)
			Expect(err).NotTo(HaveOccurred())
			Expect(owned.tag).To(BeTrue(), "a tag nobody owns is a default nobody asked for")

			result, err = migration.Run(context.TODO())
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Migrated).To(Equal(1))
			Expect(drain(recorder)).To(HaveLen(1))
			Expect(storedImage(c, key("no-image"))).To(Equal(map[string]any{"pullPolicy": "IfNotPresent"}))
			Expect(reconciled("no-image")).To(Equal(currentImage))
			Expect(storedImage(c, key("pinned"))).To(Equal(legacyImageBlock()), "the user's tag survives all of it")
		})
	})
})
