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
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

const (
	currentImage = "cybertecpostgresql/pgwatch:6.2.0"
	legacyImage  = "cybertecpostgresql/pgwatch:5.1.0"
)

// legacyOperatorClient talks to the API server the way operator releases up
// to v1.0.2 did: without a field manager of their own, so that the API server
// records their writes under the name of the binary.
func legacyOperatorClient(base *rest.Config) client.Client {
	GinkgoHelper()
	legacy := rest.CopyConfig(base)
	legacy.UserAgent = "manager/v0.0.0 (linux/amd64) kubernetes/$Format"
	c, err := client.New(legacy, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	return c
}

// legacyImageBlock is what those releases left in the spec of a monitor.
func legacyImageBlock() map[string]any {
	return map[string]any{
		"repository": builder.DefaultImageRepository,
		"tag":        legacyImageTag,
		"pullPolicy": "IfNotPresent",
	}
}

// storedImage returns spec.image as the API server holds it.
func storedImage(c client.Client, key types.NamespacedName) map[string]any {
	GinkgoHelper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(pgpilotv1.GroupVersion.WithKind("PgpilotMonitor"))
	Expect(c.Get(context.TODO(), key, obj)).To(Succeed())
	image, _, err := unstructured.NestedMap(obj.Object, "spec", "image")
	Expect(err).NotTo(HaveOccurred())
	return image
}

func collectorImage(c client.Client, key types.NamespacedName) func(g Gomega) string {
	return func(g Gomega) string {
		var dep appsv1.Deployment
		g.Expect(c.Get(context.TODO(),
			types.NamespacedName{Name: builder.ResourceName(key.Name), Namespace: key.Namespace}, &dep)).To(Succeed())
		g.Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
		return dep.Spec.Template.Spec.Containers[0].Image
	}
}

func drain(recorder *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case e := <-recorder.Events:
			events = append(events, e)
		default:
			return events
		}
	}
}

var _ = Describe("pgwatch image of a monitor", func() {
	var ns string

	BeforeEach(func() {
		ns = testNamespace("test-image-")
		secret := &corev1.Secret{StringData: map[string]string{"username": "pgwatch", "password": "secret"}}
		secret.Name, secret.Namespace = "pg-creds", ns
		createForSpec(secret)
	})

	It("is the release the operator is built for when the monitor names none", func() {
		monitor := minimalMonitor(ns, "follows-operator")
		createForSpec(monitor)
		key := client.ObjectKeyFromObject(monitor)

		Eventually(collectorImage(k8sClient, key), timeout, interval).Should(Equal(currentImage))
		Expect(currentImage).To(Equal(builder.DefaultImageRepository + ":" + builder.PgwatchVersion))

		// Nothing may write the image back into the spec: a stored tag is
		// what keeps a monitor on an old release after the operator moved on.
		Consistently(func() map[string]any { return storedImage(k8sClient, key) }, 2*time.Second, interval).
			Should(BeEmpty())
	})

	DescribeTable("keeps what the monitor sets",
		func(image map[string]any, want string) {
			monitor := withSpec(minimalMonitor(ns, "pinned"), image, "image")
			createForSpec(monitor)
			Eventually(collectorImage(k8sClient, client.ObjectKeyFromObject(monitor)), timeout, interval).
				Should(Equal(want))
		},
		Entry("a tag", map[string]any{"tag": "5.3.0"}, "cybertecpostgresql/pgwatch:5.3.0"),
		Entry("the legacy tag, set by the user", map[string]any{"tag": "5.1.0"}, legacyImage),
		Entry("a repository", map[string]any{"repository": "registry.example.com/mirror/pgwatch"},
			"registry.example.com/mirror/pgwatch:6.2.0"),
		Entry("both", map[string]any{"repository": "registry.example.com/pgwatch", "tag": "6.1.0"},
			"registry.example.com/pgwatch:6.1.0"),
		Entry("only a pull policy", map[string]any{"pullPolicy": "Always"}, currentImage),
	)

	// The monitor was stored before the manager of the suite started; nothing
	// in this spec runs the migration.
	It("is moved to the operator's release by the manager itself when it starts", func() {
		Eventually(func() map[string]any { return storedImage(k8sClient, leftBehindBeforeStart) }, timeout, interval).
			Should(Equal(map[string]any{"pullPolicy": "IfNotPresent"}))
		Eventually(collectorImage(k8sClient, leftBehindBeforeStart), timeout, interval).Should(Equal(currentImage))

		Eventually(func(g Gomega) {
			var events corev1.EventList
			g.Expect(k8sClient.List(context.TODO(), &events, client.InNamespace(leftBehindBeforeStart.Namespace))).To(Succeed())
			g.Expect(events.Items).To(ContainElement(SatisfyAll(
				HaveField("InvolvedObject.Name", leftBehindBeforeStart.Name),
				HaveField("Reason", "ImageDefaultRestored"),
			)))
		}, timeout, interval).Should(Succeed())

		// What the operator writes carries its own name from now on: the
		// status it reports for this monitor is the first such write.
		Eventually(func(g Gomega) []string {
			obj := &pgpilotv1.PgpilotMonitor{}
			g.Expect(k8sClient.Get(context.TODO(), leftBehindBeforeStart, obj)).To(Succeed())
			managers := make([]string, 0, len(obj.ManagedFields))
			for _, entry := range obj.ManagedFields {
				managers = append(managers, entry.Manager)
			}
			return managers
		}, timeout, interval).Should(ContainElement("pgpilot-operator"))
	})

	Describe("left behind by an operator release that wrote the image into the spec", func() {
		var (
			legacy     client.Client
			asOperator client.Client
			recorder   *record.FakeRecorder
			migration  *StoredImageMigration
		)

		// leftBehind stores a monitor the way those releases left it: the
		// finalizer and the image block written in one request of theirs.
		leftBehind := func(name string) types.NamespacedName {
			GinkgoHelper()
			monitor := withSpec(minimalMonitor(ns, name), legacyImageBlock(), "image")
			monitor.SetFinalizers([]string{finalizerName})
			Expect(legacy.Create(context.TODO(), monitor)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(context.TODO(), monitor) })
			key := client.ObjectKeyFromObject(monitor)
			// The suite's manager reconciles the new monitor and writes its
			// status. The migration patches against the version it listed
			// and tries again later when that version is gone; these specs
			// run it once, so that write has to have landed.
			Eventually(func(g Gomega) {
				var stored pgpilotv1.PgpilotMonitor
				g.Expect(k8sClient.Get(context.TODO(), key, &stored)).To(Succeed())
				g.Expect(stored.Status.ObservedGeneration).To(Equal(stored.Generation))
			}, timeout, interval).Should(Succeed())
			return key
		}

		BeforeEach(func() {
			legacy = legacyOperatorClient(cfg)

			// The migration gets what the shipped ClusterRole allows and
			// reads straight from the API server, as it does in the manager.
			var err error
			asOperator, err = client.New(operatorConfig, client.Options{Scheme: scheme.Scheme})
			Expect(err).NotTo(HaveOccurred())
			recorder = record.NewFakeRecorder(100)
			// Limited to the namespace of the spec, so that what other
			// specs and the suite's own manager do with other monitors
			// cannot change the counts asserted here. The cluster-wide
			// list is what the manager itself runs (see the spec above).
			migration = &StoredImageMigration{
				Reader:   client.NewNamespacedClient(asOperator, ns),
				Client:   client.WithFieldOwner(asOperator, fieldOwner),
				Recorder: recorder,
			}
		})

		It("moves the monitor to the operator's release", func() {
			key := leftBehind("legacy")
			Eventually(collectorImage(k8sClient, key), timeout, interval).Should(Equal(legacyImage),
				"before the migration the stored tag decides")

			result, err := migration.Run(context.TODO())
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(MigrationResult{Migrated: 1}))

			Expect(storedImage(k8sClient, key)).To(Equal(map[string]any{"pullPolicy": "IfNotPresent"}),
				"tag and repository go, the rest of the block stays")
			Eventually(collectorImage(k8sClient, key), timeout, interval).Should(Equal(currentImage))

			Expect(drain(recorder)).To(ConsistOf(And(
				HavePrefix("Normal ImageDefaultRestored "),
				ContainSubstring("5.1.0"), ContainSubstring(currentImage),
			)))

			By("finding nothing left to do on a second pass")
			result, err = migration.Run(context.TODO())
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(MigrationResult{}))
			Expect(drain(recorder)).To(BeEmpty())
		})

		// The repository next to the tag is the old operator's as well,
		// unless somebody else has claimed it since.
		It("leaves the repository in place when another writer owns it", func() {
			key := leftBehind("repository-claimed")
			applied := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": pgpilotv1.GroupVersion.String(),
				"kind":       "PgpilotMonitor",
				"metadata":   map[string]any{"name": key.Name, "namespace": key.Namespace},
				"spec":       map[string]any{"image": map[string]any{"repository": builder.DefaultImageRepository}},
			}}
			Expect(k8sClient.Patch(context.TODO(), applied, client.Apply, //nolint:staticcheck // SA1019
				client.FieldOwner("gitops"), client.ForceOwnership)).To(Succeed())

			result, err := migration.Run(context.TODO())
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(MigrationResult{Migrated: 1}))
			Expect(storedImage(k8sClient, key)).To(Equal(map[string]any{
				"repository": builder.DefaultImageRepository, "pullPolicy": "IfNotPresent",
			}))
			Eventually(collectorImage(k8sClient, key), timeout, interval).Should(Equal(currentImage))
		})

		It("does not undo the tag when the user sets it again afterwards", func() {
			key := leftBehind("stays-on-purpose")
			result, err := migration.Run(context.TODO())
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Migrated).To(Equal(1))

			patch := []byte(`{"spec":{"image":{"tag":"5.1.0"}}}`)
			obj := &pgpilotv1.PgpilotMonitor{}
			obj.Name, obj.Namespace = key.Name, key.Namespace
			Expect(k8sClient.Patch(context.TODO(), obj, client.RawPatch(types.MergePatchType, patch))).To(Succeed())

			result, err = migration.Run(context.TODO())
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Migrated).To(BeZero())
			Expect(result.Kept).To(ConsistOf(HavePrefix(key.String() + " (tag set by ")))
			Expect(storedImage(k8sClient, key)).To(HaveKeyWithValue("tag", "5.1.0"))
			Eventually(collectorImage(k8sClient, key), timeout, interval).Should(Equal(legacyImage))
		})

		// Each of these has an image tag in its spec that is not, or not
		// provably, something the old operator alone put there. kept is what
		// the migration reports about the monitor; empty when the monitor
		// does not carry the legacy tag at all.
		DescribeTable("leaves alone a monitor",
			func(prepare func(name string) types.NamespacedName, want map[string]any, kept string) {
				key := prepare("not-ours")

				result, err := migration.Run(context.TODO())
				Expect(err).NotTo(HaveOccurred())
				Expect(result.Migrated).To(BeZero())
				if kept == "" {
					Expect(result.Kept).To(BeEmpty())
				} else {
					Expect(result.Kept).To(ConsistOf(And(HavePrefix(key.String()+" ("), ContainSubstring(kept))))
				}
				Expect(storedImage(k8sClient, key)).To(Equal(want))
				Expect(drain(recorder)).To(BeEmpty())
			},
			Entry("whose tag the user wrote",
				func(name string) types.NamespacedName {
					monitor := withSpec(minimalMonitor(ns, name), map[string]any{"tag": "5.1.0"}, "image")
					createForSpec(monitor)
					// The old operator then added its finalizer with a full
					// update, which does not take over fields it leaves as
					// they are.
					Eventually(func(g Gomega) {
						var typed pgpilotv1.PgpilotMonitor
						g.Expect(legacy.Get(context.TODO(), client.ObjectKeyFromObject(monitor), &typed)).To(Succeed())
						typed.Labels = map[string]string{"touched-by": "old-operator"}
						g.Expect(legacy.Update(context.TODO(), &typed)).To(Succeed())
					}, timeout, interval).Should(Succeed())
					return client.ObjectKeyFromObject(monitor)
				},
				map[string]any{"tag": "5.1.0", "pullPolicy": "IfNotPresent"}, "tag set by "),
			Entry("whose legacy tag the user applied again with server-side apply",
				func(name string) types.NamespacedName {
					key := leftBehind(name)
					applied := &unstructured.Unstructured{Object: map[string]any{
						"apiVersion": pgpilotv1.GroupVersion.String(),
						"kind":       "PgpilotMonitor",
						"metadata":   map[string]any{"name": key.Name, "namespace": key.Namespace},
						"spec":       map[string]any{"image": map[string]any{"tag": "5.1.0"}},
					}}
					Expect(k8sClient.Patch(context.TODO(), applied, client.Apply, //nolint:staticcheck // SA1019
						client.FieldOwner("gitops"), client.ForceOwnership)).To(Succeed())
					return key
				},
				legacyImageBlock(), "tag set by gitops"),
			// `kubectl apply` of a manifest whose tag is the stored one
			// changes no field, so the API server records no new owner. The
			// manifest kubectl keeps in an annotation is the only trace.
			Entry("whose legacy tag is in the manifest last applied with kubectl",
				func(name string) types.NamespacedName {
					key := leftBehind(name)
					obj := &pgpilotv1.PgpilotMonitor{}
					obj.Name, obj.Namespace = key.Name, key.Namespace
					patch := []byte(`{"metadata":{"annotations":{"kubectl.kubernetes.io/last-applied-configuration":` +
						`"{\"spec\":{\"image\":{\"tag\":\"5.1.0\"}}}"}},"spec":{"image":{"tag":"5.1.0"}}}`)
					Expect(k8sClient.Patch(context.TODO(), obj, client.RawPatch(types.MergePatchType, patch))).To(Succeed())
					return key
				},
				legacyImageBlock(), "tag set with kubectl apply"),
			Entry("annotated to keep its tag",
				func(name string) types.NamespacedName {
					key := leftBehind(name)
					obj := &pgpilotv1.PgpilotMonitor{}
					obj.Name, obj.Namespace = key.Name, key.Namespace
					patch := []byte(`{"metadata":{"annotations":{"pgpilot.io/keep-image-tag":"true"}}}`)
					Expect(k8sClient.Patch(context.TODO(), obj, client.RawPatch(types.MergePatchType, patch))).To(Succeed())
					return key
				},
				legacyImageBlock(), "annotation pgpilot.io/keep-image-tag"),
			Entry("whose repository the user changed since",
				func(name string) types.NamespacedName {
					key := leftBehind(name)
					obj := &pgpilotv1.PgpilotMonitor{}
					obj.Name, obj.Namespace = key.Name, key.Namespace
					patch := []byte(`{"spec":{"image":{"repository":"registry.example.com/pgwatch"}}}`)
					Expect(k8sClient.Patch(context.TODO(), obj, client.RawPatch(types.MergePatchType, patch))).To(Succeed())
					return key
				},
				map[string]any{"repository": "registry.example.com/pgwatch", "tag": "5.1.0", "pullPolicy": "IfNotPresent"},
				"its own image repository"),
			Entry("whose tag the user changed since",
				func(name string) types.NamespacedName {
					key := leftBehind(name)
					obj := &pgpilotv1.PgpilotMonitor{}
					obj.Name, obj.Namespace = key.Name, key.Namespace
					patch := []byte(`{"spec":{"image":{"tag":"5.3.0"}}}`)
					Expect(k8sClient.Patch(context.TODO(), obj, client.RawPatch(types.MergePatchType, patch))).To(Succeed())
					return key
				},
				map[string]any{"repository": builder.DefaultImageRepository, "tag": "5.3.0", "pullPolicy": "IfNotPresent"},
				""),
			// Only as long as the old operator never touched the monitor:
			// once it has added its finalizer, the two writers share one
			// entry of the record and cannot be told apart.
			Entry("written by another controller that is also called \"manager\", which the old operator never reconciled",
				func(name string) types.NamespacedName {
					// No finalizer in its request: the operator of this
					// suite adds it, under its own name.
					monitor := withSpec(minimalMonitor(ns, name), legacyImageBlock(), "image")
					Expect(legacy.Create(context.TODO(), monitor)).To(Succeed())
					DeferCleanup(func() { _ = k8sClient.Delete(context.TODO(), monitor) })
					key := client.ObjectKeyFromObject(monitor)
					Eventually(func(g Gomega) {
						var typed pgpilotv1.PgpilotMonitor
						g.Expect(k8sClient.Get(context.TODO(), key, &typed)).To(Succeed())
						g.Expect(typed.Finalizers).To(ContainElement(finalizerName))
					}, timeout, interval).Should(Succeed())
					return key
				},
				legacyImageBlock(), `a writer called "manager" that is not the operator`),
		)
	})
})

var _ = Describe("A preset the collector cannot gather", func() {
	var ns string

	BeforeEach(func() {
		ns = testNamespace("test-preset-")
		secret := &corev1.Secret{StringData: map[string]string{"username": "pgwatch", "password": "secret"}}
		secret.Name, secret.Namespace = "pg-creds", ns
		createForSpec(secret)
	})

	conditionOf := func(g Gomega, key types.NamespacedName, conditionType string) *metav1.Condition {
		var monitor pgpilotv1.PgpilotMonitor
		g.Expect(k8sClient.Get(context.TODO(), key, &monitor)).To(Succeed())
		for i := range monitor.Status.Conditions {
			if monitor.Status.Conditions[i].Type == conditionType {
				return &monitor.Status.Conditions[i]
			}
		}
		return nil
	}

	expectRefused := func(key types.NamespacedName, fragment string) {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			for _, conditionType := range []string{conditionConfigGenerated, "Ready"} {
				cond := conditionOf(g, key, conditionType)
				g.Expect(cond).NotTo(BeNil(), "condition %s", conditionType)
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse), "condition %s", conditionType)
				g.Expect(cond.Reason).To(Equal("InvalidPreset"))
				g.Expect(cond.Message).To(ContainSubstring(fragment))
				g.Expect(cond.Message).To(ContainSubstring("available: aiven, aurora, azure, basic,"))
			}
			var monitor pgpilotv1.PgpilotMonitor
			g.Expect(k8sClient.Get(context.TODO(), key, &monitor)).To(Succeed())
			g.Expect(monitor.Status.Ready).To(BeFalse())
			g.Expect(monitor.Status.ObservedGeneration).To(Equal(monitor.Generation))
		}, timeout, interval).Should(Succeed())

		Eventually(func(g Gomega) {
			var events corev1.EventList
			g.Expect(k8sClient.List(context.TODO(), &events, client.InNamespace(key.Namespace))).To(Succeed())
			g.Expect(events.Items).To(ContainElement(SatisfyAll(
				HaveField("InvolvedObject.Name", key.Name),
				HaveField("Type", corev1.EventTypeWarning),
				HaveField("Reason", "InvalidPreset"),
				HaveField("Message", ContainSubstring(fragment)),
			)))
		}, timeout, interval).Should(Succeed())
	}

	// exist reports whether anything was generated for the monitor.
	exist := func(key types.NamespacedName) bool {
		name := types.NamespacedName{Name: builder.ResourceName(key.Name), Namespace: key.Namespace}
		config := types.NamespacedName{Name: name.Name + "-config", Namespace: key.Namespace}
		return k8sClient.Get(context.TODO(), name, &appsv1.Deployment{}) == nil ||
			k8sClient.Get(context.TODO(), config, &corev1.ConfigMap{}) == nil
	}

	DescribeTable("is reported in the status instead of starting a collector with nothing to collect",
		func(metrics map[string]any, fragment string) {
			monitor := withSpec(minimalMonitor(ns, "bad-preset"), metrics, "metrics")
			createForSpec(monitor)
			key := client.ObjectKeyFromObject(monitor)

			expectRefused(key, fragment)
			Consistently(func() bool { return exist(key) }, 2*time.Second, interval).Should(BeFalse(),
				"nothing may be generated for a monitor without a usable configuration")
		},
		Entry("a name pgwatch does not know",
			map[string]any{"preset": "exhaustiv"}, `"exhaustiv" is not a preset of pgwatch 6.2.0`),
		Entry("a name pgwatch does not know, next to custom metrics",
			map[string]any{"preset": "exhaustiv", "custom": []any{metricNamed("queue_depth")}},
			`"exhaustiv" is not a preset of pgwatch 6.2.0`),
		Entry("a pgwatch preset for Prometheus-type sources",
			map[string]any{"preset": "patroni"}, `"patroni" is a pgwatch preset for Prometheus-type sources`),
		Entry("a pgwatch preset for Prometheus-type sources, next to custom metrics",
			map[string]any{"preset": "patroni", "custom": []any{metricNamed("queue_depth")}},
			`"patroni" is a pgwatch preset for Prometheus-type sources`),
	)

	It("recovers as soon as the preset is corrected, and reports a preset broken later", func() {
		monitor := withSpec(minimalMonitor(ns, "typo"), "standrad", "metrics", "preset")
		createForSpec(monitor)
		key := client.ObjectKeyFromObject(monitor)
		expectRefused(key, `"standrad" is not a preset`)

		setPreset := func(preset string) {
			GinkgoHelper()
			obj := &pgpilotv1.PgpilotMonitor{}
			obj.Name, obj.Namespace = key.Name, key.Namespace
			patch := []byte(`{"spec":{"metrics":{"preset":"` + preset + `"}}}`)
			Expect(k8sClient.Patch(context.TODO(), obj, client.RawPatch(types.MergePatchType, patch))).To(Succeed())
		}

		setPreset("standard")
		Eventually(func(g Gomega) {
			cond := conditionOf(g, key, conditionConfigGenerated)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(exist(key)).To(BeTrue(), "the collector must be created now")
		}, timeout, interval).Should(Succeed())

		// envtest runs no Deployment controller: report the collector ready
		// in its place, so that the monitor has a Ready=True to lose.
		Eventually(func(g Gomega) {
			var dep appsv1.Deployment
			g.Expect(k8sClient.Get(context.TODO(),
				types.NamespacedName{Name: builder.ResourceName(key.Name), Namespace: key.Namespace}, &dep)).To(Succeed())
			dep.Status.ObservedGeneration = dep.Generation
			dep.Status.Replicas, dep.Status.UpdatedReplicas = 1, 1
			dep.Status.ReadyReplicas, dep.Status.AvailableReplicas = 1, 1
			g.Expect(k8sClient.Status().Update(context.TODO(), &dep)).To(Succeed())
		}, timeout, interval).Should(Succeed())
		Eventually(func(g Gomega) {
			var m pgpilotv1.PgpilotMonitor
			g.Expect(k8sClient.Get(context.TODO(), key, &m)).To(Succeed())
			g.Expect(m.Status.Ready).To(BeTrue())
		}, timeout, interval).Should(Succeed())

		setPreset("patroni")
		expectRefused(key, `"patroni" is a pgwatch preset for Prometheus-type sources`)
		Expect(exist(key)).To(BeTrue(), "the collector of the last valid spec keeps running")
	})
})

func managedBy(manager string, operation metav1.ManagedFieldsOperationType, subresource, fields string) metav1.ManagedFieldsEntry {
	return metav1.ManagedFieldsEntry{
		Manager: manager, Operation: operation, Subresource: subresource, APIVersion: pgpilotv1.GroupVersion.String(),
		FieldsType: "FieldsV1", FieldsV1: metav1.NewFieldsV1(fields),
	}
}

func TestLegacyOwnedImageFields(t *testing.T) {
	const (
		finalizer  = `"f:metadata":{"f:finalizers":{".":{},"v:\"pgpilot.io/finalizer\"":{}}}`
		imageBlock = `"f:spec":{"f:image":{".":{},"f:pullPolicy":{},"f:repository":{},"f:tag":{}}}`
		tagOnly    = `"f:spec":{"f:image":{"f:tag":{}}}`
		repoOnly   = `"f:spec":{"f:image":{"f:repository":{}}}`
		database   = `"f:spec":{"f:database":{"f:host":{}}}`
		update     = metav1.ManagedFieldsOperationUpdate
		apply      = metav1.ManagedFieldsOperationApply
	)
	cases := map[string]struct {
		entries []metav1.ManagedFieldsEntry
		want    legacyOwnership
	}{
		"written by the old operator together with its finalizer": {
			[]metav1.ManagedFieldsEntry{managedBy("manager", update, "", "{"+finalizer+","+imageBlock+"}")},
			legacyOwnership{tag: true, repository: true},
		},
		"other fields of the spec owned by the user": {
			[]metav1.ManagedFieldsEntry{
				managedBy("kubectl-client-side-apply", update, "", "{"+database+"}"),
				managedBy("manager", update, "", "{"+finalizer+","+imageBlock+"}"),
				managedBy("manager", update, "status", `{"f:status":{"f:ready":{}}}`),
			},
			legacyOwnership{tag: true, repository: true},
		},
		"the old operator owns the tag but the user owns the repository": {
			[]metav1.ManagedFieldsEntry{
				managedBy("manager", update, "", "{"+finalizer+","+tagOnly+"}"),
				managedBy("kubectl-edit", update, "", "{"+repoOnly+"}"),
			},
			legacyOwnership{tag: true},
		},
		"repository shared with another manager": {
			[]metav1.ManagedFieldsEntry{
				managedBy("manager", update, "", "{"+finalizer+","+imageBlock+"}"),
				managedBy("gitops", apply, "", "{"+repoOnly+"}"),
			},
			legacyOwnership{tag: true},
		},
		// A default the API server filled in on its own, as on a status
		// update under the old CRD after a rollback: nobody asked for it.
		"tag owned by nobody, in an object whose spec has owners": {
			[]metav1.ManagedFieldsEntry{
				managedBy("kubectl-client-side-apply", update, "", "{"+database+"}"),
				managedBy("manager", update, "", "{"+finalizer+"}"),
			},
			legacyOwnership{tag: true, repository: true},
		},
		"tag owned by nobody, repository by the user": {
			[]metav1.ManagedFieldsEntry{managedBy("helm", update, "", "{"+repoOnly+"}")},
			legacyOwnership{tag: true},
		},
		"an entry without fields next to the old operator's": {
			[]metav1.ManagedFieldsEntry{
				{Manager: "before-first-apply", Operation: update},
				managedBy("manager", update, "", "{"+finalizer+","+imageBlock+"}"),
			},
			legacyOwnership{tag: true, repository: true},
		},

		"no managedFields at all (a cached object, or one restored from a backup)": {
			nil, legacyOwnership{why: "no ownership record"},
		},
		"managedFields that cover the status only": {
			[]metav1.ManagedFieldsEntry{managedBy("manager", update, "status", `{"f:status":{"f:ready":{}}}`)},
			legacyOwnership{why: "no ownership record"},
		},
		"tag owned by the user": {
			[]metav1.ManagedFieldsEntry{
				managedBy("manager", update, "", "{"+finalizer+"}"),
				managedBy("kubectl-create", update, "", "{"+imageBlock+"}"),
			},
			legacyOwnership{why: "kubectl-create"},
		},
		"tag shared with another manager": {
			[]metav1.ManagedFieldsEntry{
				managedBy("manager", update, "", "{"+finalizer+","+imageBlock+"}"),
				managedBy("gitops", apply, "", "{"+tagOnly+"}"),
			},
			legacyOwnership{why: "gitops"},
		},
		"a manager of the same name that does not own the operator's finalizer": {
			[]metav1.ManagedFieldsEntry{
				managedBy("manager", update, "", "{"+imageBlock+"}"),
				managedBy(fieldOwner, update, "", "{"+finalizer+"}"),
			},
			legacyOwnership{why: `a writer called "manager"`},
		},
		"applied, not updated, by something called manager": {
			[]metav1.ManagedFieldsEntry{managedBy("manager", apply, "", "{"+finalizer+","+imageBlock+"}")},
			legacyOwnership{why: "manager"},
		},
		"written through a subresource": {
			[]metav1.ManagedFieldsEntry{managedBy("manager", update, "scale", "{"+finalizer+","+imageBlock+"}")},
			legacyOwnership{why: "manager"},
		},
		"written by this operator under its own name": {
			[]metav1.ManagedFieldsEntry{managedBy(fieldOwner, update, "", "{"+finalizer+","+imageBlock+"}")},
			legacyOwnership{why: fieldOwner},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			monitor := &pgpilotv1.PgpilotMonitor{}
			monitor.ManagedFields = tc.entries
			got, err := legacyOwnedImageFields(monitor)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.tag != tc.want.tag || got.repository != tc.want.repository {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
			// The reason ends up in the operator's log, next to the monitor
			// that was left alone.
			if !strings.Contains(got.why, tc.want.why) || (tc.want.why == "") != (got.why == "") {
				t.Errorf("reason %q, want it to contain %q", got.why, tc.want.why)
			}
		})
	}

	t.Run("unreadable managedFields are an error, not a reason to migrate or to skip silently", func(t *testing.T) {
		monitor := &pgpilotv1.PgpilotMonitor{}
		monitor.ManagedFields = []metav1.ManagedFieldsEntry{managedBy("manager", update, "", `{"f:spec":`)}
		if _, err := legacyOwnedImageFields(monitor); err == nil {
			t.Error("want an error")
		}
	})
}

func TestHasFieldPath(t *testing.T) {
	var fields map[string]any
	if err := json.Unmarshal([]byte(`{"f:spec":{"f:image":{"f:tag":{}},"f:leaf":"x"}}`), &fields); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		path []string
		want bool
	}{
		"a leaf":                         {[]string{"f:spec", "f:image", "f:tag"}, true},
		"an inner node":                  {[]string{"f:spec", "f:image"}, true},
		"a missing leaf":                 {[]string{"f:spec", "f:image", "f:repository"}, false},
		"a missing inner node":           {[]string{"f:metadata", "f:finalizers"}, false},
		"through a value that is no map": {[]string{"f:spec", "f:leaf", "f:tag"}, false},
		"the empty path":                 {nil, true},
	}
	for name, tc := range cases {
		if got := hasFieldPath(fields, tc.path...); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

func TestLastAppliedSetsTag(t *testing.T) {
	cases := map[string]struct {
		annotation *string
		want       bool
	}{
		"never applied with kubectl":        {nil, false},
		"manifest without an image":         {new(`{"spec":{"database":{"host":"pg"}}}`), false},
		"manifest with another image field": {new(`{"spec":{"image":{"pullPolicy":"Always"}}}`), false},
		"manifest with the tag":             {new(`{"spec":{"image":{"tag":"5.1.0"}}}`), true},
		"manifest with another tag":         {new(`{"spec":{"image":{"tag":"5.3.0"}}}`), true},
		// Better to keep a tag than to remove one on a guess.
		"an annotation that is not JSON": {new(`{"spec":`), true},
	}
	for name, tc := range cases {
		monitor := &pgpilotv1.PgpilotMonitor{}
		if tc.annotation != nil {
			monitor.Annotations = map[string]string{"kubectl.kubernetes.io/last-applied-configuration": *tc.annotation}
		}
		if got := lastAppliedSetsTag(monitor); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

// legacyMonitor is a monitor as the old operator left it.
func legacyMonitor(name string) *pgpilotv1.PgpilotMonitor {
	monitor := &pgpilotv1.PgpilotMonitor{}
	monitor.Name, monitor.Namespace = name, "default"
	monitor.Finalizers = []string{finalizerName}
	monitor.Spec.Image = pgpilotv1.ImageSpec{Repository: builder.DefaultImageRepository, Tag: legacyImageTag}
	monitor.ManagedFields = []metav1.ManagedFieldsEntry{managedBy("manager", metav1.ManagedFieldsOperationUpdate, "",
		`{"f:metadata":{"f:finalizers":{"v:\"pgpilot.io/finalizer\"":{}}},"f:spec":{"f:image":{"f:repository":{},"f:tag":{}}}}`)}
	return monitor
}

func migrationScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := pgpilotv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// monitorLister stands in for the API server's list of monitors. The fake
// client cannot: it does not keep the managedFields the migration reads.
// It serves pages of pageSize monitors (all of them when zero), with the
// index of the next monitor as continue token.
type monitorLister struct {
	client.Reader
	monitors []*pgpilotv1.PgpilotMonitor
	pageSize int
	// fail is consulted before every List.
	fail func(call int) error
	// calls counts the List requests; limits and tokens are what they asked.
	calls  int
	limits []int64
	tokens []string
}

func (l *monitorLister) List(_ context.Context, list client.ObjectList, opts ...client.ListOption) error {
	l.calls++
	if l.fail != nil {
		if err := l.fail(l.calls); err != nil {
			return err
		}
	}
	options := &client.ListOptions{}
	for _, o := range opts {
		o.ApplyToList(options)
	}
	l.limits = append(l.limits, options.Limit)
	l.tokens = append(l.tokens, options.Continue)

	start := 0
	if options.Continue != "" {
		var err error
		if start, err = strconv.Atoi(options.Continue); err != nil {
			return err
		}
	}
	end := len(l.monitors)
	if l.pageSize > 0 && start+l.pageSize < end {
		end = start + l.pageSize
	}
	out := list.(*pgpilotv1.PgpilotMonitorList)
	out.Items, out.Continue = nil, ""
	for _, m := range l.monitors[start:end] {
		out.Items = append(out.Items, *m.DeepCopy())
	}
	if end < len(l.monitors) {
		out.Continue = strconv.Itoa(end)
	}
	return nil
}

// patchRecorder is a client whose Patch calls fn instead of the API server.
// When fn accepts the patch the object comes back without its tag, as the API
// server returns it under the current CRD.
func patchRecorder(t *testing.T, fn func(obj client.Object, patch client.Patch) error) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(migrationScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, patch client.Patch, _ ...client.PatchOption) error {
				if err := fn(obj, patch); err != nil {
					return err
				}
				obj.(*pgpilotv1.PgpilotMonitor).Spec.Image.Tag = ""
				return nil
			},
		}).Build()
}

func patchNothing(client.Object, client.Patch) error { return nil }

func monitors(m ...*pgpilotv1.PgpilotMonitor) []*pgpilotv1.PgpilotMonitor { return m }

func TestStoredImageMigration_RunCarriesOnPastAFailure(t *testing.T) {
	var patched []string
	c := patchRecorder(t, func(obj client.Object, patch client.Patch) error {
		if patch.Type() != types.JSONPatchType {
			t.Errorf("patch type %s: the migration must not send the object back", patch.Type())
		}
		if obj.GetName() == "b" {
			return errors.New("admission said no")
		}
		patched = append(patched, obj.GetName())
		return nil
	})
	reader := &monitorLister{monitors: monitors(legacyMonitor("a"), legacyMonitor("b"), legacyMonitor("c"))}

	result, err := (&StoredImageMigration{Reader: reader, Client: c}).Run(context.Background())
	if result.Migrated != 2 || !slices.Equal(patched, []string{"a", "c"}) {
		t.Errorf("migrated %d (%v), want the two monitors that could be patched", result.Migrated, patched)
	}
	if err == nil || !strings.Contains(err.Error(), "default/b") || !strings.Contains(err.Error(), "admission said no") {
		t.Errorf("the error must name the monitor that failed, got %v", err)
	}
}

// A monitor deleted between the list and the patch is nothing to report.
func TestStoredImageMigration_AMonitorDeletedMeanwhileIsNoError(t *testing.T) {
	c := patchRecorder(t, func(obj client.Object, _ client.Patch) error {
		if obj.GetName() == "gone" {
			return apierrors.NewNotFound(pgpilotv1.GroupVersion.WithResource("pgpilotmonitors").GroupResource(), "gone")
		}
		return nil
	})
	reader := &monitorLister{monitors: monitors(legacyMonitor("gone"), legacyMonitor("there"))}

	result, err := (&StoredImageMigration{Reader: reader, Client: c}).Run(context.Background())
	if err != nil || result.Migrated != 1 || len(result.Kept) != 0 {
		t.Errorf("got %+v, %v; want one migrated monitor and no error", result, err)
	}
}

// The list is read page by page: a cluster can hold more monitors than one
// request returns.
func TestStoredImageMigration_RunReadsEveryPage(t *testing.T) {
	var patched []string
	c := patchRecorder(t, func(obj client.Object, _ client.Patch) error {
		patched = append(patched, obj.GetName())
		return nil
	})
	reader := &monitorLister{
		monitors: monitors(legacyMonitor("a"), legacyMonitor("b"), legacyMonitor("c")),
		pageSize: 1,
	}

	result, err := (&StoredImageMigration{Reader: reader, Client: c, PageSize: 1}).Run(context.Background())
	if err != nil || result.Migrated != 3 || !slices.Equal(patched, []string{"a", "b", "c"}) {
		t.Errorf("got %+v, %v, patched %v; want all three monitors", result, err, patched)
	}
	if !slices.Equal(reader.tokens, []string{"", "1", "2"}) {
		t.Errorf("continue tokens sent: %q, want each page asked for once, in order", reader.tokens)
	}
	if !slices.Equal(reader.limits, []int64{1, 1, 1}) {
		t.Errorf("limits sent: %v, want the page size on every request", reader.limits)
	}

	t.Run("with the default page size", func(t *testing.T) {
		reader := &monitorLister{monitors: monitors(legacyMonitor("a"))}
		if _, err := (&StoredImageMigration{Reader: reader, Client: c}).Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(reader.limits, []int64{200}) {
			t.Errorf("limits sent: %v, want 200", reader.limits)
		}
	})
}

// The patch must only succeed against the object the decision was made on.
func TestStoredImageMigration_PatchIsConditional(t *testing.T) {
	var body string
	c := patchRecorder(t, func(obj client.Object, patch client.Patch) error {
		data, err := patch.Data(obj)
		body = string(data)
		return err
	})
	const (
		version = `{"op":"test","path":"/metadata/resourceVersion","value":"4711"},`
		tag     = `{"op":"test","path":"/spec/image/tag","value":"5.1.0"},{"op":"remove","path":"/spec/image/tag"}`
		repo    = `,{"op":"test","path":"/spec/image/repository","value":"cybertecpostgresql/pgwatch"},` +
			`{"op":"remove","path":"/spec/image/repository"}`
	)

	t.Run("tag and repository of the old operator", func(t *testing.T) {
		monitor := legacyMonitor("a")
		monitor.ResourceVersion = "4711"
		reader := &monitorLister{monitors: monitors(monitor)}
		if result, err := (&StoredImageMigration{Reader: reader, Client: c}).Run(context.Background()); result.Migrated != 1 || err != nil {
			t.Fatalf("got %+v, %v", result, err)
		}
		if want := "[" + version + tag + repo + "]"; body != want {
			t.Errorf("patch:\n got  %s\n want %s", body, want)
		}
	})

	// A repository somebody else has claimed is theirs to keep, even when it
	// is the default one.
	t.Run("repository owned by another writer", func(t *testing.T) {
		monitor := legacyMonitor("a")
		monitor.ResourceVersion = "4711"
		monitor.ManagedFields = append(monitor.ManagedFields,
			managedBy("gitops", metav1.ManagedFieldsOperationApply, "", `{"f:spec":{"f:image":{"f:repository":{}}}}`))
		reader := &monitorLister{monitors: monitors(monitor)}
		if result, err := (&StoredImageMigration{Reader: reader, Client: c}).Run(context.Background()); result.Migrated != 1 || err != nil {
			t.Fatalf("got %+v, %v", result, err)
		}
		if want := "[" + version + tag + "]"; body != want {
			t.Errorf("patch:\n got  %s\n want %s", body, want)
		}
	})
}

func TestStoredImageMigration_LeavesAloneAndReportsWhatItMustNotTouch(t *testing.T) {
	deleting := legacyMonitor("being-deleted")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now

	otherTag := legacyMonitor("other-tag")
	otherTag.Spec.Image.Tag = "5.3.0"

	noTag := legacyMonitor("no-tag")
	noTag.Spec.Image.Tag = ""

	mirror := legacyMonitor("mirror")
	mirror.Spec.Image.Repository = "registry.example.com/pgwatch"

	userOwned := legacyMonitor("user-owned")
	userOwned.ManagedFields[0].Manager = "kubectl-client-side-apply"

	annotated := legacyMonitor("annotated")
	annotated.Annotations = map[string]string{AnnotationKeepImageTag: "true"}

	applied := legacyMonitor("applied")
	applied.Annotations = map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": `{"spec":{"image":{"tag":"5.1.0"}}}`,
	}

	c := patchRecorder(t, func(obj client.Object, _ client.Patch) error {
		t.Errorf("%s must not be patched", obj.GetName())
		return nil
	})
	reader := &monitorLister{monitors: monitors(deleting, otherTag, noTag, mirror, userOwned, annotated, applied)}
	result, err := (&StoredImageMigration{Reader: reader, Client: c}).Run(context.Background())
	if err != nil || result.Migrated != 0 {
		t.Errorf("got %+v, %v", result, err)
	}
	// Only monitors that still carry the legacy tag are worth a line in the
	// log; each with the reason it was left alone.
	want := []string{
		"default/mirror (its own image repository)",
		"default/user-owned (tag set by kubectl-client-side-apply)",
		"default/annotated (annotation pgpilot.io/keep-image-tag)",
		"default/applied (tag set with kubectl apply)",
	}
	if !slices.Equal(result.Kept, want) {
		t.Errorf("kept:\n got  %q\n want %q", result.Kept, want)
	}

	t.Run("the annotation only counts when it says true", func(t *testing.T) {
		monitor := legacyMonitor("annotated-false")
		monitor.Annotations = map[string]string{AnnotationKeepImageTag: "false"}
		reader := &monitorLister{monitors: monitors(monitor)}
		result, err := (&StoredImageMigration{Reader: reader, Client: patchRecorder(t, patchNothing)}).Run(context.Background())
		if err != nil || result.Migrated != 1 {
			t.Errorf("got %+v, %v", result, err)
		}
	})

	// A spec without a repository uses the default one, like a spec that
	// spells it out.
	t.Run("an empty repository is the default repository", func(t *testing.T) {
		monitor := legacyMonitor("no-repository")
		monitor.Spec.Image.Repository = ""
		reader := &monitorLister{monitors: monitors(monitor)}
		result, err := (&StoredImageMigration{Reader: reader, Client: patchRecorder(t, patchNothing)}).Run(context.Background())
		if err != nil || result.Migrated != 1 {
			t.Errorf("got %+v, %v", result, err)
		}
	})
}

// While the cluster still serves the old CRD the API server puts the default
// tag straight back. Reporting that as a migration would be a lie, and the
// same is true for every other monitor: the pass stops there.
func TestStoredImageMigration_FailsWhenTheOldCRDPutsTheTagBack(t *testing.T) {
	patches := 0
	c := fake.NewClientBuilder().WithScheme(migrationScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			// The object comes back as it went in: tag and all.
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				patches++
				return nil
			},
		}).Build()
	recorder := record.NewFakeRecorder(10)
	reader := &monitorLister{monitors: monitors(legacyMonitor("a"), legacyMonitor("b"))}

	result, err := (&StoredImageMigration{Reader: reader, Client: c, Recorder: recorder}).Run(context.Background())
	if !errors.Is(err, errTagStillDefaulted) || !strings.Contains(err.Error(), "install the CRDs of this operator release") {
		t.Errorf("want an error that says the CRD has to be upgraded, got %v", err)
	}
	if result.Migrated != 0 {
		t.Errorf("counted %d migrated monitors although nothing changed", result.Migrated)
	}
	if patches != 1 {
		t.Errorf("%d monitors patched, want the pass to stop at the first", patches)
	}
	if events := drain(recorder); len(events) != 0 {
		t.Errorf("recorded %q although nothing changed", events)
	}
}

func TestStoredImageMigration_RecordsAnEventPerMigratedMonitor(t *testing.T) {
	recorder := record.NewFakeRecorder(10)
	reader := &monitorLister{monitors: monitors(legacyMonitor("a"))}
	m := &StoredImageMigration{Reader: reader, Client: patchRecorder(t, patchNothing), Recorder: recorder}
	if _, err := m.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := drain(recorder)
	if len(events) != 1 || !strings.HasPrefix(events[0], "Normal ImageDefaultRestored Removed image tag 5.1.0") ||
		!strings.HasSuffix(events[0], "cybertecpostgresql/pgwatch:"+builder.PgwatchVersion) {
		t.Errorf("events: %q", events)
	}
}

func TestStoredImageMigration_StartRetriesAndNeverFailsTheManager(t *testing.T) {
	backoff := wait.Backoff{Duration: time.Millisecond, Factor: 1, Steps: 4}

	t.Run("retries until a pass succeeds", func(t *testing.T) {
		reader := &monitorLister{
			monitors: monitors(legacyMonitor("a")),
			fail: func(call int) error {
				if call < 3 {
					return errors.New("API server not ready")
				}
				return nil
			},
		}
		m := &StoredImageMigration{Reader: reader, Client: patchRecorder(t, patchNothing), Backoff: backoff}
		if err := m.Start(context.Background()); err != nil {
			t.Fatalf("Start returned %v", err)
		}
		if reader.calls != 3 {
			t.Errorf("listed %d times, want 3 (two failures, one success, then no more)", reader.calls)
		}
	})

	t.Run("retries a pass in which a monitor could not be patched", func(t *testing.T) {
		patches := 0
		c := patchRecorder(t, func(client.Object, client.Patch) error {
			patches++
			if patches == 1 {
				return errors.New("conflict")
			}
			return nil
		})
		reader := &monitorLister{monitors: monitors(legacyMonitor("a"))}
		if err := (&StoredImageMigration{Reader: reader, Client: c, Backoff: backoff}).Start(context.Background()); err != nil {
			t.Fatalf("Start returned %v", err)
		}
		if patches != 2 || reader.calls != 2 {
			t.Errorf("patched %d times in %d passes, want a second pass after the failed patch", patches, reader.calls)
		}
	})

	t.Run("gives up without an error", func(t *testing.T) {
		reader := &monitorLister{fail: func(int) error { return errors.New("forbidden") }}
		m := &StoredImageMigration{Reader: reader, Client: patchRecorder(t, patchNothing), Backoff: backoff}
		if err := m.Start(context.Background()); err != nil {
			t.Errorf("a failed migration must not stop the operator, got %v", err)
		}
		if reader.calls != backoff.Steps {
			t.Errorf("listed %d times, want %d", reader.calls, backoff.Steps)
		}
	})

	t.Run("stops when the manager stops", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		reader := &monitorLister{fail: func(int) error { return errors.New("never ready") }}
		m := &StoredImageMigration{
			Reader: reader, Client: patchRecorder(t, patchNothing),
			Backoff: wait.Backoff{Duration: time.Hour, Factor: 1, Steps: 5},
		}
		done := make(chan error, 1)
		go func() { done <- m.Start(ctx) }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("got %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Start did not return after the context was cancelled")
		}
	})

	if !(&StoredImageMigration{}).NeedLeaderElection() {
		t.Error("only the leader may write")
	}
}

// The schedule the manager runs with: long enough to outlast an API server
// that is slow to come up, short enough not to keep failing for hours.
func TestStoredImageMigration_DefaultRetrySchedule(t *testing.T) {
	b := defaultImageMigrationBackoff
	if b.Steps != 6 || b.Duration != 5*time.Second || b.Factor != 2 {
		t.Errorf("default backoff is %+v, want six attempts starting at 5s and doubling", b)
	}
	var total time.Duration
	step := b.Duration
	for range b.Steps - 1 {
		total += step
		step = time.Duration(float64(step) * b.Factor)
	}
	if total < 2*time.Minute || total > 5*time.Minute {
		t.Errorf("the attempts span %s, want a few minutes", total)
	}

	// A migration without a Backoff of its own uses it: with a reader that
	// always fails and a context that ends, Start must have tried at once
	// and still be waiting for its second attempt.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	reader := &monitorLister{fail: func(int) error { return errors.New("never ready") }}
	if err := (&StoredImageMigration{Reader: reader, Client: patchRecorder(t, patchNothing)}).Start(ctx); err != nil {
		t.Fatalf("Start returned %v", err)
	}
	if reader.calls != 1 {
		t.Errorf("listed %d times within 300ms, want one attempt and a five second wait", reader.calls)
	}
}
