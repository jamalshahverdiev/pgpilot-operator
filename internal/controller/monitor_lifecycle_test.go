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
	"slices"
	"strings"
	"testing"
	"time"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

// someoneElses returns an object of the given kind under the name the
// operator generates for the monitor "db", as someone else might have
// created it.
func someoneElses(t *testing.T, kind string) client.Object {
	t.Helper()
	var obj client.Object
	switch kind {
	case "ConfigMap":
		obj = &corev1.ConfigMap{Data: map[string]string{"theirs": "data"}}
		obj.SetName("pgpilot-db-config")
	case "Deployment":
		labels := map[string]string{"app": "theirs"}
		dep := &appsv1.Deployment{}
		dep.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		dep.Spec.Template.Labels = labels
		dep.Spec.Template.Spec.Containers = []corev1.Container{{Name: "app", Image: "example.com/app:1"}}
		obj = dep
	case "Service":
		svc := &corev1.Service{}
		svc.Spec.Selector = map[string]string{"app": "theirs"}
		svc.Spec.Ports = []corev1.ServicePort{{Port: 8080}}
		obj = svc
	case "ServiceMonitor":
		obj = &monitoringv1.ServiceMonitor{}
	default:
		t.Fatalf("no such kind: %s", kind)
	}
	if obj.GetName() == "" {
		obj.SetName("pgpilot-db")
	}
	obj.SetNamespace("team")
	obj.SetLabels(map[string]string{"made-by": "someone-else"})
	return obj
}

func lifecycleScheme(t *testing.T) {
	t.Helper()
	unitScheme(t)
	if err := monitoringv1.AddToScheme(scheme.Scheme); err != nil {
		t.Fatal(err)
	}
}

// The names of the generated objects are predictable, and an apply that
// forces ownership asks no questions. An object that happened to carry such
// a name was rewritten, got the monitor as its owner, and was deleted with
// it.
func TestReconcile_AnObjectOfTheSameNameIsLeftAlone(t *testing.T) {
	lifecycleScheme(t)
	for _, kind := range []string{"ConfigMap", "Deployment", "Service", "ServiceMonitor"} {
		t.Run(kind, func(t *testing.T) {
			theirs := someoneElses(t, kind)
			h := newReconcileHarness(t, unitMonitor(), unitSecret(), theirs)
			h.r.ServiceMonitorEnabled = kind == "ServiceMonitor"
			before := theirs.DeepCopyObject().(client.Object)
			if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(theirs), before); err != nil {
				t.Fatal(err)
			}

			result, err := h.reconcile()
			// Not a failed reconcile, and nothing a retry in a moment would
			// change: looked at again after a while.
			if err != nil || result != (ctrl.Result{RequeueAfter: 30 * time.Second}) {
				t.Errorf("got %+v, %v; want another look in thirty seconds and no error", result, err)
			}
			if slices.Contains(h.applied, kind) {
				t.Errorf("applied %v: the %s is not the monitor's to write", h.applied, kind)
			}
			h.expectCondition("Ready", metav1.ConditionFalse, "NameConflict", kind+" "+theirs.GetName()+" already exists")
			h.expectCondition("Ready", metav1.ConditionFalse, "NameConflict",
				"is not this monitor's: it has no owner. It was left as it is; delete it")
			if kind == "ConfigMap" {
				h.expectCondition("ConfigGenerated", metav1.ConditionFalse, "NameConflict", "")
			} else {
				h.expectCondition("ConfigGenerated", metav1.ConditionTrue, "ConfigMapUpdated", "")
			}
			events := drain(h.recorder)
			if len(events) == 0 || !strings.HasPrefix(events[len(events)-1], "Warning NameConflict "+kind) {
				t.Errorf("events: %q", events)
			}

			after := theirs.DeepCopyObject().(client.Object)
			if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(theirs), after); err != nil {
				t.Fatal(err)
			}
			if after.GetResourceVersion() != before.GetResourceVersion() || len(after.GetOwnerReferences()) != 0 ||
				after.GetLabels()["made-by"] != "someone-else" {
				t.Errorf("the object was written to: %+v", after)
			}

			// Still there at the next look: nothing new to say.
			writes := h.statusWrites
			if _, err := h.reconcile(); err != nil {
				t.Fatal(err)
			}
			if again := drain(h.recorder); len(again) != 0 || h.statusWrites != writes {
				t.Errorf("the conflict was reported again: events %q, %d status writes", again, h.statusWrites-writes)
			}

			// Once it is out of the way the monitor gets its own.
			if err := h.client.Delete(context.Background(), theirs); err != nil {
				t.Fatal(err)
			}
			if _, err := h.reconcile(); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(h.applied, kind) {
				t.Errorf("applied %v, without the %s", h.applied, kind)
			}
			if cond := meta.FindStatusCondition(h.monitor().Status.Conditions, "Ready"); cond.Reason == "NameConflict" {
				t.Errorf("still reported as a conflict: %+v", cond)
			}
		})
	}
}

func TestReconcile_WhoseAnObjectOfTheSameNameIs(t *testing.T) {
	lifecycleScheme(t)
	controller := true
	ownedBy := func(ref metav1.OwnerReference) client.Object {
		svc := someoneElses(t, "Service")
		svc.SetOwnerReferences([]metav1.OwnerReference{ref})
		return svc
	}
	monitor := func() *pgpilotv1.PgpilotMonitor {
		m := unitMonitor()
		m.UID = "the-monitor"
		return m
	}
	labelled := someoneElses(t, "Service")
	labelled.SetLabels(builder.SystemLabels("db"))

	cases := map[string]struct {
		existing client.Object
		// wantOwner is part of the message, up to the end of its sentence;
		// empty when there is no conflict.
		wantOwner string
	}{
		"another controller's": {
			ownedBy(metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "web", UID: "web", Controller: &controller}),
			"it is owned by Deployment web. ",
		},
		// The remark about an earlier object is for a monitor of that name,
		// not for whatever is called the same.
		"an owner of another kind that happens to have the monitor's name": {
			ownedBy(metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "db", UID: "web", Controller: &controller}),
			"it is owned by Deployment db. ",
		},
		// Left behind by `kubectl delete --cascade=orphan`, or labelled by
		// hand: the label is no claim, only the owner reference is.
		"the operator's labels, and no owner": {labelled, "it has no owner. "},
		// A monitor deleted and created again: what the first one owned is
		// on its way out and must not be taken over meanwhile.
		"an earlier monitor's of the same name": {
			ownedBy(metav1.OwnerReference{
				APIVersion: "pgpilot.io/v1", Kind: "PgpilotMonitor", Name: "db", UID: "the-one-before", Controller: &controller,
			}),
			"it is owned by PgpilotMonitor db, an earlier object of that name that is being removed. ",
		},
		"another monitor's": {
			ownedBy(metav1.OwnerReference{
				APIVersion: "pgpilot.io/v1", Kind: "PgpilotMonitor", Name: "other", UID: "other", Controller: &controller,
			}),
			"it is owned by PgpilotMonitor other. ",
		},
		// An owner that does not control it is no claim to it.
		"this monitor's, but not as its controller": {
			ownedBy(metav1.OwnerReference{APIVersion: "pgpilot.io/v1", Kind: "PgpilotMonitor", Name: "db", UID: "the-monitor"}),
			"it has no owner. ",
		},
		"this monitor's": {
			ownedBy(metav1.OwnerReference{
				APIVersion: "pgpilot.io/v1", Kind: "PgpilotMonitor", Name: "db", UID: "the-monitor", Controller: &controller,
			}),
			"",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newReconcileHarness(t, monitor(), unitSecret(), tc.existing)
			if _, err := h.reconcile(); err != nil {
				t.Fatal(err)
			}
			cond := meta.FindStatusCondition(h.monitor().Status.Conditions, "Ready")
			if tc.wantOwner == "" {
				if cond.Reason == "NameConflict" || !slices.Contains(h.applied, "Service") {
					t.Errorf("the monitor's own Service was not applied: %v, %+v", h.applied, cond)
				}
				return
			}
			if cond.Reason != "NameConflict" || !strings.Contains(cond.Message, tc.wantOwner) {
				t.Errorf("Ready: %s %q, want a conflict that says %q", cond.Reason, cond.Message, tc.wantOwner)
			}
			if slices.Contains(h.applied, "Service") {
				t.Errorf("applied %v: the Service is not the monitor's to write", h.applied)
			}
		})
	}
}

// countingReader counts the reads that go to the API server.
type countingReader struct {
	client.Reader
	gets []string
	fail error
}

func (c *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.gets = append(c.gets, kindOf(obj))
	if c.fail != nil {
		return c.fail
	}
	return c.Reader.Get(ctx, key, obj, opts...)
}

func kindOf(obj client.Object) string {
	switch obj.(type) {
	case *corev1.ConfigMap:
		return "ConfigMap"
	case *appsv1.Deployment:
		return "Deployment"
	case *corev1.Service:
		return "Service"
	case *corev1.Secret:
		return "Secret"
	case *monitoringv1.ServiceMonitor:
		return "ServiceMonitor"
	}
	return "other"
}

// The operator's cache only holds what carries its label. Whether a name is
// taken by something else can only be asked at the API server.
func TestReconcile_TheAPIServerIsAskedForWhatTheCacheDoesNotShow(t *testing.T) {
	lifecycleScheme(t)
	apiServer := func(objs ...client.Object) *countingReader {
		return &countingReader{Reader: fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()}
	}

	t.Run("an object the cache does not show is found there", func(t *testing.T) {
		h := newReconcileHarness(t, unitMonitor(), unitSecret())
		live := apiServer(someoneElses(t, "Service"))
		h.r.APIReader = live

		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("Ready", metav1.ConditionFalse, "NameConflict", "Service pgpilot-db already exists")
		if got := strings.Join(h.applied, ","); got != "ConfigMap,Deployment" {
			t.Errorf("applied %s", got)
		}
	})

	t.Run("asked once per object, before it is created", func(t *testing.T) {
		h := newReconcileHarness(t, unitMonitor(), unitSecret())
		live := apiServer()
		h.r.APIReader = live

		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(live.gets, ","); got != "ConfigMap,Deployment,Service" {
			t.Errorf("asked the API server for %s", got)
		}
		// From then on the cache shows them as the monitor's own.
		live.gets = nil
		for range 3 {
			if _, err := h.reconcile(); err != nil {
				t.Fatal(err)
			}
		}
		if len(live.gets) != 0 {
			t.Errorf("asked the API server again for %v although the cache has the objects", live.gets)
		}
	})

	t.Run("not knowing is a failed reconcile, not a free name", func(t *testing.T) {
		h := newReconcileHarness(t, unitMonitor(), unitSecret())
		boom := apierrors.NewServiceUnavailable("etcd leader changed")
		live := apiServer()
		live.fail = boom
		h.r.APIReader = live

		result, err := h.reconcile()
		if !errors.Is(err, boom) || result != (ctrl.Result{}) {
			t.Errorf("got %+v, %v; want the error handed to the work queue", result, err)
		}
		if len(h.applied) != 0 {
			t.Errorf("applied %v without knowing whether the name is free", h.applied)
		}
		h.expectCondition("Ready", metav1.ConditionFalse, "ApplyFailed", "ConfigMap pgpilot-db-config could not be applied")
	})

	// The API server is asked about what the cache does not have, not about
	// what the cache could not say.
	t.Run("a cache that cannot answer is a failed reconcile, and the API server is not asked instead", func(t *testing.T) {
		h := newReconcileHarness(t, unitMonitor(), unitSecret())
		live := apiServer()
		h.r.APIReader = live
		boom := apierrors.NewServiceUnavailable("the cache is not ready")
		h.getError = func(obj client.Object) error {
			if _, is := obj.(*corev1.ConfigMap); is {
				return boom
			}
			return nil
		}

		if _, err := h.reconcile(); !errors.Is(err, boom) {
			t.Errorf("got %v, want the failed read handed to the work queue", err)
		}
		if len(live.gets) != 0 || len(h.applied) != 0 {
			t.Errorf("asked the API server for %v, applied %v", live.gets, h.applied)
		}
	})

	// What the API server returns is what is judged.
	controller := true
	heldByTheAPIServer := func(ref metav1.OwnerReference) *reconcileHarness {
		monitor := unitMonitor()
		monitor.UID = "the-monitor"
		h := newReconcileHarness(t, monitor, unitSecret())
		svc := someoneElses(t, "Service")
		svc.SetOwnerReferences([]metav1.OwnerReference{ref})
		h.r.APIReader = apiServer(svc)
		return h
	}

	// Right after the operator created an object its cache may not show it
	// yet. The API server does, and it is the monitor's own.
	t.Run("the monitor's own object that the cache does not show yet is applied", func(t *testing.T) {
		h := heldByTheAPIServer(metav1.OwnerReference{
			APIVersion: "pgpilot.io/v1", Kind: "PgpilotMonitor", Name: "db", UID: "the-monitor", Controller: &controller,
		})
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		cond := meta.FindStatusCondition(h.monitor().Status.Conditions, "Ready")
		if cond.Reason == "NameConflict" || !slices.Contains(h.applied, "Service") {
			t.Errorf("applied %v, Ready %s %q", h.applied, cond.Reason, cond.Message)
		}
	})

	t.Run("whose it is is read from what the API server holds", func(t *testing.T) {
		h := heldByTheAPIServer(metav1.OwnerReference{
			APIVersion: "apps/v1", Kind: "Deployment", Name: "web", UID: "web", Controller: &controller,
		})
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("Ready", metav1.ConditionFalse, "NameConflict", "it is owned by Deployment web. ")
	})
}

// removals returns what the events say was removed because the Prometheus
// sink is off, and reads all other events away.
func removals(h *reconcileHarness) []string {
	var out []string
	for _, e := range drain(h.recorder) {
		if what, is := strings.CutPrefix(e, "Normal ScrapeTargetRemoved "); is {
			out = append(out, what)
		}
	}
	return out
}

// grpcOnly turns the Prometheus sink of a stored monitor off.
func grpcOnly(h *reconcileHarness) {
	h.t.Helper()
	m := h.monitor()
	m.Spec.Sinks = pgpilotv1.SinksSpec{
		Prometheus: &pgpilotv1.PrometheusSink{Enabled: false},
		GRPC:       &pgpilotv1.GRPCSink{Enabled: true, Endpoint: "receiver.example.com:5000"},
	}
	m.Generation++
	if err := h.client.Update(context.Background(), m); err != nil {
		h.t.Fatal(err)
	}
}

func exists(h *reconcileHarness, obj client.Object) bool {
	h.t.Helper()
	err := h.client.Get(context.Background(), types.NamespacedName{Namespace: "team", Name: "pgpilot-db"}, obj)
	if err != nil && !apierrors.IsNotFound(err) {
		h.t.Fatal(err)
	}
	return err == nil
}

// A Service and a ServiceMonitor point at the port of the Prometheus sink.
// Without the sink nothing listens there: a target that is always down.
func TestReconcile_ScrapeTargetsFollowThePrometheusSink(t *testing.T) {
	lifecycleScheme(t)

	t.Run("turned off: the Service and the ServiceMonitor go", func(t *testing.T) {
		h := healthyHarness(t)
		h.r.ServiceMonitorEnabled = true
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if !exists(h, &corev1.Service{}) || !exists(h, &monitoringv1.ServiceMonitor{}) {
			t.Fatal("the Service and the ServiceMonitor were not created to begin with")
		}
		h.expectCondition("ServiceMonitorApplied", metav1.ConditionTrue, "Applied", "ServiceMonitor pgpilot-db is in place")

		grpcOnly(h)
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(h.applied, ","); got != "ConfigMap,Deployment" {
			t.Errorf("applied %s", got)
		}
		if got := strings.Join(h.deleted, ","); got != "*v1.Service pgpilot-db,*v1.ServiceMonitor pgpilot-db" {
			t.Errorf("deleted %s", got)
		}
		if exists(h, &corev1.Service{}) || exists(h, &monitoringv1.ServiceMonitor{}) {
			t.Error("still there")
		}
		if cond := meta.FindStatusCondition(h.monitor().Status.Conditions, "ServiceMonitorApplied"); cond != nil {
			t.Errorf("a monitor that is to have no ServiceMonitor still reports on one: %+v", cond)
		}
		// Objects that may have been there for months do not go unsaid.
		if got := removals(h); !slices.Equal(got, []string{
			"Service pgpilot-db removed: the Prometheus sink is off",
			"ServiceMonitor pgpilot-db removed: the Prometheus sink is off",
		}) {
			t.Errorf("events about what was removed: %q", got)
		}

		// Nothing left to remove at the next look.
		if _, err := h.reconcile(); err != nil || len(h.deleted) != 0 || len(removals(h)) != 0 {
			t.Errorf("second look: deleted %v, %v", h.deleted, err)
		}

		// Turned on again: both are back.
		m := h.monitor()
		m.Spec.Sinks.Prometheus.Enabled = true
		m.Generation++
		if err := h.client.Update(context.Background(), m); err != nil {
			t.Fatal(err)
		}
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(h.applied, ","); got != "ConfigMap,Deployment,Service,ServiceMonitor" {
			t.Errorf("applied %s", got)
		}
	})

	t.Run("off from the start: neither is created", func(t *testing.T) {
		monitor := unitMonitor()
		monitor.Spec.Sinks = pgpilotv1.SinksSpec{GRPC: &pgpilotv1.GRPCSink{Enabled: true, Endpoint: "receiver:5000"}}
		h := newReconcileHarness(t, monitor, unitSecret())
		h.r.ServiceMonitorEnabled = true
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(h.applied, ","); got != "ConfigMap,Deployment" || len(h.deleted) != 0 {
			t.Errorf("applied %s, deleted %v", got, h.deleted)
		}
	})

	// The manager's cache is told to hold only the operator's ServiceMonitors
	// when support was found at start. A read of one without that would start
	// an informer on every ServiceMonitor of the cluster.
	t.Run("without ServiceMonitor support only the Service is looked for", func(t *testing.T) {
		h := healthyHarness(t)
		grpcOnly(h)
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(h.deleted, ","); got != "*v1.Service pgpilot-db" {
			t.Errorf("deleted %s", got)
		}
		if slices.Contains(h.read, "ServiceMonitor") || !slices.Contains(h.read, "Service") {
			t.Errorf("read %v", h.read)
		}
	})

}

// Turning the sink off removes what the monitor controls and nothing else.
func TestReconcile_OnlyTheMonitorsOwnScrapeTargetsAreRemoved(t *testing.T) {
	lifecycleScheme(t)

	t.Run("an object of that name that is not the monitor's stays", func(t *testing.T) {
		controller := true
		for name, owner := range map[string]*metav1.OwnerReference{
			"without an owner":     nil,
			"another controller's": {APIVersion: "apps/v1", Kind: "Deployment", Name: "web", UID: "web", Controller: &controller},
			"an earlier monitor's of the same name": {
				APIVersion: "pgpilot.io/v1", Kind: "PgpilotMonitor", Name: "db", UID: "the-one-before", Controller: &controller,
			},
		} {
			t.Run(name, func(t *testing.T) {
				monitor := unitMonitor()
				monitor.UID = "the-monitor"
				monitor.Spec.Sinks = pgpilotv1.SinksSpec{GRPC: &pgpilotv1.GRPCSink{Enabled: true, Endpoint: "receiver:5000"}}
				svc, sm := someoneElses(t, "Service"), someoneElses(t, "ServiceMonitor")
				if owner != nil {
					svc.SetOwnerReferences([]metav1.OwnerReference{*owner})
					sm.SetOwnerReferences([]metav1.OwnerReference{*owner})
				}
				h := newReconcileHarness(t, monitor, unitSecret(), svc, sm)
				h.r.ServiceMonitorEnabled = true
				if _, err := h.reconcile(); err != nil {
					t.Fatal(err)
				}
				if len(h.deleted) != 0 || !exists(h, &corev1.Service{}) || !exists(h, &monitoringv1.ServiceMonitor{}) {
					t.Errorf("deleted %v", h.deleted)
				}
				if got := removals(h); len(got) != 0 {
					t.Errorf("announced %q", got)
				}
			})
		}
	})

	// One created under the name since the look is not the one looked at.
	t.Run("only the object that was looked at is deleted", func(t *testing.T) {
		monitor := unitMonitor()
		monitor.UID = "the-monitor"
		h := newReconcileHarness(t, monitor, unitSecret())
		h.r.ServiceMonitorEnabled = true
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		// The fake client hands out no UIDs.
		for obj, uid := range map[client.Object]types.UID{
			&corev1.Service{}: "the-service", &monitoringv1.ServiceMonitor{}: "the-servicemonitor",
		} {
			if !exists(h, obj) {
				t.Fatalf("%T was not created", obj)
			}
			obj.SetUID(uid)
			if err := h.client.Update(context.Background(), obj); err != nil {
				t.Fatal(err)
			}
		}
		grpcOnly(h)
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(h.deletedIf, ","); got != "Service the-service,ServiceMonitor the-servicemonitor" {
			t.Errorf("deleted on condition of %q", got)
		}
	})

}

func TestReconcile_HowScrapeTargetsAreRemoved(t *testing.T) {
	lifecycleScheme(t)

	// The cache holds everything the monitor controls: a monitor without the
	// Prometheus sink must not cost two requests at every reconcile.
	t.Run("what is to be removed is looked for in the cache", func(t *testing.T) {
		h := healthyHarness(t)
		h.r.ServiceMonitorEnabled = true
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		live := &countingReader{Reader: h.client}
		h.r.APIReader = live
		grpcOnly(h)
		for range 2 {
			if _, err := h.reconcile(); err != nil {
				t.Fatal(err)
			}
		}
		if len(live.gets) != 0 {
			t.Errorf("asked the API server for %v", live.gets)
		}
		if exists(h, &corev1.Service{}) || exists(h, &monitoringv1.ServiceMonitor{}) {
			t.Error("still there")
		}
	})

	// What an upgrade finds: the Service of an earlier release on a monitor
	// whose status is already what it will be. Events that wait for a status
	// write would never be recorded.
	t.Run("a removal is announced even when the status does not change", func(t *testing.T) {
		controller := true
		monitor := unitMonitor()
		monitor.UID = "the-monitor"
		monitor.Spec.Sinks = pgpilotv1.SinksSpec{GRPC: &pgpilotv1.GRPCSink{Enabled: true, Endpoint: "receiver:5000"}}
		h := newReconcileHarness(t, monitor, unitSecret(), runningPod("pgpilot-db-abc"))
		h.deploymentStatus = rolledOut
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		drain(h.recorder)
		svc := someoneElses(t, "Service")
		svc.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: "pgpilot.io/v1", Kind: "PgpilotMonitor", Name: "db", UID: "the-monitor", Controller: &controller,
		}})
		if err := h.client.Create(context.Background(), svc); err != nil {
			t.Fatal(err)
		}
		writes := h.statusWrites

		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if h.statusWrites != writes {
			t.Fatalf("%d status writes: this is about a reconcile that writes none", h.statusWrites-writes)
		}
		if got := removals(h); !slices.Equal(got, []string{"Service pgpilot-db removed: the Prometheus sink is off"}) {
			t.Errorf("events about what was removed: %q", got)
		}
	})

}

func TestReconcile_ScrapeTargetsThatCannotBeRemoved(t *testing.T) {
	lifecycleScheme(t)

	t.Run("a Service that cannot be removed is a failed reconcile", func(t *testing.T) {
		h := healthyHarness(t)
		grpcOnly(h)
		boom := apierrors.NewForbidden(schema.GroupResource{Resource: "services"}, "pgpilot-db", errors.New("no"))
		h.deleteError = func(client.Object) error { return boom }

		result, err := h.reconcile()
		if !errors.Is(err, boom) || result != (ctrl.Result{}) {
			t.Errorf("got %+v, %v; want the error handed to the work queue", result, err)
		}
		h.expectCondition("Ready", metav1.ConditionFalse, "ApplyFailed", "Service pgpilot-db is no longer wanted and could not be removed")
	})

	t.Run("a Service deleted by someone else meanwhile is no error", func(t *testing.T) {
		h := healthyHarness(t)
		grpcOnly(h)
		h.deleteError = func(obj client.Object) error {
			return apierrors.NewNotFound(schema.GroupResource{Resource: "services"}, obj.GetName())
		}
		if _, err := h.reconcile(); err != nil {
			t.Errorf("got %v", err)
		}
		// It was not this reconcile that removed it.
		if got := removals(h); len(got) != 0 {
			t.Errorf("announced %q", got)
		}
	})

	// A monitor with both, whose Prometheus sink has just been turned off.
	withBoth := func(t *testing.T) *reconcileHarness {
		t.Helper()
		h := healthyHarness(t)
		h.r.ServiceMonitorEnabled = true
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		grpcOnly(h)
		return h
	}

	t.Run("a ServiceMonitor that cannot be removed is a failed reconcile", func(t *testing.T) {
		h := withBoth(t)
		boom := apierrors.NewForbidden(schema.GroupResource{Resource: "servicemonitors"}, "pgpilot-db", errors.New("no"))
		h.deleteError = func(obj client.Object) error {
			if _, is := obj.(*monitoringv1.ServiceMonitor); is {
				return boom
			}
			return nil
		}

		result, err := h.reconcile()
		if !errors.Is(err, boom) || result != (ctrl.Result{}) {
			t.Errorf("got %+v, %v; want the error handed to the work queue", result, err)
		}
		h.expectCondition("Ready", metav1.ConditionFalse, "ApplyFailed",
			"ServiceMonitor pgpilot-db is no longer wanted and could not be removed")
		if !exists(h, &monitoringv1.ServiceMonitor{}) {
			t.Error("the ServiceMonitor is gone all the same")
		}
		if got := removals(h); !slices.Equal(got, []string{"Service pgpilot-db removed: the Prometheus sink is off"}) {
			t.Errorf("events about what was removed: %q", got)
		}
	})

	t.Run("a Service that cannot be looked for is a failed reconcile", func(t *testing.T) {
		h := withBoth(t)
		boom := apierrors.NewServiceUnavailable("the cache is not ready")
		h.getError = func(obj client.Object) error {
			if _, is := obj.(*corev1.Service); is {
				return boom
			}
			return nil
		}
		if _, err := h.reconcile(); !errors.Is(err, boom) {
			t.Errorf("got %v, want the failed read handed to the work queue", err)
		}
	})

	t.Run("a ServiceMonitor kind the API server no longer knows is nothing to remove", func(t *testing.T) {
		h := withBoth(t)
		h.getError = func(obj client.Object) error {
			if _, is := obj.(*monitoringv1.ServiceMonitor); is {
				return &meta.NoKindMatchError{
					GroupKind:        schema.GroupKind{Group: "monitoring.coreos.com", Kind: "ServiceMonitor"},
					SearchedVersions: []string{"v1"},
				}
			}
			return nil
		}
		if _, err := h.reconcile(); err != nil {
			t.Errorf("got %v", err)
		}
	})
}

// The operator decides when it starts whether the cluster has ServiceMonitors.
// A cluster can stop serving them later; every reconcile then failed before
// it got to the status, for something the collector does not depend on.
func TestReconcile_ServiceMonitorAPIGoneWhileRunning(t *testing.T) {
	lifecycleScheme(t)
	gone := map[string]error{
		"the path answers 404": apierrors.NewNotFound(schema.GroupResource{Group: "monitoring.coreos.com", Resource: "servicemonitors"}, "pgpilot-db"),
		"the kind is unknown": &meta.NoKindMatchError{
			GroupKind: schema.GroupKind{Group: "monitoring.coreos.com", Kind: "ServiceMonitor"}, SearchedVersions: []string{"v1"},
		},
	}
	for name, failure := range gone {
		t.Run(name, func(t *testing.T) {
			monitor := unitMonitor()
			monitor.Spec.Database.CredentialsSecret = nil
			monitor.Spec.Database.Username, monitor.Spec.Database.Password = "u", "p"
			h := newReconcileHarness(t, monitor, runningPod("pgpilot-db-abc"))
			h.r.ServiceMonitorEnabled = true
			h.deploymentStatus = rolledOut
			serviceMonitorFails := func(obj client.Object) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == "ServiceMonitor" {
					return failure
				}
				return nil
			}
			h.applyError = serviceMonitorFails

			result, err := h.reconcile()
			// The collector runs: the monitor is ready, and the reconcile did
			// not fail. Inline credentials give no other reason to come back,
			// so the operator comes back for this.
			if err != nil || result.RequeueAfter < 5*time.Minute || result.RequeueAfter > 5*time.Minute+30*time.Second {
				t.Errorf("got %+v, %v; want no error and another look in five minutes", result, err)
			}
			h.expectCondition("Ready", metav1.ConditionTrue, "PodRunning", "")
			h.expectCondition("ServiceMonitorApplied", metav1.ConditionFalse, "ServiceMonitorAPIMissing",
				"this cluster no longer serves monitoring.coreos.com/v1")
			events := drain(h.recorder)
			warnings := 0
			for _, e := range events {
				if strings.HasPrefix(e, "Warning ServiceMonitorAPIMissing ServiceMonitor pgpilot-db could not be applied") {
					warnings++
				}
			}
			if warnings != 1 {
				t.Errorf("events: %q", events)
			}

			// Said once: not at the next look, and not when the status is
			// written again for another reason.
			if _, err := h.reconcile(); err != nil {
				t.Fatal(err)
			}
			// A collector on its way up is looked at again soon, whatever
			// the ServiceMonitor waits for.
			h.deploymentStatus = starting
			if result, err := h.reconcile(); err != nil || result.RequeueAfter != notReadyRequeue {
				t.Fatalf("got %+v, %v; want another look after %s", result, err, notReadyRequeue)
			}
			h.expectCondition("Ready", metav1.ConditionFalse, "PodNotReady", "")
			for _, e := range drain(h.recorder) {
				if strings.Contains(e, "ServiceMonitorAPIMissing") {
					t.Errorf("announced again: %q", e)
				}
			}

			// The API is back.
			h.applyError = nil
			h.deploymentStatus = rolledOut
			if _, err := h.reconcile(); err != nil {
				t.Fatal(err)
			}
			h.expectCondition("ServiceMonitorApplied", metav1.ConditionTrue, "Applied", "")

			// And gone once more: that is news again.
			h.applyError = serviceMonitorFails
			drain(h.recorder)
			if _, err := h.reconcile(); err != nil {
				t.Fatal(err)
			}
			if events := drain(h.recorder); len(events) != 1 || !strings.HasPrefix(events[0], "Warning ServiceMonitorAPIMissing ") {
				t.Errorf("events when it went missing again: %q", events)
			}
		})
	}
}

// serviceMonitorAPIGone is the answer of a cluster that stopped serving
// ServiceMonitors, for the harness's applyError.
func serviceMonitorAPIGone(obj client.Object) error {
	if obj.GetObjectKind().GroupVersionKind().Kind != "ServiceMonitor" {
		return nil
	}
	return apierrors.NewNotFound(schema.GroupResource{Group: "monitoring.coreos.com", Resource: "servicemonitors"}, obj.GetName())
}

// A monitor that reads its credentials from a Secret is looked at again for
// that anyway. How soon is a setting, and the ServiceMonitor is not to wait
// for it: an hour between two reads of a Secret is a sensible choice, an hour
// without a scrape target after the prometheus-operator was reinstalled is
// not.
func TestReconcile_ServiceMonitorRetryAndTheCredentialsRecheck(t *testing.T) {
	lifecycleScheme(t)
	for name, tc := range map[string]struct {
		recheck  time.Duration
		min, max time.Duration
	}{
		"the Secret is read again later than the ServiceMonitor is tried":  {time.Hour, rejectedRetry, rejectedRetry + 30*time.Second},
		"the Secret is read again sooner than the ServiceMonitor is tried": {30 * time.Second, 30 * time.Second, 33 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			h := healthyHarness(t)
			h.r.ServiceMonitorEnabled = true
			h.r.CredentialsRecheck = tc.recheck
			h.applyError = serviceMonitorAPIGone

			result, err := h.reconcile()
			if err != nil || result.RequeueAfter < tc.min || result.RequeueAfter > tc.max {
				t.Errorf("got %+v, %v; want another look after %s to %s", result, err, tc.min, tc.max)
			}
			h.expectCondition("Ready", metav1.ConditionTrue, "PodRunning", "")
			h.expectCondition("ServiceMonitorApplied", metav1.ConditionFalse, "ServiceMonitorAPIMissing", "")
		})
	}
}

func TestReconcile_ServiceMonitorCondition(t *testing.T) {
	lifecycleScheme(t)
	condition := func(h *reconcileHarness) *metav1.Condition {
		return meta.FindStatusCondition(h.monitor().Status.Conditions, "ServiceMonitorApplied")
	}

	t.Run("not there when the operator has no ServiceMonitor support", func(t *testing.T) {
		h := healthyHarness(t)
		if cond := condition(h); cond != nil {
			t.Errorf("got %+v", cond)
		}
	})

	t.Run("removed when the operator lost that support", func(t *testing.T) {
		h := healthyHarness(t)
		h.r.ServiceMonitorEnabled = true
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if condition(h) == nil {
			t.Fatal("no condition while ServiceMonitors are supported")
		}
		// The operator was restarted on a cluster without the CRD.
		h.r.ServiceMonitorEnabled = false
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if cond := condition(h); cond != nil {
			t.Errorf("got %+v", cond)
		}
	})

	t.Run("unknown when the reconcile did not get that far", func(t *testing.T) {
		h := healthyHarness(t)
		h.r.ServiceMonitorEnabled = true
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if err := h.client.Delete(context.Background(), unitSecret()); err != nil {
			t.Fatal(err)
		}
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("ServiceMonitorApplied", metav1.ConditionUnknown, "SecretNotFound", "Not evaluated: Secret")
	})

	// That the cluster serves no ServiceMonitors is a fact about the cluster.
	// A reconcile that ended early has not looked at it again, and the next
	// one that does has nothing new to say.
	t.Run("a missing API stays reported, and is not announced again, when a reconcile did not get that far", func(t *testing.T) {
		h := healthyHarness(t)
		h.r.ServiceMonitorEnabled = true
		h.applyError = serviceMonitorAPIGone
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("ServiceMonitorApplied", metav1.ConditionFalse, "ServiceMonitorAPIMissing", "")
		if events := drain(h.recorder); len(events) != 1 || !strings.HasPrefix(events[0], "Warning ServiceMonitorAPIMissing ") {
			t.Fatalf("events when it went missing: %q", events)
		}

		// The Secret is replaced by deleting and creating it.
		if err := h.client.Delete(context.Background(), unitSecret()); err != nil {
			t.Fatal(err)
		}
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("Ready", metav1.ConditionFalse, "SecretNotFound", "")
		h.expectCondition("ServiceMonitorApplied", metav1.ConditionFalse, "ServiceMonitorAPIMissing", "")
		if err := h.client.Create(context.Background(), unitSecret()); err != nil {
			t.Fatal(err)
		}
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("Ready", metav1.ConditionTrue, "PodRunning", "")
		h.expectCondition("ServiceMonitorApplied", metav1.ConditionFalse, "ServiceMonitorAPIMissing", "")
		for _, e := range drain(h.recorder) {
			if strings.Contains(e, "ServiceMonitorAPIMissing") {
				t.Errorf("announced again although the API never came back: %q", e)
			}
		}
	})

	t.Run("not made up for a monitor that never had it", func(t *testing.T) {
		h := newReconcileHarness(t, unitMonitor())
		h.r.ServiceMonitorEnabled = true
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("Ready", metav1.ConditionFalse, "SecretNotFound", "")
		if cond := condition(h); cond != nil {
			t.Errorf("got %+v", cond)
		}
	})
}

// The operator puts no finalizer on a monitor, and lets go of one an earlier
// release left when the monitor is deleted.
func TestReconcile_Finalizer(t *testing.T) {
	t.Run("none is added", func(t *testing.T) {
		h := newReconcileHarness(t, unitMonitor(), unitSecret())
		for range 2 {
			if _, err := h.reconcile(); err != nil {
				t.Fatal(err)
			}
		}
		if m := h.monitor(); len(m.Finalizers) != 0 {
			t.Errorf("finalizers: %v", m.Finalizers)
		}
	})

	// Taking it off a monitor that stays is the migration's: only that
	// reads the ownership record that says whose the image tag is.
	t.Run("one that is there stays while the monitor does", func(t *testing.T) {
		monitor := unitMonitor()
		monitor.Finalizers = []string{finalizerName}
		h := newReconcileHarness(t, monitor, unitSecret())
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if m := h.monitor(); !slices.Equal(m.Finalizers, []string{finalizerName}) {
			t.Errorf("finalizers: %v", m.Finalizers)
		}
	})

	t.Run("a deleted monitor is let go, and nothing else is done", func(t *testing.T) {
		monitor := unitMonitor()
		monitor.Finalizers = []string{"example.com/backup", finalizerName}
		h := newReconcileHarness(t, monitor, unitSecret())
		if err := h.client.Delete(context.Background(), monitor); err != nil {
			t.Fatal(err)
		}
		version := h.monitor().ResourceVersion

		result, err := h.reconcile()
		if err != nil || result != (ctrl.Result{}) {
			t.Errorf("got %+v, %v", result, err)
		}
		m := h.monitor()
		if !slices.Equal(m.Finalizers, []string{"example.com/backup"}) {
			t.Errorf("finalizers: %v, want only the one that is not the operator's", m.Finalizers)
		}
		// A merge patch carries the whole list: it only goes through against
		// the list it was computed from.
		if len(h.monitorPatches) != 1 || !strings.Contains(h.monitorPatches[0], `"resourceVersion":"`+version+`"`) {
			t.Errorf("sent %q, want one patch that names version %s", h.monitorPatches, version)
		}
		if len(h.applied) != 0 || h.statusWrites != 0 || len(drain(h.recorder)) != 0 {
			t.Errorf("a monitor on its way out was worked on: applied %v, %d status writes", h.applied, h.statusWrites)
		}
	})

	// A failure that is passed over leaves the monitor in Terminating until
	// something else happens to bring it back: a change of its finalizers
	// does not trigger a reconcile.
	t.Run("a release that fails is tried again", func(t *testing.T) {
		monitor := unitMonitor()
		monitor.Finalizers = []string{finalizerName}
		h := newReconcileHarness(t, monitor, unitSecret())
		if err := h.client.Delete(context.Background(), monitor); err != nil {
			t.Fatal(err)
		}
		gr := schema.GroupResource{Group: "pgpilot.io", Resource: "pgpilotmonitors"}

		h.monitorPatchError = apierrors.NewConflict(gr, "db", errors.New("the object has been modified"))
		if _, err := h.reconcile(); !apierrors.IsConflict(err) {
			t.Errorf("got %v, want the conflict handed to the work queue", err)
		}
		// Gone already: nothing left to release.
		h.monitorPatchError = apierrors.NewNotFound(gr, "db")
		if result, err := h.reconcile(); err != nil || result != (ctrl.Result{}) {
			t.Errorf("got %+v, %v", result, err)
		}
	})

	t.Run("a deleted monitor without the finalizer needs nothing", func(t *testing.T) {
		monitor := unitMonitor()
		monitor.Finalizers = []string{"example.com/backup"}
		h := newReconcileHarness(t, monitor, unitSecret())
		if err := h.client.Delete(context.Background(), monitor); err != nil {
			t.Fatal(err)
		}
		version := h.monitor().ResourceVersion
		if result, err := h.reconcile(); err != nil || result != (ctrl.Result{}) {
			t.Errorf("got %+v, %v", result, err)
		}
		if got := h.monitor().ResourceVersion; got != version {
			t.Error("the monitor was written to")
		}
	})
}
