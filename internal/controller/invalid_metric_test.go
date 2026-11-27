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
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

// The API server checks a metric definition when it is written. One that was
// stored before a rule existed is still handed to the operator as it is; the
// fake API server of these tests stands in for that, it checks nothing.

func usableMetric(name string) pgpilotv1.MetricDefinition {
	return pgpilotv1.MetricDefinition{Name: name, SQLs: map[string]string{"14": "SELECT 1"}}
}

// wildcardMetric is what was common before the rule: a statement for
// PostgreSQL 14 and one "for everything else", which was never used.
func wildcardMetric(name string) pgpilotv1.MetricDefinition {
	return pgpilotv1.MetricDefinition{Name: name, SQLs: map[string]string{"14": "SELECT 1", "*": "SELECT 2"}}
}

func unitLibrary(name string, metrics ...pgpilotv1.MetricDefinition) *pgpilotv1.PgpilotMetricLibrary {
	lib := &pgpilotv1.PgpilotMetricLibrary{}
	lib.Name, lib.Namespace, lib.Generation = name, "team", 2
	lib.Spec.Metrics = metrics
	return lib
}

// monitorUsing returns the unit monitor with these libraries, in this order.
func monitorUsing(libraries ...string) *pgpilotv1.PgpilotMonitor {
	monitor := unitMonitor()
	for _, name := range libraries {
		monitor.Spec.Metrics.FromLibraries = append(monitor.Spec.Metrics.FromLibraries, pgpilotv1.LibraryRef{Name: name})
	}
	return monitor
}

const wildcardProblem = `sqls key "*" is not a PostgreSQL major version written as a plain integer`

func TestReconcile_InvalidMetric(t *testing.T) {
	t.Run("in a library", func(t *testing.T) {
		h := newReconcileHarness(t, monitorUsing("extra"), unitSecret(),
			unitLibrary("extra", usableMetric("fine"), wildcardMetric("queue_depth")))

		result, err := h.reconcile()
		// A mistake in what is stored: trying again cannot fix it, and a
		// change of the library or of the monitor starts a reconcile.
		if err != nil || result != (ctrl.Result{}) {
			t.Errorf("got %+v, %v; want neither an error nor another look", result, err)
		}
		if len(h.applied) != 0 {
			t.Errorf("applied %v from definitions that cannot be used", h.applied)
		}
		const want = `PgpilotMetricLibrary extra cannot be used: metric "queue_depth": ` + wildcardProblem
		h.expectCondition("Ready", metav1.ConditionFalse, "InvalidMetric", want)
		h.expectCondition("Ready", metav1.ConditionFalse, "InvalidMetric",
			"Nothing is changed for this monitor until that is corrected")
		h.expectCondition("ConfigGenerated", metav1.ConditionFalse, "InvalidMetric", want)
		events := drain(h.recorder)
		if len(events) != 1 || !strings.HasPrefix(events[0], "Warning InvalidMetric PgpilotMetricLibrary extra cannot be used") {
			t.Errorf("events: %q", events)
		}

		// Still so at the next look: nothing new to say.
		writes := h.statusWrites
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if again := drain(h.recorder); len(again) != 0 || h.statusWrites != writes {
			t.Errorf("reported again: events %q, %d status writes", again, h.statusWrites-writes)
		}
	})

	t.Run("in the monitor itself", func(t *testing.T) {
		monitor := unitMonitor()
		lag := usableMetric("lag")
		lag.MasterOnly, lag.StandbyOnly = true, true
		monitor.Spec.Metrics.Custom = []pgpilotv1.MetricDefinition{usableMetric("fine"), lag}
		h := newReconcileHarness(t, monitor, unitSecret())

		if result, err := h.reconcile(); err != nil || result != (ctrl.Result{}) || len(h.applied) != 0 {
			t.Errorf("got %+v, %v, applied %v", result, err, h.applied)
		}
		h.expectCondition("Ready", metav1.ConditionFalse, "InvalidMetric",
			`spec.metrics.custom cannot be used: metric "lag": masterOnly and standbyOnly are both set`)
		h.expectCondition("Ready", metav1.ConditionFalse, "InvalidMetric",
			"Nothing is changed for this monitor until that is corrected")
	})

	// One thing to correct at a time, and the same one at every look.
	t.Run("the first library that cannot be used is the one named, before the monitor's own", func(t *testing.T) {
		monitor := monitorUsing("fine", "second", "first")
		monitor.Spec.Metrics.Custom = []pgpilotv1.MetricDefinition{wildcardMetric("inline")}
		h := newReconcileHarness(t, monitor, unitSecret(),
			unitLibrary("fine", usableMetric("ok")),
			unitLibrary("first", wildcardMetric("b")),
			unitLibrary("second", wildcardMetric("a")))
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("Ready", metav1.ConditionFalse, "InvalidMetric",
			`PgpilotMetricLibrary second cannot be used: metric "a": `+wildcardProblem)
		if message := meta.FindStatusCondition(h.monitor().Status.Conditions, "Ready").Message; strings.Contains(message, "first") ||
			strings.Contains(message, "spec.metrics.custom") {
			t.Errorf("more than one thing to correct in one message: %q", message)
		}
	})

	t.Run("a collector that runs is left as it was, and brought up to date once the definition is corrected", func(t *testing.T) {
		h := newReconcileHarness(t, monitorUsing("extra"), unitSecret(), runningPod("pgpilot-db-abc"),
			unitLibrary("extra", usableMetric("queue_depth")))
		h.deploymentStatus = rolledOut
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("Ready", metav1.ConditionTrue, "PodRunning", "")
		versions := func() string {
			t.Helper()
			cm, dep := h.storedConfigMap(), h.storedDeployment()
			return cm.ResourceVersion + "/" + dep.ResourceVersion
		}
		before := versions()
		setMetrics := func(metrics ...pgpilotv1.MetricDefinition) {
			t.Helper()
			var lib pgpilotv1.PgpilotMetricLibrary
			if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: "team", Name: "extra"}, &lib); err != nil {
				t.Fatal(err)
			}
			lib.Spec.Metrics = metrics
			if err := h.client.Update(context.Background(), &lib); err != nil {
				t.Fatal(err)
			}
		}

		setMetrics(wildcardMetric("queue_depth"))
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("Ready", metav1.ConditionFalse, "InvalidMetric", "PgpilotMetricLibrary extra cannot be used")
		if len(h.applied) != 0 || versions() != before {
			t.Errorf("applied %v; the ConfigMap and the Deployment were %s and are %s", h.applied, before, versions())
		}

		setMetrics(usableMetric("queue_depth"), usableMetric("another"))
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(h.applied, ","); got != "ConfigMap,Deployment,Service" {
			t.Errorf("applied %s", got)
		}
		if cond := meta.FindStatusCondition(h.monitor().Status.Conditions, "Ready"); cond.Reason == "InvalidMetric" {
			t.Errorf("still reported: %+v", cond)
		}
		h.expectCondition("ConfigGenerated", metav1.ConditionTrue, "ConfigMapUpdated", "")
	})

	// A library stored before it had to hold a definition. It adds nothing,
	// and takes nothing away from what else the monitor collects.
	t.Run("a library without a single definition is nothing to refuse", func(t *testing.T) {
		h := newReconcileHarness(t, monitorUsing("empty"), unitSecret(), unitLibrary("empty"))
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(h.applied, ","); got != "ConfigMap,Deployment,Service" {
			t.Errorf("applied %s", got)
		}
	})
}

// Names have to be unique within one list. Across lists the later
// definition wins (docs/architecture.md, "Metric merge order"): that is
// how a monitor overrides a metric of a library, and no duplicate.
func TestReconcile_TheSameNameInTwoListsIsAnOverride(t *testing.T) {
	from := func(name, source string) pgpilotv1.MetricDefinition {
		return pgpilotv1.MetricDefinition{Name: name, SQLs: map[string]string{"14": "SELECT 1 AS from_" + source}}
	}
	monitor := monitorUsing("one", "two")
	monitor.Spec.Metrics.Custom = []pgpilotv1.MetricDefinition{from("shared", "inline")}
	h := newReconcileHarness(t, monitor, unitSecret(),
		unitLibrary("one", from("shared", "one")),
		unitLibrary("two", from("shared", "two"), from("other", "two_too")))
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.applied, ","); got != "ConfigMap,Deployment,Service" {
		t.Errorf("applied %q", got)
	}
	h.expectCondition("ConfigGenerated", metav1.ConditionTrue, "ConfigMapUpdated", "")
	rendered := h.storedConfigMap().Data["metrics.yaml"]
	if !strings.Contains(rendered, "from_inline") || !strings.Contains(rendered, "from_two_too") ||
		strings.Contains(rendered, "from_one") || strings.Contains(rendered, "from_two\n") {
		t.Errorf("rendered:\n%s", rendered)
	}
}

// A status the API server refuses as invalid belongs to a monitor that
// breaks some rule, not necessarily one about metrics. What cannot be said
// in the status is said in an event only when it is a problem.
func TestReconcile_AStatusRefusedAsInvalid(t *testing.T) {
	// Before Kubernetes 1.33 the API server refuses the status of a monitor
	// whose own spec breaks a rule. The monitor keeps the status it had, and
	// the event is what is left to say that nothing is done for it.
	t.Run("in the monitor itself, where its status cannot be written", func(t *testing.T) {
		monitor := unitMonitor()
		monitor.Spec.Metrics.Custom = []pgpilotv1.MetricDefinition{wildcardMetric("inline")}
		monitor.Status.Ready = true
		monitor.Status.Conditions = []metav1.Condition{{
			Type: "Ready", Status: metav1.ConditionTrue, Reason: "PodRunning", Message: "as an earlier release left it",
			LastTransitionTime: metav1.Now(),
		}}
		h := newReconcileHarness(t, monitor, unitSecret())
		h.statusError = invalid("PgpilotMonitor", "db", "spec.metrics.custom[0].sqls: Invalid value")

		const want = `Warning InvalidMetric spec.metrics.custom cannot be used: metric "inline": ` + wildcardProblem
		// At every attempt: an event lives for an hour, and the status
		// keeps saying that the monitor is ready.
		for attempt := range 2 {
			result, err := h.reconcile()
			if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "update status: ") || result != (ctrl.Result{}) {
				t.Errorf("attempt %d: got %+v, %v", attempt, result, err)
			}
			if events := drain(h.recorder); len(events) != 1 || !strings.HasPrefix(events[0], want) ||
				!strings.Contains(events[0], "Nothing is changed for this monitor") {
				t.Errorf("attempt %d: events %q", attempt, events)
			}
		}
		if len(h.applied) != 0 {
			t.Errorf("applied %v", h.applied)
		}
		stored := h.monitor()
		if cond := meta.FindStatusCondition(stored.Status.Conditions, "Ready"); !stored.Status.Ready || cond == nil ||
			cond.Reason != "PodRunning" || cond.Message != "as an earlier release left it" {
			t.Errorf("the stored status is no longer what it was: ready %v, %+v", stored.Status.Ready, cond)
		}
	})

	refused := invalid("PgpilotMonitor", "db", "spec.sinks: Invalid value")

	t.Run("a problem of another kind is said in an event as well", func(t *testing.T) {
		h := newReconcileHarness(t, unitMonitor())
		h.statusError = refused
		if _, err := h.reconcile(); !apierrors.IsInvalid(err) {
			t.Errorf("got %v", err)
		}
		if events := drain(h.recorder); len(events) != 1 || !strings.HasPrefix(events[0], "Warning SecretNotFound ") {
			t.Errorf("events: %q", events)
		}
	})

	t.Run("a collector on its way up is nothing to warn about", func(t *testing.T) {
		h := newReconcileHarness(t, unitMonitor(), unitSecret())
		h.statusError = refused
		if _, err := h.reconcile(); !apierrors.IsInvalid(err) {
			t.Errorf("got %v", err)
		}
		if got := strings.Join(h.applied, ","); got != "ConfigMap,Deployment,Service" {
			t.Errorf("applied %q", got)
		}
		if events := drain(h.recorder); len(events) != 0 {
			t.Errorf("events: %q", events)
		}
	})

	t.Run("neither is a monitor that is ready", func(t *testing.T) {
		h := newReconcileHarness(t, unitMonitor(), unitSecret(), runningPod("pgpilot-db-abc"))
		h.deploymentStatus = rolledOut
		h.statusError = refused
		if _, err := h.reconcile(); !apierrors.IsInvalid(err) {
			t.Errorf("got %v", err)
		}
		if events := drain(h.recorder); len(events) != 0 {
			t.Errorf("events: %q", events)
		}
	})

	t.Run("any other refusal of the status says nothing", func(t *testing.T) {
		h := newReconcileHarness(t, unitMonitor())
		h.statusError = apierrors.NewServiceUnavailable("etcd leader changed")
		if _, err := h.reconcile(); !errors.Is(err, h.statusError) {
			t.Errorf("got %v", err)
		}
		if events := drain(h.recorder); len(events) != 0 {
			t.Errorf("events: %q", events)
		}
	})
}

// libraryHarness runs the library reconciler against a fake API server.
type libraryHarness struct {
	t      *testing.T
	client client.Client
	r      *PgpilotMetricLibraryReconciler
	key    client.ObjectKey

	// patches holds the status writes, as "<subresource> <patch type> <body>";
	// patchError is what the API server answers to them.
	patches    []string
	patchError error
	// beforeWrite runs once, when the next status write is about to reach
	// the API server.
	beforeWrite func()
}

func newLibraryHarness(t *testing.T, lib *pgpilotv1.PgpilotMetricLibrary) *libraryHarness {
	t.Helper()
	unitScheme(t)
	h := &libraryHarness{t: t, key: client.ObjectKeyFromObject(lib)}
	h.client = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(lib).WithStatusSubresource(lib).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				data, err := patch.Data(obj)
				if err != nil {
					return err
				}
				h.patches = append(h.patches, sub+" "+string(patch.Type())+" "+string(data))
				if h.patchError != nil {
					return h.patchError
				}
				if hook := h.beforeWrite; hook != nil {
					h.beforeWrite = nil
					hook()
				}
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
			// A full update sends the spec back as the Go struct serialises
			// it, and replaces whatever status is stored by then.
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				t.Error("the status was sent with a full update")
				return nil
			},
		}).Build()
	h.r = &PgpilotMetricLibraryReconciler{Client: h.client, Scheme: scheme.Scheme}
	return h
}

func (h *libraryHarness) reconcile() (ctrl.Result, error) {
	h.t.Helper()
	return h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: h.key})
}

func (h *libraryHarness) library() *pgpilotv1.PgpilotMetricLibrary {
	h.t.Helper()
	var lib pgpilotv1.PgpilotMetricLibrary
	if err := h.client.Get(context.Background(), h.key, &lib); err != nil {
		h.t.Fatal(err)
	}
	return &lib
}

// expectValid checks the Valid condition and the rest of the stored status.
func (h *libraryHarness) expectValid(status metav1.ConditionStatus, reason, messagePart string, metrics int32) {
	h.t.Helper()
	lib := h.library()
	cond := meta.FindStatusCondition(lib.Status.Conditions, "Valid")
	if cond == nil {
		h.t.Fatalf("no Valid condition: %+v", lib.Status)
	}
	if cond.Status != status || cond.Reason != reason || !strings.Contains(cond.Message, messagePart) {
		h.t.Errorf("Valid is %s/%s %q, want %s/%s with %q", cond.Status, cond.Reason, cond.Message, status, reason, messagePart)
	}
	if cond.ObservedGeneration != lib.Generation || lib.Status.ObservedGeneration != lib.Generation {
		h.t.Errorf("observed generation %d and %d, the library is of %d",
			cond.ObservedGeneration, lib.Status.ObservedGeneration, lib.Generation)
	}
	if lib.Status.MetricCount != metrics {
		h.t.Errorf("metricCount %d, want %d", lib.Status.MetricCount, metrics)
	}
}

func TestLibraryReconcile_SaysWhetherTheDefinitionsCanBeUsed(t *testing.T) {
	t.Run("all of them", func(t *testing.T) {
		h := newLibraryHarness(t, unitLibrary("extra", usableMetric("one"), usableMetric("two")))
		if result, err := h.reconcile(); err != nil || result != (ctrl.Result{}) {
			t.Fatalf("got %+v, %v", result, err)
		}
		h.expectValid(metav1.ConditionTrue, "Valid", "Metric definitions that can be used: 2", 2)
	})

	// The same rules the monitor reconciler goes by, so that the library
	// says what its monitors will say.
	t.Run("one that cannot", func(t *testing.T) {
		h := newLibraryHarness(t, unitLibrary("extra", usableMetric("fine"), wildcardMetric("queue_depth")))
		if result, err := h.reconcile(); err != nil || result != (ctrl.Result{}) {
			t.Fatalf("got %+v, %v", result, err)
		}
		h.expectValid(metav1.ConditionFalse, "InvalidMetric", `metric "queue_depth": `+wildcardProblem, 2)
		h.expectValid(metav1.ConditionFalse, "InvalidMetric",
			"Monitors that use this library report InvalidMetric and are not changed until it is corrected", 2)

		// Corrected: the next generation is judged anew.
		lib := h.library()
		lib.Spec.Metrics = []pgpilotv1.MetricDefinition{usableMetric("queue_depth")}
		lib.Generation++
		if err := h.client.Update(context.Background(), lib); err != nil {
			t.Fatal(err)
		}
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectValid(metav1.ConditionTrue, "Valid", "Metric definitions that can be used: 1", 1)
	})

	// The CRD asks for at least one definition; a library stored before it
	// did can be without.
	t.Run("none at all", func(t *testing.T) {
		h := newLibraryHarness(t, unitLibrary("extra"))
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectValid(metav1.ConditionFalse, "Empty", "The library has no metric definitions", 0)
	})

	// "metrics: []" as it is stored: an empty list, not a missing one.
	t.Run("an empty list", func(t *testing.T) {
		lib := unitLibrary("extra")
		lib.Spec.Metrics = []pgpilotv1.MetricDefinition{}
		h := newLibraryHarness(t, lib)
		if h.library().Spec.Metrics == nil {
			t.Fatal("the fake API server hands back no list at all: this proves nothing")
		}
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectValid(metav1.ConditionFalse, "Empty", "The library has no metric definitions", 0)
	})

	// What the upgrade is about: an earlier release called every library
	// with an entry valid. The generation has not moved since, and the
	// definitions have to be judged all the same.
	asLeftByAnEarlierRelease := func(lib *pgpilotv1.PgpilotMetricLibrary) *pgpilotv1.PgpilotMetricLibrary {
		lib.Status.ObservedGeneration = lib.Generation
		lib.Status.MetricCount = int32(len(lib.Spec.Metrics)) //nolint:gosec // a handful
		lib.Status.Conditions = []metav1.Condition{{
			Type: "Valid", Status: metav1.ConditionTrue, ObservedGeneration: lib.Generation,
			Reason: "Valid", Message: "Library contains metric definitions", LastTransitionTime: metav1.Now(),
		}}
		return lib
	}

	t.Run("a status of an earlier release, over definitions that break a rule", func(t *testing.T) {
		h := newLibraryHarness(t, asLeftByAnEarlierRelease(unitLibrary("extra", wildcardMetric("queue_depth"))))
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectValid(metav1.ConditionFalse, "InvalidMetric", `metric "queue_depth": `+wildcardProblem, 1)
	})

	t.Run("a status of an earlier release, over definitions in order: the message is the current one", func(t *testing.T) {
		h := newLibraryHarness(t, asLeftByAnEarlierRelease(unitLibrary("extra", usableMetric("queue_depth"))))
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectValid(metav1.ConditionTrue, "Valid", "Metric definitions that can be used: 1", 1)
	})

	// A library that cannot be read is not a library that is gone.
	t.Run("a read that fails is handed to the work queue", func(t *testing.T) {
		unitScheme(t)
		boom := apierrors.NewServiceUnavailable("etcd leader changed")
		c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return boom
			},
		}).Build()
		r := &PgpilotMetricLibraryReconciler{Client: c, Scheme: scheme.Scheme}
		result, err := r.Reconcile(context.Background(),
			ctrl.Request{Namespace: "team", Name: "extra"})
		if !errors.Is(err, boom) || result != (ctrl.Result{}) {
			t.Errorf("got %+v, %v", result, err)
		}
	})

	t.Run("a library that is gone needs nothing", func(t *testing.T) {
		h := newLibraryHarness(t, unitLibrary("extra", usableMetric("one")))
		h.key.Name = "another"
		if result, err := h.reconcile(); err != nil || result != (ctrl.Result{}) || len(h.patches) != 0 {
			t.Errorf("got %+v, %v, wrote %q", result, err, h.patches)
		}
	})
}

func TestLibraryReconcile_HowTheStatusIsWritten(t *testing.T) {
	t.Run("as a patch of the status alone, against the version it was worked out from, and only when it changed", func(t *testing.T) {
		h := newLibraryHarness(t, unitLibrary("extra", usableMetric("one")))
		version := h.library().ResourceVersion
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if len(h.patches) != 1 {
			t.Fatalf("status writes: %q", h.patches)
		}
		sent := h.patches[0]
		if !strings.HasPrefix(sent, `status application/merge-patch+json {`) ||
			!strings.Contains(sent, `"resourceVersion":"`+version+`"`) || !strings.Contains(sent, `"metricCount":1`) ||
			strings.Contains(sent, `"spec"`) {
			t.Errorf("sent %s", sent)
		}

		// The write is seen as a change of the library and reconciled.
		for range 3 {
			if result, err := h.reconcile(); err != nil || result != (ctrl.Result{}) {
				t.Fatalf("got %+v, %v", result, err)
			}
		}
		if len(h.patches) != 1 {
			t.Errorf("a status that did not change was written again: %q", h.patches[1:])
		}
	})

	gr := schema.GroupResource{Group: "pgpilot.io", Resource: "pgpilotmetriclibraries"}

	// Not a failure to count or to log: it is looked at again as it is then.
	t.Run("a library that changed since it was read is looked at again in a moment", func(t *testing.T) {
		h := newLibraryHarness(t, unitLibrary("extra", usableMetric("one")))
		h.beforeWrite = func() {
			lib := h.library()
			lib.Spec.Metrics = []pgpilotv1.MetricDefinition{wildcardMetric("one")}
			lib.Generation++
			if err := h.client.Update(context.Background(), lib); err != nil {
				t.Fatal(err)
			}
		}
		result, err := h.reconcile()
		if err != nil || result != (ctrl.Result{RequeueAfter: staleReadRetry}) {
			t.Errorf("got %+v, %v; want another look after %s and no error", result, err, staleReadRetry)
		}
		// What was worked out from the earlier version must not have landed
		// on the later one.
		if conds := h.library().Status.Conditions; len(conds) != 0 {
			t.Errorf("the status of a version that was never looked at: %+v", conds)
		}
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectValid(metav1.ConditionFalse, "InvalidMetric", wildcardProblem, 1)
	})

	t.Run("a conflict answered by the API server likewise", func(t *testing.T) {
		h := newLibraryHarness(t, unitLibrary("extra", usableMetric("one")))
		h.patchError = apierrors.NewConflict(gr, "extra", errors.New("the object has been modified"))
		if result, err := h.reconcile(); err != nil || result != (ctrl.Result{RequeueAfter: staleReadRetry}) {
			t.Errorf("got %+v, %v", result, err)
		}
	})

	t.Run("a library deleted meanwhile is no error", func(t *testing.T) {
		h := newLibraryHarness(t, unitLibrary("extra", usableMetric("one")))
		h.patchError = apierrors.NewNotFound(gr, "extra")
		if result, err := h.reconcile(); err != nil || result != (ctrl.Result{}) {
			t.Errorf("got %+v, %v", result, err)
		}
	})

	t.Run("any other failure is handed to the work queue", func(t *testing.T) {
		h := newLibraryHarness(t, unitLibrary("extra", usableMetric("one")))
		boom := apierrors.NewServiceUnavailable("etcd leader changed")
		h.patchError = boom
		result, err := h.reconcile()
		if !errors.Is(err, boom) || !strings.Contains(err.Error(), "update status: ") || result != (ctrl.Result{}) {
			t.Errorf("got %+v, %v", result, err)
		}
	})

	// Before Kubernetes 1.33 the API server refuses the status of a library
	// that breaks a rule. The error in the operator's log is the one trace
	// of that (the upgrade notes quote it): it must not be taken for a
	// conflict and retried every second without a word.
	t.Run("a status refused as invalid is such a failure", func(t *testing.T) {
		h := newLibraryHarness(t, unitLibrary("extra", wildcardMetric("queue_depth")))
		h.patchError = apierrors.NewInvalid(schema.GroupKind{Group: "pgpilot.io", Kind: "PgpilotMetricLibrary"}, "extra", nil)
		result, err := h.reconcile()
		if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "update status: ") || result != (ctrl.Result{}) {
			t.Errorf("got %+v, %v", result, err)
		}
	})
}
