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

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

// reconcileHarness runs the monitor reconciler against a fake API server
// whose behaviour a test can bend: which generated object is refused and how,
// and what the Deployment reports once it is applied.
type reconcileHarness struct {
	t        *testing.T
	client   client.Client
	recorder *record.FakeRecorder
	r        *PgpilotMonitorReconciler
	key      client.ObjectKey

	// applyError is asked before every server-side apply of a generated
	// object; a non-nil result is what the API server answers.
	applyError func(obj client.Object) error
	// deploymentStatus is what the API server returns as the status of the
	// Deployment after an apply, with observedGeneration set to the
	// Deployment's generation.
	deploymentStatus *appsv1.DeploymentStatus
	// statusWrites counts the writes of the monitor's status; statusError
	// makes them fail. statusPatches holds what was sent.
	statusWrites  int
	statusError   error
	statusPatches []string
	// beforeStatusWrite runs once, when the next status write is about to
	// reach the API server: the place of a write by someone else that the
	// reconcile has not seen.
	beforeStatusWrite func()
	// dropStatusWrite makes the API server answer a status write with
	// success and the object unchanged.
	dropStatusWrite bool
	// cached, when set, is what a read of the monitor returns: a cache that
	// has not caught up.
	cached *pgpilotv1.PgpilotMonitor
	// podListError makes listing the pods fail.
	podListError error
	// read lists the kinds of the objects the last reconcile read one by
	// one; getError is asked before each such read.
	read     []string
	getError func(obj client.Object) error
	// monitorPatches holds the patches sent for the monitor itself, not for
	// its status; monitorPatchError is what the API server answers to them.
	monitorPatches    []string
	monitorPatchError error
	// deleteError is asked before every delete; deleted lists what was
	// deleted, as "<Go type> name" ("*v1.Service pgpilot-db"), and
	// deletedIf what each delete was conditional on, as "Kind uid".
	deleteError func(obj client.Object) error
	deleted     []string
	deletedIf   []string
	// applied lists the kinds applied, in order.
	applied []string
}

func newReconcileHarness(t *testing.T, monitor *pgpilotv1.PgpilotMonitor, objs ...client.Object) *reconcileHarness {
	t.Helper()
	unitScheme(t)
	h := &reconcileHarness{t: t, recorder: record.NewFakeRecorder(50), key: client.ObjectKeyFromObject(monitor)}
	h.client = fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(append([]client.Object{monitor}, objs...)...).
		WithStatusSubresource(monitor).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if m, isMonitor := obj.(*pgpilotv1.PgpilotMonitor); isMonitor && h.cached != nil {
					h.cached.DeepCopyInto(m)
					return nil
				}
				h.read = append(h.read, kindOf(obj))
				if h.getError != nil {
					if err := h.getError(obj); err != nil {
						return err
					}
				}
				return c.Get(ctx, key, obj, opts...)
			},
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, isPods := list.(*corev1.PodList); isPods && h.podListError != nil {
					return h.podListError
				}
				return c.List(ctx, list, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if h.deleteError != nil {
					if err := h.deleteError(obj); err != nil {
						return err
					}
				}
				if err := c.Delete(ctx, obj, opts...); err != nil {
					return err
				}
				h.deleted = append(h.deleted, fmt.Sprintf("%T %s", obj, obj.GetName()))
				h.deletedIf = append(h.deletedIf, kindOf(obj)+" "+uidPrecondition(opts))
				return nil
			},
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if patch.Type() != "application/apply-patch+yaml" {
					if _, isMonitor := obj.(*pgpilotv1.PgpilotMonitor); isMonitor {
						if data, err := patch.Data(obj); err == nil {
							h.monitorPatches = append(h.monitorPatches, string(data))
						}
						if h.monitorPatchError != nil {
							return h.monitorPatchError
						}
					}
					return c.Patch(ctx, obj, patch, opts...)
				}
				kind := obj.GetObjectKind().GroupVersionKind().Kind
				h.applied = append(h.applied, kind)
				if h.applyError != nil {
					if err := h.applyError(obj); err != nil {
						return err
					}
				}
				if err := c.Patch(ctx, obj, patch, opts...); err != nil {
					return err
				}
				if dep, ok := obj.(*appsv1.Deployment); ok && h.deploymentStatus != nil {
					dep.Status = *h.deploymentStatus.DeepCopy()
					dep.Status.ObservedGeneration = dep.Generation
				}
				return nil
			},
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if _, isMonitor := obj.(*pgpilotv1.PgpilotMonitor); isMonitor && sub == "status" {
					h.statusWrites++
					if data, err := patch.Data(obj); err == nil {
						h.statusPatches = append(h.statusPatches, string(data))
					}
					if h.statusError != nil {
						return h.statusError
					}
					if h.dropStatusWrite {
						return nil
					}
					if hook := h.beforeStatusWrite; hook != nil {
						h.beforeStatusWrite = nil
						hook()
					}
				}
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	h.r = &PgpilotMonitorReconciler{Client: h.client, Scheme: scheme.Scheme, Recorder: h.recorder}
	return h
}

func (h *reconcileHarness) reconcile() (ctrl.Result, error) {
	h.t.Helper()
	h.applied, h.deleted, h.deletedIf, h.read = nil, nil, nil, nil
	return h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: h.key})
}

// uidPrecondition returns the UID a delete is conditional on. The fake client
// does not act on it, so only what was sent can be looked at.
func uidPrecondition(opts []client.DeleteOption) string {
	options := &client.DeleteOptions{}
	options.ApplyOptions(opts)
	if options.Preconditions == nil || options.Preconditions.UID == nil {
		return "<unconditional>"
	}
	return string(*options.Preconditions.UID)
}

// monitor returns the monitor as the API server holds it, whatever the
// reconciler's cache is made to say.
func (h *reconcileHarness) monitor() *pgpilotv1.PgpilotMonitor {
	h.t.Helper()
	cached := h.cached
	h.cached = nil
	defer func() { h.cached = cached }()
	var m pgpilotv1.PgpilotMonitor
	if err := h.client.Get(context.Background(), h.key, &m); err != nil {
		h.t.Fatal(err)
	}
	return &m
}

// expectCondition checks one condition of the stored monitor.
func (h *reconcileHarness) expectCondition(condType string, status metav1.ConditionStatus, reason, messagePart string) {
	h.t.Helper()
	m := h.monitor()
	cond := meta.FindStatusCondition(m.Status.Conditions, condType)
	if cond == nil {
		h.t.Errorf("condition %s is missing; conditions: %+v", condType, m.Status.Conditions)
		return
	}
	if cond.Status != status || cond.Reason != reason || !strings.Contains(cond.Message, messagePart) {
		h.t.Errorf("condition %s is %s/%s %q, want %s/%s with %q",
			condType, cond.Status, cond.Reason, cond.Message, status, reason, messagePart)
	}
	if cond.ObservedGeneration != m.Generation {
		h.t.Errorf("condition %s is of generation %d, the monitor of %d", condType, cond.ObservedGeneration, m.Generation)
	}
}

func unitSecret() *corev1.Secret {
	secret := &corev1.Secret{Data: map[string][]byte{"username": []byte("u"), "password": []byte("p")}}
	secret.Name, secret.Namespace = "db-creds", "team"
	return secret
}

var rolledOut = &appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}

func runningPod(name string) *corev1.Pod {
	pod := podAt(name, 1, podReady)
	pod.Namespace = "team"
	pod.Labels = builder.SystemLabels("db")
	return &pod
}

// healthyHarness returns a monitor with credentials from a Secret that has
// been reconciled once and is ready, its events read.
func healthyHarness(t *testing.T) *reconcileHarness {
	t.Helper()
	h := newReconcileHarness(t, unitMonitor(), unitSecret(), runningPod("pgpilot-db-abc"))
	h.deploymentStatus = rolledOut
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	if !h.monitor().Status.Ready {
		t.Fatal("the monitor did not become ready")
	}
	drain(h.recorder)
	return h
}

// starting is what a Deployment reports whose only pod is not available yet.
var starting = &appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, UnavailableReplicas: 1}

// invalid is the answer of an API server that refuses an object for good.
func invalid(kind, name, reason string) error {
	return apierrors.NewInvalid(schema.GroupKind{Kind: kind}, name,
		field.ErrorList{field.Invalid(field.NewPath("spec"), "x", reason)})
}

func TestReconcile_HealthyMonitor(t *testing.T) {
	h := newReconcileHarness(t, unitMonitor(), unitSecret(), runningPod("pgpilot-db-abc"))
	h.deploymentStatus = rolledOut

	result, err := h.reconcile()
	if err != nil {
		t.Fatal(err)
	}
	// Credentials come from a Secret: come back to read it again.
	if result.RequeueAfter < 5*time.Minute || result.RequeueAfter > 5*time.Minute+30*time.Second {
		t.Errorf("RequeueAfter = %s, want the credentials recheck", result.RequeueAfter)
	}
	if got := strings.Join(h.applied, ","); got != "ConfigMap,Deployment,Service" {
		t.Errorf("applied %s", got)
	}

	m := h.monitor()
	if !m.Status.Ready || m.Status.ObservedGeneration != m.Generation || m.Status.PodName != "pgpilot-db-abc" ||
		m.Status.ConfigHash == "" || m.Status.LastReconciled == nil {
		t.Errorf("status: %+v", m.Status)
	}
	h.expectCondition("Ready", metav1.ConditionTrue, "PodRunning", "pgwatch pod is running")
	h.expectCondition("ConfigGenerated", metav1.ConditionTrue, "ConfigMapUpdated", "up to date")
	h.expectCondition("CredentialsAvailable", metav1.ConditionTrue, "CredentialsFound", "db-creds")
	if got := drain(h.recorder); len(got) != 2 ||
		!strings.HasPrefix(got[0], "Normal ConfigMapUpdated ") || got[1] != "Normal PodRunning pgwatch pod is running" {
		t.Errorf("events: %q", got)
	}
	// The hash of the configuration is on the pod template: that is what
	// starts a new pod when the configuration changes.
	var dep appsv1.Deployment
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: "team", Name: "pgpilot-db"}, &dep); err != nil {
		t.Fatal(err)
	}
	if got := dep.Spec.Template.Annotations[builder.AnnotationConfigHash]; got == "" || got != m.Status.ConfigHash {
		t.Errorf("the pod template carries the config hash %q, the status says %q", got, m.Status.ConfigHash)
	}

	// A second look at an unchanged monitor says nothing and writes nothing.
	writes, reconciledAt := h.statusWrites, m.Status.LastReconciled
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	if h.statusWrites != writes {
		t.Errorf("the status was written again (%d writes) although nothing changed", h.statusWrites-writes)
	}
	if got := drain(h.recorder); len(got) != 0 {
		t.Errorf("events on a reconcile that changed nothing: %q", got)
	}
	if !h.monitor().Status.LastReconciled.Equal(reconciledAt) {
		t.Error("lastReconciled moved although the status did not change")
	}
}

// The rechecks of many monitors must not all come due in the same instant.
func TestReconcile_CredentialsRecheckIsSpreadOut(t *testing.T) {
	h := healthyHarness(t)
	seen := map[time.Duration]bool{}
	for range 20 {
		result, err := h.reconcile()
		if err != nil {
			t.Fatal(err)
		}
		if result.RequeueAfter < 5*time.Minute || result.RequeueAfter > 5*time.Minute+30*time.Second {
			t.Fatalf("RequeueAfter = %s, want within ten percent above five minutes", result.RequeueAfter)
		}
		seen[result.RequeueAfter] = true
	}
	if len(seen) < 2 {
		t.Error("twenty rechecks all came due after the same interval")
	}
}

// Inline credentials cannot change without the spec changing: a healthy
// monitor that has them is left alone.
func TestReconcile_HealthyMonitorWithInlineCredentialsIsLeftAlone(t *testing.T) {
	monitor := unitMonitor()
	monitor.Spec.Database.CredentialsSecret = nil
	monitor.Spec.Database.Username, monitor.Spec.Database.Password = "u", "p"
	h := newReconcileHarness(t, monitor, runningPod("pgpilot-db-abc"))
	h.deploymentStatus = rolledOut

	result, err := h.reconcile()
	if err != nil || result != (ctrl.Result{}) {
		t.Errorf("got %+v, %v; want nothing to come back for", result, err)
	}
	h.expectCondition("Ready", metav1.ConditionTrue, "PodRunning", "")
	h.expectCondition("CredentialsAvailable", metav1.ConditionTrue, "InlineCredentials", "")
}

// The pods of another monitor, of a monitor of the same name elsewhere, and
// of something else that happens to carry the instance label are not this
// monitor's.
func TestReconcile_OnlyTheMonitorsOwnPodsCount(t *testing.T) {
	labelled := func(name, namespace string, labels map[string]string, age int) *corev1.Pod {
		pod := podAt(name, age, podReady)
		pod.Namespace, pod.Labels = namespace, labels
		return &pod
	}
	h := newReconcileHarness(t, unitMonitor(), unitSecret(), runningPod("pgpilot-db-abc"),
		labelled("pgpilot-other-xyz", "team", builder.SystemLabels("other"), 8),
		labelled("pgpilot-db-elsewhere", "other-team", builder.SystemLabels("db"), 7),
		labelled("helm-release-db", "team",
			map[string]string{builder.LabelAppInstance: "db", builder.LabelAppName: "postgresql"}, 9),
	)
	h.deploymentStatus = rolledOut
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	if got := h.monitor().Status.PodName; got != "pgpilot-db-abc" {
		t.Errorf("podName = %q, want the monitor's own pod", got)
	}
}

// One ready replica used to be enough for Ready=True, at the new generation.
// While a changed pod template is rolled out, and for good when that rollout
// cannot finish, the pod of the previous spec is that ready replica.
func TestReconcile_ReadyFollowsTheRolloutNotTheOldPod(t *testing.T) {
	h := newReconcileHarness(t, unitMonitor(), unitSecret(), runningPod("old"))
	h.deploymentStatus = rolledOut
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	drain(h.recorder)

	// The new pod cannot pull its image; the old one keeps running.
	h.deploymentStatus = &appsv1.DeploymentStatus{
		Replicas: 2, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, UnavailableReplicas: 1,
	}
	stuck := podAt("new", 2, waiting("ImagePullBackOff", "Back-off pulling image"))
	stuck.Namespace, stuck.Labels = "team", builder.SystemLabels("db")
	if err := h.client.Create(context.Background(), &stuck); err != nil {
		t.Fatal(err)
	}

	result, err := h.reconcile()
	if err != nil {
		t.Fatal(err)
	}
	// What holds the pod up is known; whether it still does only shows on
	// the pod, so the monitor is looked at again, at a slower pace.
	if result.RequeueAfter != time.Minute {
		t.Errorf("RequeueAfter = %s, want another look in a minute", result.RequeueAfter)
	}
	m := h.monitor()
	if m.Status.Ready {
		t.Error("status.ready is true while the pod of the current spec cannot start")
	}
	if m.Status.PodName != "old" {
		t.Errorf("podName = %q, want the pod that runs", m.Status.PodName)
	}
	h.expectCondition("Ready", metav1.ConditionFalse, "ImagePullBackOff", "Pod new cannot pull image")
	h.expectCondition("ConfigGenerated", metav1.ConditionTrue, "ConfigMapUpdated", "")
	if got := drain(h.recorder); len(got) != 1 || !strings.HasPrefix(got[0], "Warning ImagePullBackOff Pod new cannot pull image") {
		t.Errorf("events: %q", got)
	}

	// Looked at again; the event is not repeated.
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	if got := drain(h.recorder); len(got) != 0 {
		t.Errorf("the same state was announced again: %q", got)
	}

	// The image appears; the rollout completes.
	if err := h.client.Delete(context.Background(), &stuck); err != nil {
		t.Fatal(err)
	}
	h.deploymentStatus = rolledOut
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	if !h.monitor().Status.Ready {
		t.Error("not ready after the rollout completed")
	}
	if got := drain(h.recorder); len(got) != 1 || got[0] != "Normal PodRunning pgwatch pod is running" {
		t.Errorf("events: %q", got)
	}
}

// A monitor that was healthy and then loses what it needs must not keep
// saying so in the parts of its status the failing step does not touch.
func TestReconcile_EveryExitWritesAConsistentStatus(t *testing.T) {
	healthy := healthyHarness
	// A change of the spec, so that observedGeneration has something to
	// catch up with.
	touchSpec := func(h *reconcileHarness, change func(*pgpilotv1.PgpilotMonitor)) {
		h.t.Helper()
		m := h.monitor()
		change(m)
		m.Generation++
		if err := h.client.Update(context.Background(), m); err != nil {
			h.t.Fatal(err)
		}
	}
	expectNotReady := func(h *reconcileHarness) {
		h.t.Helper()
		m := h.monitor()
		if m.Status.Ready {
			h.t.Error("status.ready still says true")
		}
		if m.Status.ObservedGeneration != m.Generation {
			h.t.Errorf("observedGeneration = %d, generation = %d", m.Status.ObservedGeneration, m.Generation)
		}
		// The collector of the last good spec is still there, and is named.
		if m.Status.PodName != "pgpilot-db-abc" {
			h.t.Errorf("podName = %q", m.Status.PodName)
		}
	}

	t.Run("the Secret is gone", func(t *testing.T) {
		h := healthy(t)
		if err := h.client.Delete(context.Background(), unitSecret()); err != nil {
			t.Fatal(err)
		}
		touchSpec(h, func(m *pgpilotv1.PgpilotMonitor) { m.Spec.Database.Host = "other" })

		result, err := h.reconcile()
		if err != nil || result.RequeueAfter != 30*time.Second {
			t.Errorf("got %+v, %v; want a retry after thirty seconds", result, err)
		}
		expectNotReady(h)
		h.expectCondition("Ready", metav1.ConditionFalse, "SecretNotFound", `Secret "db-creds" not found`)
		h.expectCondition("CredentialsAvailable", metav1.ConditionFalse, "SecretNotFound", "")
		// Nothing was rendered for this spec; "True" from the last one
		// would say the ConfigMap matches it.
		h.expectCondition("ConfigGenerated", metav1.ConditionUnknown, "SecretNotFound", "Not evaluated: Secret")
		if len(h.applied) != 0 {
			t.Errorf("applied %v without credentials", h.applied)
		}
		if got := drain(h.recorder); len(got) != 1 || !strings.HasPrefix(got[0], "Warning SecretNotFound ") {
			t.Errorf("events: %q", got)
		}

		// Looked at again every thirty seconds: no event, no write.
		writes := h.statusWrites
		for range 3 {
			if _, err := h.reconcile(); err != nil {
				t.Fatal(err)
			}
		}
		if got := drain(h.recorder); len(got) != 0 {
			t.Errorf("the missing Secret was announced again: %q", got)
		}
		if h.statusWrites != writes {
			t.Errorf("%d status writes while nothing changed", h.statusWrites-writes)
		}
	})

	t.Run("a library is gone", func(t *testing.T) {
		h := healthy(t)
		touchSpec(h, func(m *pgpilotv1.PgpilotMonitor) {
			m.Spec.Metrics.FromLibraries = []pgpilotv1.LibraryRef{{Name: "shared"}}
		})

		result, err := h.reconcile()
		if err != nil || result.RequeueAfter != 30*time.Second {
			t.Errorf("got %+v, %v; want a retry after thirty seconds", result, err)
		}
		expectNotReady(h)
		h.expectCondition("Ready", metav1.ConditionFalse, "LibraryResolveFailed", `library "shared"`)
		h.expectCondition("CredentialsAvailable", metav1.ConditionTrue, "CredentialsFound", "")
		h.expectCondition("ConfigGenerated", metav1.ConditionFalse, "LibraryResolveFailed", `library "shared"`)
		if got := drain(h.recorder); len(got) != 1 || !strings.HasPrefix(got[0], "Warning LibraryResolveFailed ") {
			t.Errorf("events: %q", got)
		}
	})

	t.Run("the preset is not one", func(t *testing.T) {
		h := healthy(t)
		touchSpec(h, func(m *pgpilotv1.PgpilotMonitor) { m.Spec.Metrics.Preset = "exhaustiv" })

		result, err := h.reconcile()
		if err != nil || result != (ctrl.Result{}) {
			t.Errorf("got %+v, %v; want no retry", result, err)
		}
		expectNotReady(h)
		h.expectCondition("Ready", metav1.ConditionFalse, "InvalidPreset", `"exhaustiv"`)
		h.expectCondition("ConfigGenerated", metav1.ConditionFalse, "InvalidPreset", `"exhaustiv"`)
		if got := drain(h.recorder); len(got) != 1 || !strings.Contains(got[0], `"exhaustiv"`) {
			t.Errorf("events: %q", got)
		}

		// Corrected into another name that is not a preset: the reason is
		// the same, the mistake is a new one, and the event in the namespace
		// would otherwise still talk about the first.
		touchSpec(h, func(m *pgpilotv1.PgpilotMonitor) { m.Spec.Metrics.Preset = "exhaustve" })
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("Ready", metav1.ConditionFalse, "InvalidPreset", `"exhaustve"`)
		if got := drain(h.recorder); len(got) != 1 ||
			!strings.HasPrefix(got[0], "Warning InvalidPreset ") || !strings.Contains(got[0], `"exhaustve"`) {
			t.Errorf("events after the second mistake: %q", got)
		}
		// Looked at again without a change: nothing more to say.
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if got := drain(h.recorder); len(got) != 0 {
			t.Errorf("the same mistake was announced again: %q", got)
		}
	})
}

// Releases before 1.1 left status.ready out of a monitor that was not ready.
// A patch that only carries what changed would never put it there while the
// monitor stays not ready.
func TestReconcile_StatusReadyIsPartOfEveryStatusWrite(t *testing.T) {
	h := newReconcileHarness(t, unitMonitor())
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	h.expectCondition("Ready", metav1.ConditionFalse, "SecretNotFound", "")
	// The Secret appears, without a password: not ready before, not ready
	// now.
	secret := unitSecret()
	delete(secret.Data, "password")
	if err := h.client.Create(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	h.expectCondition("Ready", metav1.ConditionFalse, "SecretKeyMissing", "")
	if len(h.statusPatches) != 2 {
		t.Fatalf("status patches: %q", h.statusPatches)
	}
	for i, patch := range h.statusPatches {
		if !strings.Contains(patch, `"ready":false`) {
			t.Errorf("status patch %d does not carry status.ready: %s", i+1, patch)
		}
	}
}

// A library that exists but cannot be read is not the user's to fix.
func TestReconcile_LibraryReadFailureIsRetriedWithBackoff(t *testing.T) {
	monitor := unitMonitor()
	monitor.Spec.Metrics.FromLibraries = []pgpilotv1.LibraryRef{{Name: "shared"}}
	unitScheme(t)
	boom := errors.New("etcdserver: request timed out")
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(monitor, unitSecret()).WithStatusSubresource(monitor).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, isLibrary := obj.(*pgpilotv1.PgpilotMetricLibrary); isLibrary {
					return boom
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &PgpilotMonitorReconciler{Client: c, Scheme: scheme.Scheme, Recorder: record.NewFakeRecorder(10)}

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(monitor)})
	if !errors.Is(err, boom) || result != (ctrl.Result{}) {
		t.Errorf("got %+v, %v; want the read error handed to the work queue", result, err)
	}
	var got pgpilotv1.PgpilotMonitor
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(monitor), &got); err != nil {
		t.Fatal(err)
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, "Ready"); cond == nil ||
		cond.Reason != "LibraryResolveFailed" || !strings.Contains(cond.Message, "request timed out") {
		t.Errorf("Ready: %+v", cond)
	}
}

// A name the generated Service cannot carry. The monitor used to get a
// ConfigMap and a Deployment and then fail on the Service for ever, with
// nothing in its status.
func TestReconcile_InvalidNameIsReportedBeforeAnythingIsGenerated(t *testing.T) {
	for name, monitorName := range map[string]string{
		"a dot":           "db.prod",
		"too long":        strings.Repeat("a", 56),
		"trailing hyphen": "db-",
	} {
		t.Run(name, func(t *testing.T) {
			monitor := unitMonitor()
			monitor.Name = monitorName
			h := newReconcileHarness(t, monitor, unitSecret())

			result, err := h.reconcile()
			if err != nil || result != (ctrl.Result{}) {
				t.Errorf("got %+v, %v; a name cannot change, so there is nothing to retry", result, err)
			}
			if len(h.applied) != 0 {
				t.Errorf("applied %v for a monitor whose Service cannot exist", h.applied)
			}
			m := h.monitor()
			if m.Status.Ready || m.Status.ObservedGeneration != m.Generation {
				t.Errorf("status: %+v", m.Status)
			}
			h.expectCondition("Ready", metav1.ConditionFalse, "InvalidName", "pgpilot-"+monitorName)
			h.expectCondition("ConfigGenerated", metav1.ConditionFalse, "InvalidName", "at most 55 characters")
			h.expectCondition("CredentialsAvailable", metav1.ConditionUnknown, "InvalidName", "Not evaluated")
			if got := drain(h.recorder); len(got) != 1 || !strings.HasPrefix(got[0], "Warning InvalidName ") {
				t.Errorf("events: %q", got)
			}
		})
	}

	t.Run("the longest name that fits is accepted", func(t *testing.T) {
		monitor := unitMonitor()
		monitor.Name = strings.Repeat("a", 55)
		h := newReconcileHarness(t, monitor, unitSecret())
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(h.applied, ","); got != "ConfigMap,Deployment,Service" {
			t.Errorf("applied %q", got)
		}
	})
}

// From the ConfigMap on, a failure used to be returned and nothing else:
// no condition, no event, a status that still described the last success.
func TestReconcile_ApplyFailuresAreReported(t *testing.T) {
	if err := monitoringv1.AddToScheme(scheme.Scheme); err != nil {
		t.Fatal(err)
	}
	harness := func(t *testing.T, kind string) *reconcileHarness {
		t.Helper()
		h := newReconcileHarness(t, unitMonitor(), unitSecret())
		h.r.ServiceMonitorEnabled = kind == "ServiceMonitor"
		return h
	}
	failOn := func(kind string, failure func(obj client.Object) error) func(client.Object) error {
		return func(obj client.Object) error {
			if obj.GetObjectKind().GroupVersionKind().Kind == kind {
				return failure(obj)
			}
			return nil
		}
	}

	for _, kind := range []string{"ConfigMap", "Deployment", "Service", "ServiceMonitor"} {
		t.Run(kind+" refused as invalid", func(t *testing.T) {
			h := harness(t, kind)
			h.applyError = failOn(kind, func(obj client.Object) error {
				return invalid(kind, obj.GetName(), "must be no more than limits")
			})

			result, err := h.reconcile()
			// Not a failed reconcile: the same object would be refused again.
			// But what refused it may be outside the monitor, so it is sent
			// again after a while.
			if err != nil || result.RequeueAfter < 5*time.Minute || result.RequeueAfter > 5*time.Minute+30*time.Second {
				t.Errorf("got %+v, %v; want no error and another attempt in five minutes", result, err)
			}
			h.expectCondition("Ready", metav1.ConditionFalse, "ApplyRejected",
				kind+" pgpilot-db")
			h.expectCondition("Ready", metav1.ConditionFalse, "ApplyRejected", "must be no more than limits")
			if kind == "ConfigMap" {
				h.expectCondition("ConfigGenerated", metav1.ConditionFalse, "ApplyRejected", "ConfigMap pgpilot-db-config")
			} else {
				h.expectCondition("ConfigGenerated", metav1.ConditionTrue, "ConfigMapUpdated", "")
			}
			m := h.monitor()
			if m.Status.Ready || m.Status.ObservedGeneration != m.Generation {
				t.Errorf("status: %+v", m.Status)
			}
			events := drain(h.recorder)
			if len(events) == 0 || !strings.HasPrefix(events[len(events)-1], "Warning ApplyRejected "+kind) {
				t.Errorf("events: %q", events)
			}

			// Refused again five minutes later: nothing new to say or write.
			writes := h.statusWrites
			if _, err := h.reconcile(); err != nil {
				t.Fatal(err)
			}
			if again := drain(h.recorder); len(again) != 0 || h.statusWrites != writes {
				t.Errorf("the refusal was reported again: events %q, %d status writes", again, h.statusWrites-writes)
			}

			// What refused it is gone (an admission policy was lifted),
			// without anything changing on the monitor.
			h.applyError = nil
			if _, err := h.reconcile(); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(h.applied, kind) {
				t.Errorf("applied %v, without the %s", h.applied, kind)
			}
			if cond := meta.FindStatusCondition(h.monitor().Status.Conditions, "Ready"); cond.Reason == "ApplyRejected" {
				t.Errorf("still reported as rejected: %+v", cond)
			}
		})

		t.Run(kind+" cannot be written", func(t *testing.T) {
			h := harness(t, kind)
			boom := apierrors.NewForbidden(schema.GroupResource{Resource: "things"}, "pgpilot-db", errors.New("no"))
			h.applyError = failOn(kind, func(client.Object) error { return boom })

			result, err := h.reconcile()
			// Not the user's to fix: the work queue retries with backoff.
			if !errors.Is(err, boom) || result != (ctrl.Result{}) {
				t.Errorf("got %+v, %v; want the error handed to the work queue", result, err)
			}
			h.expectCondition("Ready", metav1.ConditionFalse, "ApplyFailed", kind+" pgpilot-db")
			first := drain(h.recorder)
			if len(first) == 0 || !strings.HasPrefix(first[len(first)-1], "Warning ApplyFailed "+kind) {
				t.Errorf("events: %q", first)
			}

			// The retries that follow announce neither the failure nor the
			// ConfigMap again: the hash is stored with the failure.
			if _, err := h.reconcile(); err == nil {
				t.Error("the retry did not fail")
			}
			if again := drain(h.recorder); len(again) != 0 {
				t.Errorf("first: %q; repeated on the retry: %q", first, again)
			}
			if kind != "ConfigMap" && h.monitor().Status.ConfigHash == "" {
				t.Error("the config hash of the applied ConfigMap was not stored")
			}
		})
	}

	t.Run("with a ServiceMonitor, four objects are applied", func(t *testing.T) {
		h := harness(t, "ServiceMonitor")
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(h.applied, ","); got != "ConfigMap,Deployment,Service,ServiceMonitor" {
			t.Errorf("applied %s", got)
		}
	})
}

// Only an object refused as invalid is a state to report and leave. An
// admission webhook that denies without a code answers 400; a conflict or a
// server that is busy passes.
func TestReconcile_OtherApplyFailuresAreRetried(t *testing.T) {
	deployments := schema.GroupResource{Group: "apps", Resource: "deployments"}
	for name, failure := range map[string]error{
		"bad request":    apierrors.NewBadRequest("admission webhook denied the request"),
		"conflict":       apierrors.NewConflict(deployments, "pgpilot-db", errors.New("the object has been modified")),
		"internal error": apierrors.NewInternalError(errors.New("etcd leader changed")),
		"server timeout": apierrors.NewServerTimeout(deployments, "patch", 1),
		"no API error":   errors.New("connection refused"),
	} {
		t.Run(name, func(t *testing.T) {
			h := newReconcileHarness(t, unitMonitor(), unitSecret())
			h.applyError = func(obj client.Object) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == "Deployment" {
					return failure
				}
				return nil
			}
			result, err := h.reconcile()
			if !errors.Is(err, failure) || result != (ctrl.Result{}) {
				t.Errorf("got %+v, %v; want the error handed to the work queue", result, err)
			}
			h.expectCondition("Ready", metav1.ConditionFalse, "ApplyFailed", "Deployment pgpilot-db could not be applied")
		})
	}
}

// The embedded metric definitions always render. Should they ever not, the
// reconcile must stop there and say so, not go on with no ConfigMap.
func TestReconcile_ConfigThatCannotBeRendered(t *testing.T) {
	boom := errors.New("yaml: cannot marshal")
	original := buildConfigMap
	buildConfigMap = func(*pgpilotv1.PgpilotMonitor, builder.MergedMetrics) (*corev1.ConfigMap, string, error) {
		return nil, "", boom
	}
	t.Cleanup(func() { buildConfigMap = original })

	h := newReconcileHarness(t, unitMonitor(), unitSecret())
	result, err := h.reconcile()
	if !errors.Is(err, boom) || result != (ctrl.Result{}) {
		t.Errorf("got %+v, %v; want the error handed to the work queue", result, err)
	}
	if len(h.applied) != 0 {
		t.Errorf("applied %v without a configuration", h.applied)
	}
	h.expectCondition("Ready", metav1.ConditionFalse, "ConfigBuildFailed", "cannot marshal")
	h.expectCondition("ConfigGenerated", metav1.ConditionFalse, "ConfigBuildFailed", "could not be rendered")
	h.expectCondition("CredentialsAvailable", metav1.ConditionTrue, "CredentialsFound", "")
	if got := drain(h.recorder); len(got) != 1 || !strings.HasPrefix(got[0], "Warning ConfigBuildFailed ") {
		t.Errorf("events: %q", got)
	}
}

// A status that cannot be written is a failed reconcile: reported to the
// work queue, and not announced as if it had been stored.
func TestReconcile_StatusWriteFailure(t *testing.T) {
	h := newReconcileHarness(t, unitMonitor(), unitSecret())
	h.deploymentStatus = rolledOut
	h.statusError = errors.New("the server is currently unable to handle the request")

	if _, err := h.reconcile(); err == nil || !strings.Contains(err.Error(), "update status") ||
		!errors.Is(err, h.statusError) {
		t.Errorf("want the status write error, got %v", err)
	}
	if events := drain(h.recorder); len(events) != 0 {
		t.Errorf("announced %q although the status was not stored", events)
	}

	// Once the write goes through, the transition is announced.
	h.statusError = nil
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	events := drain(h.recorder)
	if len(events) == 0 || events[len(events)-1] != "Normal PodRunning pgwatch pod is running" {
		t.Errorf("events: %q", events)
	}

	// The monitor was read from a cache that had not caught up with the
	// operator's own last write. Announcing what this reconcile found would
	// repeat what the previous one already said.
	t.Run("a stale read is looked at again and announces nothing", func(t *testing.T) {
		h := newReconcileHarness(t, unitMonitor(), unitSecret())
		h.deploymentStatus = rolledOut
		h.statusError = apierrors.NewConflict(schema.GroupResource{Resource: "pgpilotmonitors"}, "db", errors.New("stale"))

		result, err := h.reconcile()
		if err != nil || result.RequeueAfter != time.Second {
			t.Errorf("got %+v, %v; want another look after a second and no error", result, err)
		}
		if events := drain(h.recorder); len(events) != 0 {
			t.Errorf("announced %q on a status that was not stored", events)
		}
	})

	// The same, with the conflict coming from the API server itself: the
	// status write must carry the version it was compared with.
	t.Run("a monitor changed since it was read is not written over", func(t *testing.T) {
		h := newReconcileHarness(t, unitMonitor(), unitSecret(), runningPod("pgpilot-db-abc"))
		h.deploymentStatus = rolledOut
		h.beforeStatusWrite = func() {
			m := h.monitor()
			m.Annotations = map[string]string{"touched": "meanwhile"}
			if err := h.client.Update(context.Background(), m); err != nil {
				t.Fatal(err)
			}
		}

		result, err := h.reconcile()
		if err != nil || result.RequeueAfter != time.Second {
			t.Errorf("got %+v, %v; want another look after a second and no error", result, err)
		}
		if events := drain(h.recorder); len(events) != 0 {
			t.Errorf("announced %q on a status that was not stored", events)
		}
		if m := h.monitor(); m.Status.Ready || len(m.Status.Conditions) != 0 {
			t.Errorf("the status was stored over a newer monitor: %+v", m.Status)
		}

		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("Ready", metav1.ConditionTrue, "PodRunning", "")
		if events := drain(h.recorder); len(events) != 2 ||
			!strings.HasPrefix(events[0], "Normal ConfigMapUpdated ") || !strings.HasPrefix(events[1], "Normal PodRunning ") {
			t.Errorf("events once the write went through: %q", events)
		}
	})

	// A reconcile that failed stays failed, whatever became of its status.
	t.Run("a conflict does not hide that the reconcile failed", func(t *testing.T) {
		h := newReconcileHarness(t, unitMonitor(), unitSecret())
		boom := apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "pgpilot-db-config", errors.New("no"))
		h.applyError = func(client.Object) error { return boom }
		h.statusError = apierrors.NewConflict(schema.GroupResource{Resource: "pgpilotmonitors"}, "db", errors.New("stale"))

		result, err := h.reconcile()
		if !errors.Is(err, boom) || result != (ctrl.Result{}) {
			t.Errorf("got %+v, %v; want the apply error handed to the work queue", result, err)
		}
		if events := drain(h.recorder); len(events) != 0 {
			t.Errorf("announced %q on a status that was not stored", events)
		}
	})
	t.Run("both failures are reported when the status cannot be written either", func(t *testing.T) {
		h := newReconcileHarness(t, unitMonitor(), unitSecret())
		boom := apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "pgpilot-db-config", errors.New("no"))
		h.applyError = func(client.Object) error { return boom }
		h.statusError = errors.New("the server is currently unable to handle the request")

		if _, err := h.reconcile(); !errors.Is(err, boom) || !errors.Is(err, h.statusError) {
			t.Errorf("got %v; want the apply error and the status write error", err)
		}
	})

	t.Run("a monitor deleted meanwhile is nothing to report", func(t *testing.T) {
		h := newReconcileHarness(t, unitMonitor(), unitSecret())
		h.statusError = apierrors.NewNotFound(schema.GroupResource{Resource: "pgpilotmonitors"}, "db")
		if result, err := h.reconcile(); err != nil || result != (ctrl.Result{}) {
			t.Errorf("got %+v, %v", result, err)
		}
	})
}

// The cache can be a step behind the operator's own last status write. A
// monitor that failed once and is fine at the retry, five milliseconds later,
// must not find its recovered status equal to the stale copy and conclude
// there is nothing to write: the failure would stay on display for hours.
func TestReconcile_ARecoveryIsNotComparedWithAStaleRead(t *testing.T) {
	h := healthyHarness(t)
	cache := h.monitor() // Ready=True, as the cache holds it

	boom := apierrors.NewServiceUnavailable("etcd leader changed")
	h.applyError = func(obj client.Object) error {
		if obj.GetObjectKind().GroupVersionKind().Kind == "Deployment" {
			return boom
		}
		return nil
	}
	if _, err := h.reconcile(); !errors.Is(err, boom) {
		t.Fatalf("got %v, want the apply error", err)
	}
	h.expectCondition("Ready", metav1.ConditionFalse, "ApplyFailed", "etcd leader changed")
	if got := drain(h.recorder); len(got) != 1 || !strings.HasPrefix(got[0], "Warning ApplyFailed ") {
		t.Fatalf("events: %q", got)
	}

	// The retry: the API server is fine again, the cache has not moved.
	h.applyError = nil
	h.cached = cache
	writes := h.statusWrites
	result, err := h.reconcile()
	if err != nil || result != (ctrl.Result{RequeueAfter: time.Second}) {
		t.Errorf("got %+v, %v; want another look in a second", result, err)
	}
	if len(h.applied) != 0 || h.statusWrites != writes {
		t.Errorf("acted on a stale read: applied %v, %d status writes", h.applied, h.statusWrites-writes)
	}
	if got := drain(h.recorder); len(got) != 0 {
		t.Errorf("events from a stale read: %q", got)
	}
	// Asked again before the cache has moved: the same answer.
	if result, err := h.reconcile(); err != nil || result.RequeueAfter != time.Second {
		t.Errorf("second stale read: got %+v, %v", result, err)
	}

	// The cache has caught up.
	h.cached = nil
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	h.expectCondition("Ready", metav1.ConditionTrue, "PodRunning", "")
	if !h.monitor().Status.Ready {
		t.Error("status.ready still says false after the recovery")
	}
	if got := drain(h.recorder); len(got) != 1 || got[0] != "Normal PodRunning pgwatch pod is running" {
		t.Errorf("events after the recovery: %q", got)
	}
	// What is remembered is for the reads that follow a write; a read that
	// has moved on ends it.
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	if _, remembered := h.r.replaced.Load(h.key); remembered {
		t.Error("the replaced version is still remembered after a newer read")
	}
}

func TestReconcile_StaleReadBookkeeping(t *testing.T) {
	t.Run("a deleted monitor is forgotten", func(t *testing.T) {
		h := healthyHarness(t)
		if _, remembered := h.r.replaced.Load(h.key); !remembered {
			t.Fatal("the version the status write replaced was not remembered")
		}
		// Gone without another look in between.
		m := h.monitor()
		m.Finalizers = nil
		if err := h.client.Update(context.Background(), m); err != nil {
			t.Fatal(err)
		}
		if err := h.client.Delete(context.Background(), m); err != nil {
			t.Fatal(err)
		}
		if result, err := h.reconcile(); err != nil || result != (ctrl.Result{}) {
			t.Errorf("got %+v, %v", result, err)
		}
		if _, remembered := h.r.replaced.Load(h.key); remembered {
			t.Error("a monitor that is gone is still remembered")
		}
	})

	// A write the API server answered without storing anything replaced
	// nothing: the version read next is the same one, and is not stale.
	t.Run("a write that changed nothing replaced nothing", func(t *testing.T) {
		h := newReconcileHarness(t, unitMonitor(), unitSecret(), runningPod("pgpilot-db-abc"))
		h.deploymentStatus = rolledOut
		h.dropStatusWrite = true
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.dropStatusWrite = false
		result, err := h.reconcile()
		if err != nil || result.RequeueAfter == time.Second {
			t.Errorf("got %+v, %v; the read was taken for a stale one", result, err)
		}
		h.expectCondition("Ready", metav1.ConditionTrue, "PodRunning", "")
	})
}

// Pods that cannot be listed are not pods that are gone: the Deployment alone
// decides, and the pod last seen stays named.
func TestReconcile_PodListFailure(t *testing.T) {
	h := healthyHarness(t)
	h.podListError = errors.New("the cache is not started")
	h.deploymentStatus = starting

	result, err := h.reconcile()
	if err != nil || result.RequeueAfter != 15*time.Second {
		t.Errorf("got %+v, %v; want another look in fifteen seconds and no error", result, err)
	}
	h.expectCondition("Ready", metav1.ConditionFalse, "PodNotReady", "not ready yet")
	if got := h.monitor().Status.PodName; got != "pgpilot-db-abc" {
		t.Errorf("podName = %q, want the pod last seen", got)
	}

	// The same on a reconcile that ends before it gets to the Deployment.
	t.Run("on a reconcile that stops early", func(t *testing.T) {
		h := healthyHarness(t)
		h.podListError = errors.New("the cache is not started")
		if err := h.client.Delete(context.Background(), unitSecret()); err != nil {
			t.Fatal(err)
		}
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("Ready", metav1.ConditionFalse, "SecretNotFound", "")
		if got := h.monitor().Status.PodName; got != "pgpilot-db-abc" {
			t.Errorf("podName = %q, want the pod last seen", got)
		}
	})
}

// podName is the pod that runs now; it must not go on naming one that is
// gone.
func TestReconcile_PodNameIsClearedWhenNoPodRuns(t *testing.T) {
	pod := runningPod("pgpilot-db-abc")
	h := newReconcileHarness(t, unitMonitor(), unitSecret(), pod)
	h.deploymentStatus = rolledOut
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	if got := h.monitor().Status.PodName; got != "pgpilot-db-abc" {
		t.Fatalf("podName = %q", got)
	}

	if err := h.client.Delete(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	h.deploymentStatus = starting
	result, err := h.reconcile()
	if err != nil {
		t.Fatal(err)
	}
	// A pod on its way up is looked at often.
	if result.RequeueAfter != 15*time.Second {
		t.Errorf("RequeueAfter = %s, want fifteen seconds", result.RequeueAfter)
	}
	m := h.monitor()
	if m.Status.PodName != "" {
		t.Errorf("podName = %q, although no pod runs", m.Status.PodName)
	}
	h.expectCondition("Ready", metav1.ConditionFalse, "PodNotReady", "not ready yet")
}

// The time of the last status change is not a heartbeat: a reconcile that
// finds everything as it was must not write.
func TestReconcile_LastReconciledMovesOnlyWithTheStatus(t *testing.T) {
	h := newReconcileHarness(t, unitMonitor(), unitSecret(), runningPod("a"))
	h.deploymentStatus = rolledOut
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	first := h.monitor().Status.LastReconciled.Time

	time.Sleep(1100 * time.Millisecond) // the timestamp has a resolution of one second
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	if got := h.monitor().Status.LastReconciled.Time; !got.Equal(first) {
		t.Errorf("lastReconciled moved from %s to %s although nothing changed", first, got)
	}

	h.deploymentStatus = starting
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	if got := h.monitor().Status.LastReconciled.Time; !got.After(first) {
		t.Errorf("lastReconciled stayed at %s although the status changed", got)
	}
}
