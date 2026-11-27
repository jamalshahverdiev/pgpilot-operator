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
	"errors"
	"fmt"
	"slices"
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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

// The ConfigMap of a monitor is named after the configuration it holds, so
// the tests do not know its name: they ask the collector which one it mounts,
// or the API server which ones the monitor has.

// mountedConfigMap returns the ConfigMap the pod template of a monitor's
// collector mounts.
func mountedConfigMap(c client.Reader, monitor types.NamespacedName) (*corev1.ConfigMap, error) {
	var dep appsv1.Deployment
	key := types.NamespacedName{Name: builder.ResourceName(monitor.Name), Namespace: monitor.Namespace}
	if err := c.Get(context.TODO(), key, &dep); err != nil {
		return nil, err
	}
	for _, volume := range dep.Spec.Template.Spec.Volumes {
		if volume.ConfigMap == nil {
			continue
		}
		var cm corev1.ConfigMap
		err := c.Get(context.TODO(), types.NamespacedName{Name: volume.ConfigMap.Name, Namespace: monitor.Namespace}, &cm)
		return &cm, err
	}
	return nil, fmt.Errorf("the collector of %s mounts no ConfigMap", monitor)
}

// configMapNames lists the ConfigMaps generated for a monitor, sorted.
func configMapNames(c client.Reader, monitor types.NamespacedName) ([]string, error) {
	var list corev1.ConfigMapList
	err := c.List(context.TODO(), &list, client.InNamespace(monitor.Namespace),
		client.MatchingLabels{builder.LabelMonitorName: monitor.Name})
	names := make([]string, 0, len(list.Items))
	for _, cm := range list.Items {
		names = append(names, cm.Name)
	}
	slices.Sort(names)
	return names, err
}

// configMapNameFor returns the name the operator gives the ConfigMap of a
// monitor that is yet to be created, with the defaults the API server fills
// in and without libraries.
func configMapNameFor(obj *unstructured.Unstructured) string {
	GinkgoHelper()
	defaulted := obj.DeepCopy()
	Expect(k8sClient.Create(context.TODO(), defaulted, client.DryRunAll)).To(Succeed())
	var monitor pgpilotv1.PgpilotMonitor
	Expect(runtime.DefaultUnstructuredConverter.FromUnstructured(defaulted.Object, &monitor)).To(Succeed())
	cm, _, err := builder.BuildConfigMap(&monitor, builder.MergeMetrics(monitor.Spec.Metrics, nil))
	Expect(err).NotTo(HaveOccurred())
	return cm.Name
}

// unitConfigMapName is the name of the ConfigMap of a monitor of the unit
// tests, without libraries.
func unitConfigMapName(t *testing.T, monitor *pgpilotv1.PgpilotMonitor) string {
	t.Helper()
	cm, _, err := builder.BuildConfigMap(monitor, builder.MergeMetrics(monitor.Spec.Metrics, nil))
	if err != nil {
		t.Fatal(err)
	}
	return cm.Name
}

func (h *reconcileHarness) storedConfigMap() *corev1.ConfigMap {
	h.t.Helper()
	cm, err := mountedConfigMap(h.client, types.NamespacedName{Namespace: "team", Name: "db"})
	if err != nil {
		h.t.Fatal(err)
	}
	return cm
}

// earlierConfigMap is a ConfigMap of the unit monitor, with the labels the
// operator gives its objects, controlled by the monitor or by nobody.
func earlierConfigMap(name string, owner *pgpilotv1.PgpilotMonitor) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{Data: map[string]string{builder.SourcesKey: "- name: db\n"}}
	cm.Name, cm.Namespace, cm.Labels = name, "team", builder.SystemLabels("db")
	if owner != nil {
		cm.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: pgpilotv1.GroupVersion.String(), Kind: "PgpilotMonitor", Name: owner.Name, UID: owner.UID,
			Controller: new(true),
		}}
	}
	return cm
}

func (h *reconcileHarness) configMaps() []string {
	h.t.Helper()
	names, err := configMapNames(h.client, types.NamespacedName{Namespace: "team", Name: "db"})
	if err != nil {
		h.t.Fatal(err)
	}
	return names
}

func configMapRemovals(h *reconcileHarness) []string {
	var out []string
	for _, e := range drain(h.recorder) {
		if what, is := strings.CutPrefix(e, "Normal ConfigMapRemoved "); is {
			out = append(out, what)
		}
	}
	return out
}

// Every configuration has a ConfigMap of its own, and the pod template names
// the one it was made for. The pod of the previous template is kept until
// its successor is available, for good when that one cannot start, and it
// reads its files again every two minutes: it must go on finding what it was
// started with.
func TestReconcile_EachConfigurationHasItsOwnConfigMap(t *testing.T) {
	changed := func(h *reconcileHarness) {
		h.t.Helper()
		stored := h.monitor()
		stored.Spec.Database.Host = "pg-new.example.com"
		stored.Generation++
		if err := h.client.Update(context.Background(), stored); err != nil {
			h.t.Fatal(err)
		}
	}

	t.Run("the collector mounts the ConfigMap of its configuration", func(t *testing.T) {
		h := healthyHarness(t)
		first := h.storedConfigMap()
		if want := unitConfigMapName(t, unitMonitor()); first.Name != want || !builder.IsConfigMapName("db", first.Name) {
			t.Errorf("the ConfigMap is called %q, want %q", first.Name, want)
		}
		if got := h.configMaps(); !slices.Equal(got, []string{first.Name}) {
			t.Errorf("ConfigMaps %v", got)
		}
		if hash := h.monitor().Status.ConfigHash; !strings.HasSuffix(first.Name, "-"+hash[:10]) {
			t.Errorf("the ConfigMap %q is not named after the hash of the status, %q", first.Name, hash)
		}
	})

	t.Run("a changed configuration leaves the one before it alone until the rollout is complete", func(t *testing.T) {
		h := healthyHarness(t)
		first := h.storedConfigMap()

		changed(h)
		h.deploymentStatus = starting
		for range 3 {
			if _, err := h.reconcile(); err != nil {
				t.Fatal(err)
			}
			if len(h.deleted) != 0 {
				t.Fatalf("deleted %v while the pod of the previous template may still run", h.deleted)
			}
		}
		second := h.storedConfigMap()
		if second.Name == first.Name || !strings.Contains(second.Data[builder.SourcesKey], "pg-new.example.com") {
			t.Fatalf("the collector mounts %q with %q", second.Name, second.Data[builder.SourcesKey])
		}
		// What the previous pod reads is what it was started with.
		kept := &corev1.ConfigMap{}
		if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(first), kept); err != nil {
			t.Fatal(err)
		}
		if kept.Data[builder.SourcesKey] != first.Data[builder.SourcesKey] ||
			strings.Contains(kept.Data[builder.SourcesKey], "pg-new.example.com") {
			t.Errorf("the ConfigMap of the previous configuration changed:\n%s", kept.Data[builder.SourcesKey])
		}
		if got := h.configMaps(); len(got) != 2 {
			t.Errorf("ConfigMaps %v", got)
		}
		if removed := configMapRemovals(h); len(removed) != 0 {
			t.Errorf("events %q", removed)
		}

		// The new pod is available and the old one is gone.
		h.deploymentStatus = rolledOut
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if got := h.configMaps(); !slices.Equal(got, []string{second.Name}) {
			t.Errorf("ConfigMaps %v, want the current one alone", got)
		}
		want := "ConfigMap " + first.Name + " removed: it holds an earlier configuration, and no pod reads it any more"
		if removed := configMapRemovals(h); !slices.Equal(removed, []string{want}) {
			t.Errorf("events %q, want %q", removed, want)
		}
		h.expectCondition("Ready", metav1.ConditionTrue, "PodRunning", "")

		// And nothing more at the looks that follow.
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if len(h.deleted) != 0 || len(configMapRemovals(h)) != 0 {
			t.Errorf("deleted %v at a look at a monitor that had nothing left to remove", h.deleted)
		}
	})

	// A configuration the monitor returns to finds its ConfigMap, or gets
	// it again, and the ones in between go.
	t.Run("several changes during one rollout", func(t *testing.T) {
		h := healthyHarness(t)
		first := h.storedConfigMap()
		h.deploymentStatus = starting
		for _, host := range []string{"pg-b.example.com", "pg-c.example.com", "pg.example.com"} {
			stored := h.monitor()
			stored.Spec.Database.Host = host
			stored.Generation++
			if err := h.client.Update(context.Background(), stored); err != nil {
				t.Fatal(err)
			}
			if _, err := h.reconcile(); err != nil {
				t.Fatal(err)
			}
		}
		if got := h.configMaps(); len(got) != 3 || len(h.deleted) != 0 {
			t.Fatalf("ConfigMaps %v, deleted %v", got, h.deleted)
		}
		h.deploymentStatus = rolledOut
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if got := h.configMaps(); !slices.Equal(got, []string{first.Name}) {
			t.Errorf("ConfigMaps %v, want %q", got, first.Name)
		}
	})
}

// The single ConfigMap of earlier releases, pgpilot-<monitor>-config, is what
// the pod of the release before mounts. It goes like the ConfigMap of any
// earlier configuration: once the pod of the new template is available.
func TestReconcile_TheConfigMapOfEarlierReleasesGoesAfterTheRollout(t *testing.T) {
	monitor := unitMonitor()
	monitor.UID = "the-monitor"
	legacy := earlierConfigMap("pgpilot-db-config", monitor)
	h := newReconcileHarness(t, monitor, unitSecret(), legacy, runningPod("pgpilot-db-abc"))
	h.deploymentStatus = starting
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	current := h.storedConfigMap()
	if got := h.configMaps(); !slices.Equal(got, []string{"pgpilot-db-config", current.Name}) || len(h.deleted) != 0 {
		t.Fatalf("ConfigMaps %v, deleted %v", got, h.deleted)
	}

	h.deploymentStatus = rolledOut
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	if got := h.configMaps(); !slices.Equal(got, []string{current.Name}) {
		t.Errorf("ConfigMaps %v", got)
	}
	if !slices.Equal(h.deleted, []string{"*v1.ConfigMap pgpilot-db-config"}) {
		t.Errorf("deleted %v", h.deleted)
	}
	want := "ConfigMap pgpilot-db-config removed: it holds an earlier configuration, and no pod reads it any more"
	if removed := configMapRemovals(h); !slices.Equal(removed, []string{want}) {
		t.Errorf("events %q", removed)
	}
}

// Only what is the monitor's and carries one of its names is removed.
func TestReconcile_ConfigMapsThatAreNotEarlierConfigurationsStay(t *testing.T) {
	monitor := unitMonitor()
	monitor.UID = "the-monitor"
	other := unitMonitor()
	other.Name, other.UID = "db-config", "another-monitor"

	stay := map[string]*corev1.ConfigMap{
		"someone else's, under the name of earlier releases": earlierConfigMap("pgpilot-db-config", nil),
		"someone else's, under the name of a configuration":  earlierConfigMap("pgpilot-db-config-0123456789", nil),
		"another monitor's":                                  earlierConfigMap("pgpilot-db-config-abcdef0123", other),
		"the monitor's, and not a configuration":             earlierConfigMap("pgpilot-db-dashboards", monitor),
		"the monitor's, with a name that only looks like it": earlierConfigMap("pgpilot-db-config-not-a-hash", monitor),
		"the monitor's, with a hash that is too long":        earlierConfigMap("pgpilot-db-config-0123456789ab", monitor),
	}
	// Owned by the monitor is not controlled by it, and a monitor of the
	// same name is not this monitor.
	owned := earlierConfigMap("pgpilot-db-config-1111111111", monitor)
	owned.OwnerReferences[0].Controller = nil
	stay["one the monitor owns and does not control"] = owned
	namesake := unitMonitor()
	namesake.UID = "an-earlier-monitor-of-that-name"
	stay["one of an earlier monitor of the same name"] = earlierConfigMap("pgpilot-db-config-2222222222", namesake)
	stay["the monitor's, with a word where the hash would be"] = earlierConfigMap("pgpilot-db-config-dashboards", monitor)

	// What the operator's cache does not show it, it does not go looking
	// for: an object without the labels of a generated one, and one in
	// another namespace, whatever its owner says.
	unlabelled := earlierConfigMap("pgpilot-db-config-fedcba9876", monitor)
	unlabelled.Labels = nil
	stay["the monitor's by its owner, without the labels of a generated object"] = unlabelled
	elsewhere := earlierConfigMap("pgpilot-db-config", monitor)
	elsewhere.Namespace = "elsewhere"
	stay["one of that name in another namespace"] = elsewhere

	objs := make([]client.Object, 0, 2+len(stay))
	objs = append(objs, unitSecret(), runningPod("pgpilot-db-abc"))
	for _, cm := range stay {
		objs = append(objs, cm)
	}
	h := newReconcileHarness(t, monitor, objs...)
	h.deploymentStatus = rolledOut
	for range 2 {
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
	}
	h.expectCondition("Ready", metav1.ConditionTrue, "PodRunning", "")
	if len(h.deleted) != 0 || len(configMapRemovals(h)) != 0 {
		t.Errorf("deleted %v", h.deleted)
	}
	for what, cm := range stay {
		if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(cm), &corev1.ConfigMap{}); err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
}

// A ConfigMap that cannot be removed is the operator's to try again. It
// does not change what the monitor is: the collector runs, on the current
// configuration, and the status says so.
func TestReconcile_AnEarlierConfigMapThatCannotBeRemoved(t *testing.T) {
	boom := apierrors.NewServiceUnavailable("etcd leader changed")
	withLegacy := func(t *testing.T) *reconcileHarness {
		monitor := unitMonitor()
		monitor.UID = "the-monitor"
		h := newReconcileHarness(t, monitor, unitSecret(), runningPod("pgpilot-db-abc"),
			earlierConfigMap("pgpilot-db-config", monitor), earlierConfigMap("pgpilot-db-config-0123456789", monitor))
		h.deploymentStatus = rolledOut
		return h
	}

	t.Run("the delete fails", func(t *testing.T) {
		h := withLegacy(t)
		h.deleteError = func(obj client.Object) error {
			if obj.GetName() == "pgpilot-db-config" {
				return boom
			}
			return nil
		}
		result, err := h.reconcile()
		if !errors.Is(err, boom) || result != (ctrl.Result{}) || !strings.Contains(err.Error(), "remove configmap pgpilot-db-config") {
			t.Errorf("got %+v, %v", result, err)
		}
		h.expectCondition("Ready", metav1.ConditionTrue, "PodRunning", "")
		// The one that could be removed was.
		if !slices.Equal(h.deleted, []string{"*v1.ConfigMap pgpilot-db-config-0123456789"}) {
			t.Errorf("deleted %v", h.deleted)
		}

		h.deleteError = nil
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if got := h.configMaps(); len(got) != 1 {
			t.Errorf("ConfigMaps %v", got)
		}
	})

	t.Run("already gone", func(t *testing.T) {
		h := withLegacy(t)
		h.deleteError = func(obj client.Object) error {
			return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, obj.GetName())
		}
		if result, err := h.reconcile(); err != nil {
			t.Errorf("got %+v, %v: a ConfigMap the cache still shows and the API server no longer has", result, err)
		}
		if removed := configMapRemovals(h); len(removed) != 0 {
			t.Errorf("events %q for what this look did not remove", removed)
		}
	})

	t.Run("the ConfigMaps cannot be listed", func(t *testing.T) {
		h := withLegacy(t)
		h.configMapListError = boom
		result, err := h.reconcile()
		if !errors.Is(err, boom) || result != (ctrl.Result{}) {
			t.Errorf("got %+v, %v", result, err)
		}
		h.expectCondition("Ready", metav1.ConditionTrue, "PodRunning", "")
		if len(h.deleted) != 0 {
			t.Errorf("deleted %v", h.deleted)
		}
	})

	// Told to go once. A ConfigMap that something else holds on to with a
	// finalizer stays where it is, and is not removed again at every look.
	t.Run("one that is on its way out", func(t *testing.T) {
		monitor := unitMonitor()
		monitor.UID = "the-monitor"
		held := earlierConfigMap("pgpilot-db-config", monitor)
		held.Finalizers = []string{"example.com/backup"}
		h := newReconcileHarness(t, monitor, unitSecret(), runningPod("pgpilot-db-abc"), held)
		h.deploymentStatus = rolledOut
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if len(h.deleted) != 1 || len(configMapRemovals(h)) != 1 {
			t.Fatalf("deleted %v", h.deleted)
		}
		for range 3 {
			if _, err := h.reconcile(); err != nil {
				t.Fatal(err)
			}
			if removed := configMapRemovals(h); len(h.deleted) != 0 || len(removed) != 0 {
				t.Errorf("deleted %v, events %q, for a ConfigMap that is already being removed", h.deleted, removed)
			}
		}
		if got := h.configMaps(); len(got) != 2 {
			t.Errorf("ConfigMaps %v", got)
		}
	})

	// The Secret of inline credentials and the ConfigMaps go at the same
	// moment. One failure does not hide the other.
	t.Run("together with a generated Secret that cannot be removed", func(t *testing.T) {
		h := switched(t, unitSecret())
		stored := h.monitor()
		legacy := earlierConfigMap("pgpilot-db-config", stored)
		if err := h.client.Create(context.Background(), legacy); err != nil {
			t.Fatal(err)
		}
		secretGone := apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "pgpilot-db-credentials", errors.New("no"))
		h.deleteError = func(obj client.Object) error {
			if _, isSecret := obj.(*corev1.Secret); isSecret {
				return secretGone
			}
			return boom
		}
		_, err := h.reconcile()
		if !errors.Is(err, boom) || !errors.Is(err, secretGone) {
			t.Errorf("got %v, want both failures", err)
		}
	})

	// Each delete is of the object that was looked at: a ConfigMap created
	// under the name since is not that one.
	t.Run("conditional on the object that was seen", func(t *testing.T) {
		monitor := unitMonitor()
		monitor.UID = "the-monitor"
		legacy := earlierConfigMap("pgpilot-db-config", monitor)
		legacy.UID = "seen"
		h := newReconcileHarness(t, monitor, unitSecret(), runningPod("pgpilot-db-abc"), legacy)
		h.deploymentStatus = rolledOut
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(h.deletedIf, []string{"ConfigMap seen"}) {
			t.Errorf("deletes conditional on %v", h.deletedIf)
		}
	})
}

// The ConfigMaps of earlier configurations go at the first look that finds
// the monitor ready. A monitor that is not ready for a reason of its own
// keeps them, also when the pods have long been replaced.
func TestReconcile_EarlierConfigMapsStayWhileTheMonitorIsNotReadyForAnotherReason(t *testing.T) {
	h := healthyHarness(t)
	stored := h.monitor()
	stored.Spec.Database.Host = "pg-new.example.com"
	stored.Generation++
	if err := h.client.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	h.applyError = func(obj client.Object) error {
		if _, isService := obj.(*corev1.Service); isService {
			return invalid("Service", "pgpilot-db", "no")
		}
		return nil
	}
	for range 2 {
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
	}
	h.expectCondition("Ready", metav1.ConditionFalse, "ApplyRejected", "Service pgpilot-db")
	if got := h.configMaps(); len(got) != 2 || len(h.deleted) != 0 {
		t.Errorf("ConfigMaps %v, deleted %v", got, h.deleted)
	}

	h.applyError = nil
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	if got := h.configMaps(); len(got) != 1 {
		t.Errorf("ConfigMaps %v once the monitor is ready", got)
	}
}

// A monitor that is not ready removes nothing, whatever it is not ready for.
func TestReconcile_NoConfigMapIsRemovedBeforeTheCollectorRuns(t *testing.T) {
	for name, status := range map[string]*appsv1.DeploymentStatus{
		"starting":           starting,
		"the old pod stays":  {Replicas: 2, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, UnavailableReplicas: 1},
		"nothing rolled out": {},
	} {
		t.Run(name, func(t *testing.T) {
			monitor := unitMonitor()
			monitor.UID = "the-monitor"
			h := newReconcileHarness(t, monitor, unitSecret(), earlierConfigMap("pgpilot-db-config", monitor))
			h.deploymentStatus = status
			for range 2 {
				if _, err := h.reconcile(); err != nil {
					t.Fatal(err)
				}
			}
			if h.monitor().Status.Ready || len(h.deleted) != 0 || len(h.configMaps()) != 2 {
				t.Errorf("ready %v, deleted %v, ConfigMaps %v", h.monitor().Status.Ready, h.deleted, h.configMaps())
			}
		})
	}
}

var _ = Describe("The ConfigMap of each configuration", func() {
	const monitorName = "configs"
	var (
		ns         string
		monitorKey types.NamespacedName
		deployKey  types.NamespacedName
	)

	BeforeEach(func() {
		ns = testNamespace("test-configs-")
		monitorKey = types.NamespacedName{Name: monitorName, Namespace: ns}
		deployKey = types.NamespacedName{Name: "pgpilot-" + monitorName, Namespace: ns}
	})

	credentials := func() *corev1.Secret {
		secret := &corev1.Secret{StringData: map[string]string{"username": "pgwatch", "password": "s3cret"}}
		secret.Name, secret.Namespace = "pg-creds", ns
		return secret
	}
	// rolledOutNow reports the collector pod of the current template as
	// running and the one before it as gone, which nothing in this API
	// server does.
	rolledOutNow := func() {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			var dep appsv1.Deployment
			g.Expect(k8sClient.Get(context.TODO(), deployKey, &dep)).To(Succeed())
			dep.Status.ObservedGeneration = dep.Generation
			dep.Status.Replicas, dep.Status.UpdatedReplicas = 1, 1
			dep.Status.ReadyReplicas, dep.Status.AvailableReplicas = 1, 1
			g.Expect(k8sClient.Status().Update(context.TODO(), &dep)).To(Succeed())
		}, timeout, interval).Should(Succeed())
		Eventually(func(g Gomega) {
			var m pgpilotv1.PgpilotMonitor
			g.Expect(k8sClient.Get(context.TODO(), monitorKey, &m)).To(Succeed())
			g.Expect(m.Status.Ready).To(BeTrue())
			g.Expect(m.Status.ObservedGeneration).To(Equal(m.Generation))
		}, timeout, interval).Should(Succeed())
	}
	names := func(g Gomega) []string {
		got, err := configMapNames(k8sClient, monitorKey)
		g.Expect(err).NotTo(HaveOccurred())
		return got
	}
	mounted := func(g Gomega) *corev1.ConfigMap {
		cm, err := mountedConfigMap(k8sClient, monitorKey)
		g.Expect(err).NotTo(HaveOccurred())
		return cm
	}
	removals := func() []string {
		var events corev1.EventList
		Expect(k8sClient.List(context.TODO(), &events, client.InNamespace(ns))).To(Succeed())
		var out []string
		for _, e := range events.Items {
			if e.Reason == "ConfigMapRemoved" && e.InvolvedObject.Name == monitorName {
				out = append(out, e.Message)
			}
		}
		return out
	}

	// The pod of the previous template runs until its successor is
	// available, and reads its files again every two minutes. A ConfigMap
	// that changed under it handed it a configuration written for the
	// successor.
	It("keeps what the previous pod reads until that pod is gone", func() {
		createForSpec(credentials())
		createForSpec(minimalMonitor(ns, monitorName))
		var first *corev1.ConfigMap
		Eventually(func(g Gomega) { first = mounted(g) }, timeout, interval).Should(Succeed())
		Expect(first.Name).To(MatchRegexp(`^pgpilot-configs-config-[0-9a-f]{10}$`))
		Expect(first.OwnerReferences).To(HaveLen(1))
		rolledOutNow()
		Expect(names(Default)).To(Equal([]string{first.Name}))

		By("what a name stands for cannot be changed")
		Expect(first.Immutable).To(HaveValue(BeTrue()))
		edited := first.DeepCopy()
		edited.Data[builder.SourcesKey] = "- name: edited by hand\n"
		err := k8sClient.Update(context.TODO(), edited)
		Expect(apierrors.IsInvalid(err) || apierrors.IsForbidden(err)).To(BeTrue(), "got: %v", err)

		By("the ConfigMap of the current configuration comes back when it is deleted")
		Expect(k8sClient.Delete(context.TODO(), first)).To(Succeed())
		Eventually(func(g Gomega) {
			again := mounted(g)
			g.Expect(again.Name).To(Equal(first.Name))
			g.Expect(again.UID).NotTo(Equal(first.UID))
			g.Expect(again.Data).To(Equal(first.Data))
			first = again
		}, timeout, interval).Should(Succeed())

		Eventually(func(g Gomega) {
			var m pgpilotv1.PgpilotMonitor
			g.Expect(k8sClient.Get(context.TODO(), monitorKey, &m)).To(Succeed())
			m.Spec.Database.Host = "pg-new.example.com"
			g.Expect(k8sClient.Update(context.TODO(), &m)).To(Succeed())
		}, timeout, interval).Should(Succeed())

		var second *corev1.ConfigMap
		Eventually(func(g Gomega) {
			second = mounted(g)
			g.Expect(second.Name).NotTo(Equal(first.Name))
			g.Expect(second.Data[builder.SourcesKey]).To(ContainSubstring("pg-new.example.com"))
		}, timeout, interval).Should(Succeed())
		// The new pod is not there yet: the Deployment's status is still
		// that of the template before.
		Consistently(func(g Gomega) {
			var kept corev1.ConfigMap
			g.Expect(k8sClient.Get(context.TODO(), client.ObjectKeyFromObject(first), &kept)).To(Succeed())
			g.Expect(kept.Data).To(Equal(first.Data), "what the previous pod reads has changed")
			g.Expect(kept.ResourceVersion).To(Equal(first.ResourceVersion))
		}, 2*time.Second, interval).Should(Succeed())
		Expect(removals()).To(BeEmpty())

		rolledOutNow()
		Eventually(func(g Gomega) { g.Expect(names(g)).To(Equal([]string{second.Name})) }, timeout, interval).Should(Succeed())
		Eventually(removals, timeout, interval).Should(ConsistOf(
			"ConfigMap " + first.Name + " removed: it holds an earlier configuration, and no pod reads it any more"))
	})

	// What a monitor of an earlier release has: one ConfigMap,
	// pgpilot-<monitor>-config, mounted by the pod that runs.
	It("moves a monitor of an earlier release to a ConfigMap of its own, and removes the old one after the rollout", func() {
		createForSpec(minimalMonitor(ns, monitorName))
		var stored pgpilotv1.PgpilotMonitor
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(context.TODO(), monitorKey, &stored)).To(Succeed())
			g.Expect(stored.Status.Conditions).NotTo(BeEmpty()) // SecretNotFound: nothing is generated yet
		}, timeout, interval).Should(Succeed())
		Expect(names(Default)).To(BeEmpty())

		earlier := &corev1.ConfigMap{Data: map[string]string{builder.SourcesKey: "- name: of the earlier release\n"}}
		earlier.APIVersion, earlier.Kind = "v1", "ConfigMap"
		earlier.Name, earlier.Namespace, earlier.Labels = "pgpilot-configs-config", ns, builder.SystemLabels(monitorName)
		Expect(controllerutil.SetControllerReference(&stored, earlier, scheme.Scheme)).To(Succeed())
		//nolint:staticcheck // SA1019: the way the operator applies, see serverSideApply
		Expect(k8sClient.Patch(context.TODO(), earlier, client.Apply,
			client.FieldOwner("pgpilot-operator"), client.ForceOwnership)).To(Succeed())
		// Someone else's ConfigMap that only looks like one of the monitor's.
		theirs := &corev1.ConfigMap{Data: map[string]string{"theirs": "data"}}
		theirs.Name, theirs.Namespace, theirs.Labels = "pgpilot-configs-config-0123456789", ns, builder.SystemLabels(monitorName)
		createForSpec(theirs)

		createForSpec(credentials())
		var current *corev1.ConfigMap
		Eventually(func(g Gomega) { current = mounted(g) }, timeout, interval).Should(Succeed())
		Expect(current.Name).NotTo(Equal(earlier.Name))
		Expect(current.Data[builder.SourcesKey]).To(ContainSubstring("pg.example.com"))
		Consistently(func(g Gomega) {
			var kept corev1.ConfigMap
			g.Expect(k8sClient.Get(context.TODO(), client.ObjectKeyFromObject(earlier), &kept)).To(Succeed())
			g.Expect(kept.Data[builder.SourcesKey]).To(Equal("- name: of the earlier release\n"))
		}, 2*time.Second, interval).Should(Succeed())

		rolledOutNow()
		Eventually(func(g Gomega) {
			g.Expect(names(g)).To(ConsistOf(current.Name, theirs.Name))
		}, timeout, interval).Should(Succeed())
		Expect(removals()).To(ConsistOf(ContainSubstring("ConfigMap pgpilot-configs-config removed")))
	})
})
