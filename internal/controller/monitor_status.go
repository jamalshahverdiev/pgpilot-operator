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
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

// Reasons of the Ready condition. They are what `kubectl get pm -o yaml`,
// the events and docs/troubleshooting.md show.
const (
	conditionReady = "Ready"

	// The collector of the current spec runs and is available.
	reasonPodRunning = "PodRunning"

	// Normal states on the way there.
	reasonRolloutInProgress = "RolloutInProgress"
	reasonPodNotReady       = "PodNotReady"

	// The rollout cannot finish, and why.
	reasonPodCreateFailed  = "PodCreateFailed"
	reasonUnschedulable    = "Unschedulable"
	reasonImagePullBackOff = "ImagePullBackOff"
	reasonCrashLoopBackOff = "CrashLoopBackOff"
	reasonRolloutStuck     = "RolloutStuck"

	// What the kubelet says about a container it cannot start, passed on
	// under the kubelet's own name for it.
	reasonCreateContainerConfigError = "CreateContainerConfigError"
	reasonCreateContainerError       = "CreateContainerError"
	reasonRunContainerError          = "RunContainerError"

	// The spec cannot be turned into a collector.
	reasonInvalidName   = "InvalidName"
	reasonInvalidPreset = "InvalidPreset"
	// The API server refused a generated object as invalid.
	reasonApplyRejected = "ApplyRejected"
	// A generated object could not be written for another reason; retried.
	reasonApplyFailed = "ApplyFailed"
	// The configuration could not be rendered; retried.
	reasonConfigBuildFailed = "ConfigBuildFailed"

	reasonLibraryResolveFailed = "LibraryResolveFailed"

	// The credentials the collector needs are not there.
	reasonSecretNotFound   = "SecretNotFound"
	reasonSecretKeyMissing = "SecretKeyMissing"
	reasonSecretReadFailed = "SecretReadFailed"
	reasonNoCredentials    = "NoCredentials"
)

const (
	// notReadyRequeue is how soon a monitor whose collector is on its way up
	// is looked at again. The Deployment is watched, but what holds a pod up
	// (an image that cannot be pulled, a crashing container) only shows on
	// the pod.
	notReadyRequeue = 15 * time.Second
	// stuckRequeue is how soon a monitor is looked at again once it is known
	// what holds its collector up. The look only refines the reason; the end
	// of the problem arrives through the Deployment, which is watched.
	stuckRequeue = time.Minute
	// libraryRetry is how soon a missing library is looked for again; its
	// creation also triggers a reconcile.
	libraryRetry = 30 * time.Second
	// rejectedRetry is how soon an object the API server refused as invalid
	// is sent again. What refused it may be outside the monitor (an admission
	// policy, the installed version of a CRD), and its removal reaches the
	// operator through no watch.
	rejectedRetry = 5 * time.Minute
	// staleReadRetry is how soon a monitor is looked at again when the cache
	// has not caught up with the operator's own last status write.
	staleReadRetry = time.Second

	// loopingRestarts is the number of restarts from which a container that
	// exits is said to be in a crash loop. Below it the container has exited
	// once and is coming back: after a node restart, or one out-of-memory
	// kill.
	loopingRestarts = 2
)

// transitional are the reasons of a monitor that is not ready yet without
// anything being wrong.
var transitional = map[string]bool{
	reasonRolloutInProgress: true,
	reasonPodNotReady:       true,
}

// outcome is what one reconcile concluded about a monitor: the Ready
// condition to report and what the work queue should do next.
type outcome struct {
	ready           bool
	reason, message string

	// requeueAfter asks for another look after a while.
	requeueAfter time.Duration
	// err hands the reconcile back to the work queue as failed, to be
	// retried with backoff. Left nil for states only a change of the spec
	// or of the cluster can end.
	err error
}

// monitorStatus collects the status of one reconcile in memory. Nothing is
// written or announced until finish.
type monitorStatus struct {
	monitor *pgpilotv1.PgpilotMonitor
	// evaluated holds the conditions this reconcile came to a verdict on.
	evaluated map[string]bool
	// pods are the monitor's collector pods, once they have been listed;
	// podsKnown is false when they could not be.
	pods       []corev1.Pod
	podsListed bool
	podsKnown  bool
	// announce holds events about what this reconcile did, to be recorded
	// once the status that goes with them is stored.
	announce []pendingEvent
}

type pendingEvent struct {
	eventType, reason, message string
}

// event queues an event for finish.
func (s *monitorStatus) event(eventType, reason, format string, args ...any) {
	s.announce = append(s.announce, pendingEvent{eventType, reason, fmt.Sprintf(format, args...)})
}

func newMonitorStatus(monitor *pgpilotv1.PgpilotMonitor) *monitorStatus {
	return &monitorStatus{monitor: monitor, evaluated: map[string]bool{}}
}

func (s *monitorStatus) set(condType string, status metav1.ConditionStatus, reason, message string) {
	s.evaluated[condType] = true
	meta.SetStatusCondition(&s.monitor.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: s.monitor.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// collectorPods lists the monitor's pods once per reconcile. A failure to
// list them is logged and the reconcile goes on without: the pods only refine
// what the Deployment already says. The second result tells "no pods" from
// "not known".
func (r *PgpilotMonitorReconciler) collectorPods(ctx context.Context, s *monitorStatus) ([]corev1.Pod, bool) {
	if !s.podsListed {
		s.podsListed = true
		var list corev1.PodList
		if err := r.List(ctx, &list,
			client.InNamespace(s.monitor.Namespace),
			client.MatchingLabels{
				builder.LabelAppInstance: s.monitor.Name,
				builder.LabelAppName:     builder.AppName,
			},
		); err != nil {
			logf.FromContext(ctx).Error(err, "failed to list the collector pods")
		} else {
			s.pods, s.podsKnown = list.Items, true
		}
	}
	return s.pods, s.podsKnown
}

// finish is the one place where the status of a monitor is written and its
// events are recorded. Every path through a reconcile ends here, so that the
// parts of the status cannot contradict each other:
//
//   - observedGeneration is the generation this reconcile looked at;
//   - status.ready says what the Ready condition says;
//   - a condition this reconcile did not get to is Unknown, not whatever an
//     earlier reconcile left there;
//   - podName names a pod that runs now, or nothing.
//
// The status is written only when it differs from what is stored, and an
// event is recorded only when the Ready condition changes its status or
// reason, or reports another problem for a changed spec: a monitor that waits
// for a Secret is looked at every thirty seconds and must not produce an
// event and a write each time.
//
// "What is stored" is what the cache held when the reconcile began, and the
// cache can be a step behind the operator's own last write. Two things keep
// that from showing:
//
//   - the write only goes through against the version it was compared with.
//     When it does not, nothing is announced and the monitor is looked at
//     again in a moment: comparing against a stale status would report a
//     state as new that was already reported;
//   - the version a write replaced is remembered, and a reconcile that is
//     handed that version again does nothing (see readIsStale). Without it a
//     monitor that failed once and recovered at the next attempt would find
//     its recovered status equal to the stale copy, write nothing and keep
//     the failure on display.
func (r *PgpilotMonitorReconciler) finish(
	ctx context.Context,
	stored *pgpilotv1.PgpilotMonitor,
	s *monitorStatus,
	out outcome,
) (ctrl.Result, error) {
	monitor := s.monitor

	for _, condType := range []string{conditionCredentials, conditionConfigGenerated} {
		if !s.evaluated[condType] {
			s.set(condType, metav1.ConditionUnknown, out.reason, "Not evaluated: "+out.message)
		}
	}
	readyStatus := metav1.ConditionFalse
	if out.ready {
		readyStatus = metav1.ConditionTrue
	}
	s.set(conditionReady, readyStatus, out.reason, out.message)
	monitor.Status.Ready = out.ready
	monitor.Status.ObservedGeneration = monitor.Generation
	// Pods that could not be listed are not pods that are gone.
	if pods, known := r.collectorPods(ctx, s); known {
		monitor.Status.PodName = runningPodName(pods)
	}

	if !sameStatus(&stored.Status, &monitor.Status) {
		monitor.Status.LastReconciled = &metav1.Time{Time: time.Now()}
		before := meta.FindStatusCondition(stored.Status.Conditions, conditionReady)
		// A patch of the status alone: it does not send the spec back to be
		// validated. A merge patch carries what differs from its base, and
		// status.ready has to be in every one of them: releases before 1.1
		// left the key out of a monitor that was not ready, and it would stay
		// missing for as long as the monitor stays not ready.
		base := stored.DeepCopy()
		base.Status.Ready = !monitor.Status.Ready
		patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
		switch err := r.Status().Patch(ctx, monitor, patch); {
		case apierrors.IsNotFound(err):
			return ctrl.Result{}, nil
		case apierrors.IsConflict(err):
			if out.err != nil {
				return ctrl.Result{}, out.err
			}
			return ctrl.Result{RequeueAfter: staleReadRetry}, nil
		case err != nil:
			return ctrl.Result{}, errors.Join(out.err, fmt.Errorf("update status: %w", err))
		}
		if monitor.ResourceVersion != stored.ResourceVersion {
			r.replaced.Store(client.ObjectKeyFromObject(monitor), stored.ResourceVersion)
		}
		for _, e := range s.announce {
			r.Recorder.Event(monitor, e.eventType, e.reason, e.message)
		}
		r.recordTransition(monitor, before, out)
	}

	if out.err != nil {
		return ctrl.Result{}, out.err
	}
	return ctrl.Result{RequeueAfter: out.requeueAfter}, nil
}

// readIsStale reports whether the monitor just read is the very version the
// operator's last status write replaced, that is, whether the cache has not
// delivered that write yet. The write was made against exactly that version,
// so nothing else can lie in between.
func (r *PgpilotMonitorReconciler) readIsStale(monitor *pgpilotv1.PgpilotMonitor) bool {
	key := client.ObjectKeyFromObject(monitor)
	replaced, ok := r.replaced.Load(key)
	if !ok {
		return false
	}
	if replaced == monitor.ResourceVersion {
		return true
	}
	r.replaced.Delete(key)
	return false
}

// sameStatus compares two statuses, leaving aside when they were written.
func sameStatus(a, b *pgpilotv1.PgpilotMonitorStatus) bool {
	x, y := a.DeepCopy(), b.DeepCopy()
	x.LastReconciled, y.LastReconciled = nil, nil
	return equality.Semantic.DeepEqual(x, y)
}

// recordTransition records an event when the Ready condition changed its
// status or reason. A problem of the same kind as before is announced again
// only when it is another one, found in a changed spec: a second preset that
// does not exist, another Secret that is missing.
func (r *PgpilotMonitorReconciler) recordTransition(
	monitor *pgpilotv1.PgpilotMonitor,
	before *metav1.Condition,
	out outcome,
) {
	wasReady := before != nil && before.Status == metav1.ConditionTrue
	if before != nil && wasReady == out.ready && before.Reason == out.reason {
		// A monitor that is still on its way up passes through here too;
		// the switch below has nothing to say about that.
		anotherProblem := !out.ready &&
			before.ObservedGeneration != monitor.Generation && before.Message != out.message
		if !anotherProblem {
			return
		}
	}
	switch {
	case out.ready:
		r.Recorder.Event(monitor, corev1.EventTypeNormal, out.reason, out.message)
	case !transitional[out.reason]:
		r.Recorder.Event(monitor, corev1.EventTypeWarning, out.reason, out.message)
	case !wasReady:
		// On its way up for the first time, or from one waiting state to
		// another: nothing to announce.
	case out.reason == reasonPodNotReady:
		// The pod of the current spec was available and no longer is.
		r.Recorder.Event(monitor, corev1.EventTypeWarning, out.reason, out.message)
	default:
		r.Recorder.Event(monitor, corev1.EventTypeNormal, out.reason, out.message)
	}
}

// collectorState decides whether the collector the spec asks for is running,
// from the Deployment as the API server returned it and the monitor's pods.
//
// A ready replica is not enough: while a rollout is stuck the pod of the
// previous spec keeps running and stays ready, and reporting that as Ready
// would claim a change is live that was never applied. The monitor is ready
// when the Deployment controller has seen the current pod template, every
// replica runs it and is available. Otherwise the reason is the most specific
// one that can be found: why no pod could be created, what a pod is waiting
// for, or that the rollout ran out of time.
func collectorState(dep *appsv1.Deployment, pods []corev1.Pod) (ready bool, reason, message string) {
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	st := dep.Status

	// Until the Deployment controller has acted on the template just
	// applied, its conditions and the pods describe the previous one.
	if st.ObservedGeneration < dep.Generation {
		return false, reasonRolloutInProgress,
			"Waiting for the Deployment controller to roll out the current pod template"
	}
	if st.UpdatedReplicas == want && st.Replicas == want && st.AvailableReplicas >= want {
		return true, reasonPodRunning, "pgwatch pod is running"
	}

	if cond := deploymentCondition(dep, appsv1.DeploymentReplicaFailure); cond != nil && cond.Status == corev1.ConditionTrue {
		return false, reasonPodCreateFailed, "No collector pod could be created: " + cond.Message
	}
	// Before the current template has a pod, the pods are those of the
	// previous one, and what troubles them is no longer the question.
	if st.UpdatedReplicas > 0 {
		if r, m, found := podProblem(pods); found {
			return false, r, m
		}
	}
	if cond := deploymentCondition(dep, appsv1.DeploymentProgressing); cond != nil &&
		cond.Status == corev1.ConditionFalse {
		return false, reasonRolloutStuck, "The rollout of the collector does not progress: " + cond.Message
	}
	if st.UpdatedReplicas == want && st.Replicas == want {
		return false, reasonPodNotReady, "pgwatch pod is not ready yet"
	}
	message = fmt.Sprintf("Waiting for the collector pod of the current spec (%d of %d updated, %d available)",
		st.UpdatedReplicas, want, st.AvailableReplicas)
	if st.Replicas > st.UpdatedReplicas {
		message += "; the pod of the previous spec keeps running until then"
	}
	return false, reasonRolloutInProgress, message
}

func deploymentCondition(dep *appsv1.Deployment, condType appsv1.DeploymentConditionType) *appsv1.DeploymentCondition {
	for i := range dep.Status.Conditions {
		if dep.Status.Conditions[i].Type == condType {
			return &dep.Status.Conditions[i]
		}
	}
	return nil
}

// newestFirst orders pods by creation time, newest first; pods created in the
// same second by name, so that the choice does not depend on list order.
func newestFirst(a, b *corev1.Pod) int {
	if c := b.CreationTimestamp.Compare(a.CreationTimestamp.Time); c != 0 {
		return c
	}
	return strings.Compare(a.Name, b.Name)
}

func podIsReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// podProblem looks for what keeps the collector pod of the current spec from
// becoming ready. That pod is the newest one that is neither ready, nor being
// deleted, nor finished for good: a pod in phase Failed (evicted, or caught
// by a node shutdown) is never started again and stays around until it is
// garbage collected, and what it died of says nothing about its replacement.
// Older pods are not asked either: with one replica they can only belong to
// a previous spec.
func podProblem(pods []corev1.Pod) (reason, message string, found bool) {
	var pod *corev1.Pod
	for i := range pods {
		candidate := &pods[i]
		if candidate.DeletionTimestamp != nil || podIsReady(candidate) ||
			candidate.Status.Phase == corev1.PodFailed || candidate.Status.Phase == corev1.PodSucceeded {
			continue
		}
		if pod == nil || newestFirst(candidate, pod) < 0 {
			pod = candidate
		}
	}
	if pod == nil {
		return "", "", false
	}

	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse &&
			cond.Reason == corev1.PodReasonUnschedulable {
			return reasonUnschedulable, fmt.Sprintf("Pod %s cannot be scheduled: %s", pod.Name, cond.Message), true
		}
	}
	for i := range pod.Status.InitContainerStatuses {
		cs := &pod.Status.InitContainerStatuses[i]
		// An init container that ran to its end stays in this state for the
		// life of the pod. The operator generates none, but service meshes
		// and secret agents inject them.
		if t := cs.State.Terminated; t != nil && t.ExitCode == 0 {
			continue
		}
		if r, m, ok := containerProblem(pod.Name, cs); ok {
			return r, m, true
		}
	}
	for i := range pod.Status.ContainerStatuses {
		if r, m, ok := containerProblem(pod.Name, &pod.Status.ContainerStatuses[i]); ok {
			return r, m, true
		}
	}
	return "", "", false
}

// containerProblem maps the state of a container to a reason. Reasons that
// alternate while the kubelet retries (ErrImagePull and ImagePullBackOff, a
// crashed container that is restarted and crashes again) are folded into one,
// so that the condition does not flap and announce itself each time.
//
// A container that exited is not yet one that keeps exiting: until it has
// been restarted loopingRestarts times it is reported as not ready, with what
// it exited of.
func containerProblem(pod string, cs *corev1.ContainerStatus) (reason, message string, found bool) {
	if w := cs.State.Waiting; w != nil {
		switch w.Reason {
		case "ImagePullBackOff", "ErrImagePull", "InvalidImageName", "ErrImageNeverPull":
			return reasonImagePullBackOff,
				fmt.Sprintf("Pod %s cannot pull image %q: %s", pod, cs.Image, detail(w.Reason, w.Message)), true
		case reasonCreateContainerConfigError, reasonCreateContainerError, reasonRunContainerError:
			return w.Reason,
				fmt.Sprintf("Container %s of pod %s cannot start: %s", cs.Name, pod, detail(w.Reason, w.Message)), true
		case reasonCrashLoopBackOff:
			return reasonCrashLoopBackOff, crashMessage(pod, cs, true), true
		}
	}
	if cs.State.Terminated != nil || (!cs.Ready && cs.RestartCount > 0 && cs.LastTerminationState.Terminated != nil) {
		if cs.RestartCount < loopingRestarts {
			return reasonPodNotReady, crashMessage(pod, cs, false), true
		}
		return reasonCrashLoopBackOff, crashMessage(pod, cs, true), true
	}
	return "", "", false
}

func detail(reason, message string) string {
	if message == "" {
		return reason
	}
	return message
}

// crashMessage describes a container that exited: the exit it is in now, or
// else the last one before it was started again.
func crashMessage(pod string, cs *corev1.ContainerStatus, looping bool) string {
	last := cs.State.Terminated
	if last == nil {
		last = cs.LastTerminationState.Terminated
	}
	exit := ""
	if last != nil {
		exit = fmt.Sprintf(" (exit code %d", last.ExitCode)
		if last.Reason != "" {
			exit += ", " + last.Reason
		}
		exit += ")"
	}
	if !looping {
		return fmt.Sprintf("Container %s of pod %s exited%s and is being restarted", cs.Name, pod, exit)
	}
	return fmt.Sprintf("Container %s of pod %s keeps exiting%s; restarted %d times. See the pod's log with --previous",
		cs.Name, pod, exit, cs.RestartCount)
}

// runningPodName returns the collector pod to look at: one that runs and is
// not on its way out, a ready one before others, the newest of those. Empty
// when there is none.
func runningPodName(pods []corev1.Pod) string {
	var best *corev1.Pod
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		switch {
		case best == nil:
			best = pod
		case podIsReady(pod) != podIsReady(best):
			if podIsReady(pod) {
				best = pod
			}
		case newestFirst(pod, best) < 0:
			best = pod
		}
	}
	if best == nil {
		return ""
	}
	return best.Name
}
