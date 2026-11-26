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
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

// deploymentWith builds a Deployment of one replica at the given generation.
func deploymentWith(generation int64, status appsv1.DeploymentStatus) *appsv1.Deployment {
	dep := &appsv1.Deployment{}
	dep.Generation = generation
	dep.Spec.Replicas = new(int32(1))
	dep.Status = status
	return dep
}

func depCondition(t appsv1.DeploymentConditionType, status corev1.ConditionStatus, reason, message string) appsv1.DeploymentCondition {
	return appsv1.DeploymentCondition{Type: t, Status: status, Reason: reason, Message: message}
}

// podAt builds a collector pod; age orders the pods, a higher one is newer.
func podAt(name string, age int, mutate ...func(*corev1.Pod)) corev1.Pod {
	pod := corev1.Pod{
		Name:              name,
		CreationTimestamp: metav1.NewTime(time.Date(2026, 1, 1, 0, 0, age, 0, time.UTC))}
	pod.Status.Phase = corev1.PodRunning
	for _, m := range mutate {
		m(&pod)
	}
	return pod
}

func podReady(pod *corev1.Pod) {
	pod.Status.Conditions = append(pod.Status.Conditions,
		corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue})
}

// podUnready is what the kubelet writes on a pod that is not ready: the
// condition is there, and false.
func podUnready(pod *corev1.Pod) {
	pod.Status.Conditions = append(pod.Status.Conditions,
		corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady"})
}

// pgwatchContainer gives the pod one container status, for the test to fill.
func pgwatchContainer(fill func(*corev1.ContainerStatus)) func(*corev1.Pod) {
	return func(pod *corev1.Pod) {
		cs := corev1.ContainerStatus{Name: "pgwatch", Image: "cybertecpostgresql/pgwatch:9.9.9"}
		fill(&cs)
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{cs}
	}
}

// initContainer adds the status of an init container, such as a service mesh
// injects.
func initContainer(name string, state corev1.ContainerState) func(*corev1.Pod) {
	return func(pod *corev1.Pod) {
		pod.Status.InitContainerStatuses = append(pod.Status.InitContainerStatuses,
			corev1.ContainerStatus{Name: name, Image: "mesh/init:1", State: state})
	}
}

func exited(code int32, reason string) *corev1.ContainerStateTerminated {
	return &corev1.ContainerStateTerminated{ExitCode: code, Reason: reason}
}

var completed = corev1.ContainerState{Terminated: exited(0, "Completed")}

func podDeleting(pod *corev1.Pod) {
	now := metav1.Now()
	pod.DeletionTimestamp = &now
}

func podPhase(phase corev1.PodPhase) func(*corev1.Pod) {
	return func(pod *corev1.Pod) { pod.Status.Phase = phase }
}

func waiting(reason, message string) func(*corev1.Pod) {
	return func(pod *corev1.Pod) {
		pod.Status.Phase = corev1.PodPending
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "pgwatch", Image: "cybertecpostgresql/pgwatch:9.9.9",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}},
		}}
	}
}

func TestCollectorState(t *testing.T) {
	const (
		availableOld = 1
	)
	// What a Deployment of one replica reports while the pod of a new
	// template cannot become ready: the old pod stays, and stays ready.
	stuck := appsv1.DeploymentStatus{
		ObservedGeneration: 2, Replicas: 2, UpdatedReplicas: 1,
		ReadyReplicas: availableOld, AvailableReplicas: availableOld, UnavailableReplicas: 1,
	}
	complete := appsv1.DeploymentStatus{
		ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
	}
	withConditions := func(st appsv1.DeploymentStatus, conds ...appsv1.DeploymentCondition) appsv1.DeploymentStatus {
		st.Conditions = conds
		return st
	}
	// The first thing a Deployment reports about a changed template: seen,
	// and no pod of it yet.
	noneUpdated := appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
	starting := appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, UnavailableReplicas: 1}
	pullError := podAt("new", 2, waiting("ImagePullBackOff", `Back-off pulling image "x"`))
	oldPod := podAt("old", 1, podReady)

	cases := map[string]struct {
		dep         *appsv1.Deployment
		pods        []corev1.Pod
		wantReady   bool
		wantReason  string
		wantMessage string
		// notMessage must not be part of the message.
		notMessage string
	}{
		"rollout complete": {
			dep: deploymentWith(2, complete), pods: []corev1.Pod{oldPod},
			wantReady: true, wantReason: "PodRunning", wantMessage: "pgwatch pod is running",
		},
		// The bug this replaces: one ready replica was enough.
		"template just applied, Deployment controller has not looked yet": {
			dep: deploymentWith(3, complete), pods: []corev1.Pod{oldPod},
			wantReason: "RolloutInProgress", wantMessage: "Deployment controller",
		},
		// Stale conditions and pods must not be read for a template the
		// Deployment controller has not seen.
		"not observed yet, although the previous rollout had failed": {
			dep: deploymentWith(3, withConditions(stuck,
				depCondition(appsv1.DeploymentReplicaFailure, corev1.ConditionTrue, "FailedCreate", "quota"))),
			pods:       []corev1.Pod{pullError},
			wantReason: "RolloutInProgress", wantMessage: "Deployment controller",
		},
		"new pod not up yet, old pod still serving": {
			dep: deploymentWith(2, stuck), pods: []corev1.Pod{oldPod, podAt("new", 2, podPhase(corev1.PodPending))},
			wantReason: "RolloutInProgress", wantMessage: "1 of 1 updated, 1 available); the pod of the previous spec keeps running",
		},
		"nothing created yet": {
			dep:        deploymentWith(2, appsv1.DeploymentStatus{ObservedGeneration: 2}),
			wantReason: "RolloutInProgress", wantMessage: "(0 of 1 updated, 0 available)", notMessage: "previous spec",
		},
		// What `kubectl rollout status` calls "0 out of 1 new replicas have
		// been updated": the one replica there is belongs to the old spec.
		"template seen, no pod of it yet, old pod serving": {
			dep: deploymentWith(2, noneUpdated), pods: []corev1.Pod{oldPod},
			wantReason:  "RolloutInProgress",
			wantMessage: "(0 of 1 updated, 1 available); the pod of the previous spec keeps running",
		},
		"template seen, no pod of it yet, old pod not available either": {
			dep:        deploymentWith(2, appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UnavailableReplicas: 1}),
			wantReason: "RolloutInProgress", wantMessage: "(0 of 1 updated, 0 available); the pod of the previous spec",
		},
		// A quota that blocks the first pod of a new template, for good.
		"no pod of the template can be created, old pod serving": {
			dep: deploymentWith(2, withConditions(noneUpdated,
				depCondition(appsv1.DeploymentReplicaFailure, corev1.ConditionTrue, "FailedCreate", "exceeded quota"))),
			pods:       []corev1.Pod{oldPod},
			wantReason: "PodCreateFailed", wantMessage: "exceeded quota",
		},
		// The image was corrected: the pod that could not pull the old one
		// is still there, the pod of the new one is not yet.
		"an old pod's trouble is not read before the new template has a pod": {
			dep:        deploymentWith(2, appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UnavailableReplicas: 1}),
			pods:       []corev1.Pod{podAt("old", 1, waiting("ImagePullBackOff", "old image"))},
			wantReason: "RolloutInProgress", notMessage: "old image",
		},
		// An evicted pod is never started again and stays until it is
		// garbage collected.
		"an evicted pod next to its replacement that is starting": {
			dep: deploymentWith(2, starting),
			pods: []corev1.Pod{
				podAt("evicted", 1, podPhase(corev1.PodFailed), pgwatchContainer(func(cs *corev1.ContainerStatus) {
					cs.State.Terminated = exited(137, "ContainerStatusUnknown")
				})),
				podAt("replacement", 2, waiting("ContainerCreating", ""), podUnready),
			},
			wantReason: "PodNotReady", wantMessage: "pgwatch pod is not ready yet",
		},
		"a pod with an injected init container that is starting": {
			dep:        deploymentWith(2, starting),
			pods:       []corev1.Pod{podAt("new", 2, waiting("ContainerCreating", ""), podUnready, initContainer("istio-init", completed))},
			wantReason: "PodNotReady", wantMessage: "pgwatch pod is not ready yet",
		},
		"a container that exited once": {
			dep: deploymentWith(2, starting),
			pods: []corev1.Pod{podAt("new", 2, podUnready, pgwatchContainer(func(cs *corev1.ContainerStatus) {
				cs.State.Terminated = exited(137, "OOMKilled")
			}))},
			wantReason:  "PodNotReady",
			wantMessage: "Container pgwatch of pod new exited (exit code 137, OOMKilled) and is being restarted",
		},
		// Progressing is Unknown on a Deployment that was paused or resumed.
		"a rollout whose progress is not known is not one that ran out of time": {
			dep: deploymentWith(2, withConditions(stuck,
				depCondition(appsv1.DeploymentProgressing, corev1.ConditionUnknown, "DeploymentResumed", "resumed"))),
			pods:       []corev1.Pod{oldPod},
			wantReason: "RolloutInProgress", wantMessage: "Waiting for the collector pod",
		},
		"only the pod of the current spec exists and is not ready": {
			dep: deploymentWith(2, appsv1.DeploymentStatus{
				ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, UnavailableReplicas: 1,
			}),
			pods:       []corev1.Pod{podAt("new", 2)},
			wantReason: "PodNotReady", wantMessage: "pgwatch pod is not ready yet",
		},
		"new pod cannot pull its image": {
			dep: deploymentWith(2, stuck), pods: []corev1.Pod{oldPod, pullError},
			wantReason: "ImagePullBackOff", wantMessage: `Pod new cannot pull image "cybertecpostgresql/pgwatch:9.9.9": Back-off pulling image "x"`,
		},
		"pods cannot be created at all": {
			dep: deploymentWith(2, withConditions(stuck,
				depCondition(appsv1.DeploymentReplicaFailure, corev1.ConditionTrue, "FailedCreate",
					`pods "pgpilot-db-" is forbidden: exceeded quota`))),
			pods:       []corev1.Pod{oldPod},
			wantReason: "PodCreateFailed", wantMessage: `No collector pod could be created: pods "pgpilot-db-" is forbidden: exceeded quota`,
		},
		// The quota is why there is no new pod; what an older pod waits
		// for is beside the point.
		"pod creation failure wins over a pod's own trouble": {
			dep: deploymentWith(2, withConditions(stuck,
				depCondition(appsv1.DeploymentReplicaFailure, corev1.ConditionTrue, "FailedCreate", "quota"))),
			pods:       []corev1.Pod{pullError},
			wantReason: "PodCreateFailed", wantMessage: "quota",
		},
		"a ReplicaFailure that is over does not count": {
			dep: deploymentWith(2, withConditions(stuck,
				depCondition(appsv1.DeploymentReplicaFailure, corev1.ConditionFalse, "", ""))),
			pods:       []corev1.Pod{oldPod},
			wantReason: "RolloutInProgress", wantMessage: "Waiting for the collector pod",
		},
		"rollout ran out of time": {
			dep: deploymentWith(2, withConditions(stuck,
				depCondition(appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded",
					`ReplicaSet "pgpilot-db-7d9" has timed out progressing.`))),
			pods:       []corev1.Pod{oldPod},
			wantReason: "RolloutStuck", wantMessage: `does not progress: ReplicaSet "pgpilot-db-7d9" has timed out progressing.`,
		},
		// "Timed out" says less than what the pod is waiting for.
		"what the pod waits for wins over the deadline": {
			dep: deploymentWith(2, withConditions(stuck,
				depCondition(appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded", "timed out"))),
			pods:       []corev1.Pod{oldPod, pullError},
			wantReason: "ImagePullBackOff", wantMessage: "cannot pull image",
		},
		"a rollout that progresses is not stuck": {
			dep: deploymentWith(2, withConditions(stuck,
				depCondition(appsv1.DeploymentProgressing, corev1.ConditionTrue, "ReplicaSetUpdated", "progressing"))),
			pods:       []corev1.Pod{oldPod},
			wantReason: "RolloutInProgress", wantMessage: "Waiting for the collector pod",
		},
		"updated but an old pod is still counted": {
			dep: deploymentWith(2, appsv1.DeploymentStatus{
				ObservedGeneration: 2, Replicas: 2, UpdatedReplicas: 1, ReadyReplicas: 2, AvailableReplicas: 2,
			}),
			wantReason: "RolloutInProgress", wantMessage: "keeps running until then",
		},
		"updated replica exists but none is available": {
			dep: deploymentWith(2, appsv1.DeploymentStatus{
				ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1,
			}),
			wantReason: "PodNotReady", wantMessage: "not ready yet",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ready, reason, message := collectorState(tc.dep, tc.pods)
			if ready != tc.wantReady || reason != tc.wantReason {
				t.Errorf("got ready=%v reason=%q (%s), want ready=%v reason=%q", ready, reason, message, tc.wantReady, tc.wantReason)
			}
			if !strings.Contains(message, tc.wantMessage) {
				t.Errorf("message %q does not contain %q", message, tc.wantMessage)
			}
			if tc.notMessage != "" && strings.Contains(message, tc.notMessage) {
				t.Errorf("message %q contains %q", message, tc.notMessage)
			}
		})
	}

	t.Run("a Deployment without spec.replicas has one", func(t *testing.T) {
		dep := deploymentWith(2, complete)
		dep.Spec.Replicas = nil
		if ready, reason, _ := collectorState(dep, nil); !ready {
			t.Errorf("not ready: %s", reason)
		}
	})
	t.Run("an observed generation ahead of the one read is as good as equal", func(t *testing.T) {
		st := complete
		st.ObservedGeneration = 5
		if ready, reason, _ := collectorState(deploymentWith(2, st), nil); !ready {
			t.Errorf("not ready: %s", reason)
		}
	})
}

func TestPodProblem(t *testing.T) {
	restarted := func(pod *corev1.Pod) {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "pgwatch", RestartCount: 4,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			LastTerminationState: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"},
			},
		}}
	}
	unschedulable := func(pod *corev1.Pod) {
		pod.Status.Phase = corev1.PodPending
		pod.Status.Conditions = []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
			Message: "0/3 nodes are available: 3 Insufficient memory.",
		}}
	}

	cases := map[string]struct {
		pods        []corev1.Pod
		wantReason  string // empty: no problem found
		wantMessage string
	}{
		"no pods":                         {},
		"a pod that is just starting":     {pods: []corev1.Pod{podAt("a", 1, waiting("ContainerCreating", ""))}},
		"a pod that initialises":          {pods: []corev1.Pod{podAt("a", 1, waiting("PodInitializing", ""))}},
		"a running pod that is not ready": {pods: []corev1.Pod{podAt("a", 1)}},
		"image pull back-off": {
			pods:       []corev1.Pod{podAt("a", 1, waiting("ImagePullBackOff", "Back-off pulling image"))},
			wantReason: "ImagePullBackOff", wantMessage: "Pod a cannot pull image",
		},
		// The kubelet alternates between the two while it retries; one
		// reason for both keeps the condition still.
		"image pull error": {
			pods:       []corev1.Pod{podAt("a", 1, waiting("ErrImagePull", "manifest unknown"))},
			wantReason: "ImagePullBackOff", wantMessage: "manifest unknown",
		},
		"invalid image name": {
			pods:       []corev1.Pod{podAt("a", 1, waiting("InvalidImageName", ""))},
			wantReason: "ImagePullBackOff", wantMessage: "InvalidImageName",
		},
		// A key missing from the credentials Secret, typically.
		"container cannot be configured": {
			pods:       []corev1.Pod{podAt("a", 1, waiting("CreateContainerConfigError", `couldn't find key password in Secret team/db`))},
			wantReason: "CreateContainerConfigError", wantMessage: "Container pgwatch of pod a cannot start: couldn't find key password",
		},
		"container cannot be created": {
			pods:       []corev1.Pod{podAt("a", 1, waiting("CreateContainerError", "boom"))},
			wantReason: "CreateContainerError", wantMessage: "boom",
		},
		"crash loop, waiting to be restarted": {
			pods: []corev1.Pod{podAt("a", 1, waiting("CrashLoopBackOff", "back-off 40s"), func(pod *corev1.Pod) {
				pod.Status.ContainerStatuses[0].RestartCount = 3
				pod.Status.ContainerStatuses[0].LastTerminationState.Terminated =
					&corev1.ContainerStateTerminated{ExitCode: 2, Reason: "Error"}
			})},
			wantReason: "CrashLoopBackOff", wantMessage: "Container pgwatch of pod a keeps exiting (exit code 2, Error); restarted 3 times",
		},
		// Between two crashes the container runs for a moment. That is the
		// same trouble, and must not read as a pod that is merely starting.
		"crash loop, running again for now": {
			pods:       []corev1.Pod{podAt("a", 1, restarted)},
			wantReason: "CrashLoopBackOff", wantMessage: "(exit code 1, Error); restarted 4 times",
		},
		// One exit is not a loop: a node that restarted, one out-of-memory
		// kill. It is said, under the reason of a pod that is coming up.
		"container just exited, for the first time": {
			pods: []corev1.Pod{podAt("a", 1, pgwatchContainer(func(cs *corev1.ContainerStatus) {
				cs.State.Terminated = exited(137, "OOMKilled")
			}))},
			wantReason:  "PodNotReady",
			wantMessage: "Container pgwatch of pod a exited (exit code 137, OOMKilled) and is being restarted",
		},
		"restarted once, running, not ready yet": {
			pods: []corev1.Pod{podAt("a", 1, podUnready, pgwatchContainer(func(cs *corev1.ContainerStatus) {
				cs.RestartCount = 1
				cs.State.Running = &corev1.ContainerStateRunning{}
				cs.LastTerminationState.Terminated = exited(255, "Unknown")
			}))},
			wantReason: "PodNotReady", wantMessage: "exited (exit code 255, Unknown) and is being restarted",
		},
		"restarted twice, running, not ready yet": {
			pods: []corev1.Pod{podAt("a", 1, podUnready, pgwatchContainer(func(cs *corev1.ContainerStatus) {
				cs.RestartCount = 2
				cs.State.Running = &corev1.ContainerStateRunning{}
				cs.LastTerminationState.Terminated = exited(1, "Error")
			}))},
			wantReason: "CrashLoopBackOff", wantMessage: "keeps exiting (exit code 1, Error); restarted 2 times",
		},
		// The kubelet's own verdict stands whatever the count.
		"crash loop by the kubelet's word after one restart": {
			pods: []corev1.Pod{podAt("a", 1, waiting("CrashLoopBackOff", "back-off 10s"), func(pod *corev1.Pod) {
				pod.Status.ContainerStatuses[0].RestartCount = 1
			})},
			wantReason: "CrashLoopBackOff", wantMessage: "Container pgwatch of pod a keeps exiting; restarted 1 times",
		},
		"the exit the container is in is the one described": {
			pods: []corev1.Pod{podAt("a", 1, pgwatchContainer(func(cs *corev1.ContainerStatus) {
				cs.RestartCount = 2
				cs.State.Terminated = exited(2, "Error")
				cs.LastTerminationState.Terminated = exited(137, "OOMKilled")
			}))},
			wantReason: "CrashLoopBackOff", wantMessage: "keeps exiting (exit code 2, Error); restarted 2 times",
		},
		"an exit without a reason": {
			pods: []corev1.Pod{podAt("a", 1, pgwatchContainer(func(cs *corev1.ContainerStatus) {
				cs.RestartCount = 5
				cs.State.Terminated = exited(3, "")
			}))},
			wantReason: "CrashLoopBackOff", wantMessage: "keeps exiting (exit code 3); restarted 5 times",
		},
		// The container passed its probe; the pod's condition follows.
		"container ready again after restarts, the pod's condition not yet": {
			pods: []corev1.Pod{podAt("a", 1, podUnready, pgwatchContainer(func(cs *corev1.ContainerStatus) {
				cs.Ready, cs.RestartCount = true, 3
				cs.State.Running = &corev1.ContainerStateRunning{}
				cs.LastTerminationState.Terminated = exited(1, "Error")
			}))},
		},
		"a last exit on a container that was never restarted": {
			pods: []corev1.Pod{podAt("a", 1, pgwatchContainer(func(cs *corev1.ContainerStatus) {
				cs.State.Running = &corev1.ContainerStateRunning{}
				cs.LastTerminationState.Terminated = exited(1, "Error")
			}))},
		},
		"a restart count without a recorded exit": {
			pods: []corev1.Pod{podAt("a", 1, pgwatchContainer(func(cs *corev1.ContainerStatus) {
				cs.RestartCount = 3
				cs.State.Running = &corev1.ContainerStateRunning{}
			}))},
		},
		"image never pulled": {
			pods:       []corev1.Pod{podAt("a", 1, waiting("ErrImageNeverPull", ""))},
			wantReason: "ImagePullBackOff", wantMessage: "ErrImageNeverPull",
		},
		"container cannot be run": {
			pods:       []corev1.Pod{podAt("a", 1, waiting("RunContainerError", "exec format error"))},
			wantReason: "RunContainerError", wantMessage: "Container pgwatch of pod a cannot start: exec format error",
		},
		// What a real pod that is not ready looks like: the condition is
		// there, and false.
		"a pod with Ready=False is asked": {
			pods:       []corev1.Pod{podAt("a", 1, waiting("ImagePullBackOff", "x"), podUnready)},
			wantReason: "ImagePullBackOff",
		},
		"a pod that restarted once and is ready again": {
			pods: []corev1.Pod{podAt("a", 1, restarted, podReady, func(pod *corev1.Pod) {
				pod.Status.ContainerStatuses[0].Ready = true
			})},
		},
		"cannot be scheduled": {
			pods:       []corev1.Pod{podAt("a", 1, unschedulable)},
			wantReason: "Unschedulable", wantMessage: "Pod a cannot be scheduled: 0/3 nodes are available",
		},
		// A pod held back on purpose is not one no node can take.
		"held by a scheduling gate": {
			pods: []corev1.Pod{podAt("a", 1, unschedulable, func(pod *corev1.Pod) {
				pod.Status.Conditions[0].Reason = corev1.PodReasonSchedulingGated
			})},
		},
		"scheduled, with the reason left over": {
			pods: []corev1.Pod{podAt("a", 1, unschedulable, func(pod *corev1.Pod) {
				pod.Status.Conditions[0].Status = corev1.ConditionTrue
			})},
		},
		"another condition with that reason": {
			pods: []corev1.Pod{podAt("a", 1, unschedulable, func(pod *corev1.Pod) {
				pod.Status.Conditions[0].Type = corev1.PodReadyToStartContainers
			})},
		},
		// An init container that ran to its end stays Terminated for the
		// life of the pod.
		"a completed init container, main container being created": {
			pods: []corev1.Pod{podAt("a", 1, waiting("ContainerCreating", ""), podUnready, initContainer("istio-init", completed))},
		},
		"a completed init container, main container running and not ready": {
			pods: []corev1.Pod{podAt("a", 1, podUnready, initContainer("istio-init", completed),
				pgwatchContainer(func(cs *corev1.ContainerStatus) { cs.State.Running = &corev1.ContainerStateRunning{} }))},
		},
		"a completed init container does not hide the main container's trouble": {
			pods:       []corev1.Pod{podAt("a", 1, waiting("ImagePullBackOff", "main"), initContainer("istio-init", completed))},
			wantReason: "ImagePullBackOff", wantMessage: "main",
		},
		"an init container that failed": {
			pods: []corev1.Pod{podAt("a", 1, waiting("PodInitializing", ""),
				initContainer("vault-init", corev1.ContainerState{Terminated: exited(1, "Error")}))},
			wantReason: "PodNotReady", wantMessage: "Container vault-init of pod a exited (exit code 1, Error)",
		},
		"an init container that keeps failing": {
			pods: []corev1.Pod{podAt("a", 1, waiting("PodInitializing", ""),
				initContainer("vault-init", corev1.ContainerState{Terminated: exited(1, "Error")}),
				func(pod *corev1.Pod) { pod.Status.InitContainerStatuses[0].RestartCount = 4 })},
			wantReason: "CrashLoopBackOff", wantMessage: "Container vault-init of pod a keeps exiting",
		},
		// Main containers wait for the init containers, so what holds an
		// init container up is what holds the pod up.
		"init containers are asked first": {
			pods: []corev1.Pod{podAt("a", 1, waiting("ImagePullBackOff", "main"),
				initContainer("init", corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerConfigError"}}))},
			wantReason: "CreateContainerConfigError", wantMessage: "Container init of pod a",
		},
		"a pod that failed for good next to its replacement": {
			pods: []corev1.Pod{
				podAt("evicted", 2, podPhase(corev1.PodFailed), pgwatchContainer(func(cs *corev1.ContainerStatus) {
					cs.State.Terminated = exited(137, "ContainerStatusUnknown")
				})),
				podAt("replacement", 1, waiting("ContainerCreating", "")),
			},
		},
		"a pod that has run its course": {
			pods: []corev1.Pod{podAt("a", 1, podPhase(corev1.PodSucceeded), pgwatchContainer(func(cs *corev1.ContainerStatus) {
				cs.State.Terminated = exited(0, "Completed")
			}))},
		},
		"an init container that cannot start": {
			pods: []corev1.Pod{podAt("a", 1, func(pod *corev1.Pod) {
				pod.Status.Phase = corev1.PodPending
				pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
					Name:  "init",
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImagePull"}},
				}}
			})},
			wantReason: "ImagePullBackOff", wantMessage: "Pod a cannot pull image",
		},
		// The newest pod is the pod of the current spec.
		"the newest pod is asked first": {
			pods: []corev1.Pod{
				podAt("older", 1, waiting("CreateContainerConfigError", "old trouble")),
				podAt("newer", 2, waiting("ImagePullBackOff", "new trouble")),
			},
			wantReason: "ImagePullBackOff", wantMessage: "Pod newer",
		},
		// With one replica an older pod belongs to a previous spec.
		"an older pod is not asked when the newest has nothing to tell": {
			pods: []corev1.Pod{
				podAt("older", 1, waiting("CreateContainerConfigError", "old trouble")),
				podAt("newer", 2, waiting("ContainerCreating", "")),
			},
		},
		"the order of the list does not matter": {
			pods: []corev1.Pod{
				podAt("newer", 2, waiting("ImagePullBackOff", "new trouble")),
				podAt("older", 1, waiting("CreateContainerConfigError", "old trouble")),
			},
			wantReason: "ImagePullBackOff", wantMessage: "Pod newer",
		},
		"a pod on its way out is not asked": {
			pods: []corev1.Pod{podAt("a", 1, waiting("ImagePullBackOff", ""), podDeleting)},
		},
		"a ready pod is not asked": {
			pods: []corev1.Pod{podAt("a", 1, restarted, podReady)},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			reason, message, found := podProblem(tc.pods)
			if found != (tc.wantReason != "") || reason != tc.wantReason {
				t.Errorf("got found=%v reason=%q (%s), want reason %q", found, reason, message, tc.wantReason)
			}
			if !strings.Contains(message, tc.wantMessage) {
				t.Errorf("message %q does not contain %q", message, tc.wantMessage)
			}
		})
	}
}

func TestRunningPodName(t *testing.T) {
	cases := map[string]struct {
		pods []corev1.Pod
		want string
	}{
		"no pods":                       {},
		"a pending pod":                 {pods: []corev1.Pod{podAt("a", 1, podPhase(corev1.PodPending))}},
		"a failed pod":                  {pods: []corev1.Pod{podAt("a", 1, podPhase(corev1.PodFailed))}},
		"a pod that has run its course": {pods: []corev1.Pod{podAt("a", 1, podPhase(corev1.PodSucceeded))}},
		"a pod that is being deleted":   {pods: []corev1.Pod{podAt("a", 1, podReady, podDeleting)}},
		"one running pod":               {pods: []corev1.Pod{podAt("a", 1)}, want: "a"},
		// During a rollout: the old pod serves, the new one is not up yet.
		"a ready pod before a newer one that is not ready": {
			pods: []corev1.Pod{podAt("new", 2), podAt("old", 1, podReady)}, want: "old",
		},
		"a ready pod before a newer one that is not ready, listed first": {
			pods: []corev1.Pod{podAt("old", 1, podReady), podAt("new", 2)}, want: "old",
		},
		// The kubelet's way of saying "not ready": the condition, false.
		"a ready pod before a newer one whose Ready condition is false": {
			pods: []corev1.Pod{podAt("new", 2, podUnready), podAt("old", 1, podReady)}, want: "old",
		},
		"a ready pod listed before a newer one whose Ready condition is false": {
			pods: []corev1.Pod{podAt("old", 1, podReady), podAt("new", 2, podUnready)}, want: "old",
		},
		"the newest of two ready pods": {
			pods: []corev1.Pod{podAt("old", 1, podReady), podAt("new", 2, podReady)}, want: "new",
		},
		"the newest of two pods that are not ready": {
			pods: []corev1.Pod{podAt("new", 2), podAt("old", 1)}, want: "new",
		},
		"the order of the list does not matter": {
			pods: []corev1.Pod{podAt("new", 2, podReady), podAt("old", 1, podReady)}, want: "new",
		},
		"pods of the same second, by name": {
			pods: []corev1.Pod{podAt("b", 1, podReady), podAt("a", 1, podReady)}, want: "a",
		},
		"an evicted pod next to its replacement": {
			pods: []corev1.Pod{podAt("evicted", 2, podPhase(corev1.PodFailed)), podAt("replacement", 1, podReady)},
			want: "replacement",
		},
	}
	for name, tc := range cases {
		if got := runningPodName(tc.pods); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}

func TestRecordTransition(t *testing.T) {
	// before is the Ready condition as stored, written for generation 1 with
	// the message "why".
	ready := &metav1.Condition{
		Type: "Ready", Status: metav1.ConditionTrue, Reason: "PodRunning", Message: "why", ObservedGeneration: 1,
	}
	notReady := func(reason string) *metav1.Condition {
		return &metav1.Condition{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: reason, Message: "why", ObservedGeneration: 1,
		}
	}
	running := outcome{ready: true, reason: "PodRunning", message: "pgwatch pod is running"}
	failing := func(reason string) outcome { return outcome{reason: reason, message: "why"} }
	because := func(reason, message string) outcome { return outcome{reason: reason, message: message} }

	cases := map[string]struct {
		before *metav1.Condition
		out    outcome
		// generation of the monitor that was looked at; 1 when left out.
		generation int64
		want       string // empty: no event
	}{
		"first time ready":                   {before: nil, out: running, want: "Normal PodRunning pgwatch pod is running"},
		"ready again":                        {before: notReady("ImagePullBackOff"), out: running, want: "Normal PodRunning pgwatch pod is running"},
		"still ready":                        {before: ready, out: running},
		"a problem on the first look":        {before: nil, out: failing("SecretNotFound"), want: "Warning SecretNotFound why"},
		"the same problem again":             {before: notReady("SecretNotFound"), out: failing("SecretNotFound")},
		"another problem":                    {before: notReady("SecretNotFound"), out: failing("SecretKeyMissing"), want: "Warning SecretKeyMissing why"},
		"a problem after being ready":        {before: ready, out: failing("CrashLoopBackOff"), want: "Warning CrashLoopBackOff why"},
		"a new monitor rolling out":          {before: nil, out: failing("RolloutInProgress")},
		"a new monitor's pod starting":       {before: nil, out: failing("PodNotReady")},
		"from one waiting state to the next": {before: notReady("RolloutInProgress"), out: failing("PodNotReady")},
		"a problem resolved, rolling out":    {before: notReady("ImagePullBackOff"), out: failing("RolloutInProgress")},
		// A change of the spec of a healthy monitor.
		"a healthy monitor rolling out": {before: ready, out: failing("RolloutInProgress"), want: "Normal RolloutInProgress why"},
		// The pod of the current spec was available and no longer is.
		"a healthy monitor losing its pod": {before: ready, out: failing("PodNotReady"), want: "Warning PodNotReady why"},
		"still rolling out":                {before: notReady("RolloutInProgress"), out: failing("RolloutInProgress")},
		"invalid name":                     {before: nil, out: failing("InvalidName"), want: "Warning InvalidName why"},
		"apply rejected":                   {before: ready, out: failing("ApplyRejected"), want: "Warning ApplyRejected why"},
		"apply failed":                     {before: ready, out: failing("ApplyFailed"), want: "Warning ApplyFailed why"},

		// The spec was changed and is wrong again, in another way: a second
		// preset that does not exist, another Secret that is missing.
		"the same kind of problem in a changed spec": {
			before: notReady("InvalidPreset"), out: because("InvalidPreset", `"bar" is not a preset`), generation: 2,
			want: `Warning InvalidPreset "bar" is not a preset`,
		},
		"the very same problem although the spec changed": {
			before: notReady("SecretNotFound"), out: failing("SecretNotFound"), generation: 2,
		},
		// The kubelet words the same trouble differently from look to look.
		"the same problem in other words, the spec unchanged": {
			before: notReady("ImagePullBackOff"), out: because("ImagePullBackOff", "Back-off pulling image"),
		},
		"still rolling out, for a changed spec": {
			before: notReady("RolloutInProgress"), out: because("RolloutInProgress", "0 of 1 updated"), generation: 2,
		},
		"still ready after a change of the spec that needed no rollout": {
			before: ready, out: running, generation: 2,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			recorder := record.NewFakeRecorder(5)
			r := &PgpilotMonitorReconciler{Recorder: recorder}
			monitor := &pgpilotv1.PgpilotMonitor{}
			monitor.Generation = max(tc.generation, 1)
			r.recordTransition(monitor, tc.before, tc.out)
			events := drain(recorder)
			switch {
			case tc.want == "" && len(events) != 0:
				t.Errorf("recorded %q, want no event", events)
			case tc.want != "" && (len(events) != 1 || events[0] != tc.want):
				t.Errorf("recorded %q, want %q", events, tc.want)
			}
		})
	}
}

func TestSameStatus(t *testing.T) {
	then, now := metav1.NewTime(time.Unix(1, 0)), metav1.NewTime(time.Unix(2, 0))
	base := pgpilotv1.PgpilotMonitorStatus{
		Ready: true, PodName: "a", ObservedGeneration: 3, ConfigHash: "abc", LastReconciled: &then,
		Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "PodRunning", Message: "m"}},
	}
	later := *base.DeepCopy()
	later.LastReconciled = &now
	if !sameStatus(&base, &later) {
		t.Error("the time of writing alone must not make two statuses differ")
	}
	if base.LastReconciled != &then || later.LastReconciled != &now {
		t.Error("comparing must not change what is compared")
	}

	for name, change := range map[string]func(*pgpilotv1.PgpilotMonitorStatus){
		"ready":              func(s *pgpilotv1.PgpilotMonitorStatus) { s.Ready = false },
		"podName":            func(s *pgpilotv1.PgpilotMonitorStatus) { s.PodName = "" },
		"observedGeneration": func(s *pgpilotv1.PgpilotMonitorStatus) { s.ObservedGeneration = 4 },
		"configHash":         func(s *pgpilotv1.PgpilotMonitorStatus) { s.ConfigHash = "def" },
		"condition message":  func(s *pgpilotv1.PgpilotMonitorStatus) { s.Conditions[0].Message = "other" },
		"condition removed":  func(s *pgpilotv1.PgpilotMonitorStatus) { s.Conditions = nil },
	} {
		changed := *base.DeepCopy()
		change(&changed)
		if sameStatus(&base, &changed) {
			t.Errorf("a different %s must make two statuses differ", name)
		}
	}
}

// readyReasons collects the reasons of the Ready condition from the code:
// every string constant in monitor_status.go whose name begins with "reason".
// A reason added there is found here without anyone listing it.
func readyReasons(t *testing.T) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "monitor_status.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]bool{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "reason") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s is not a string literal", name.Name)
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				reasons[value] = true
			}
		}
	}
	return reasons
}

// Every reason a monitor can report has to be explained where users are sent
// to look it up: in the table of reasons at the top of the troubleshooting
// page. And the table must not explain reasons that no longer exist.
func TestReadyReasonsAreDocumented(t *testing.T) {
	reasons := readyReasons(t)
	for _, known := range []string{reasonPodRunning, reasonSecretNotFound, reasonRunContainerError, reasonApplyRejected} {
		if !reasons[known] {
			t.Fatalf("the reasons read from the code lack %s: %v", known, reasons)
		}
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "troubleshooting.md"))
	if err != nil {
		t.Fatal(err)
	}
	// The table: from its header to the first line that is not a row.
	_, rest, found := strings.Cut(string(raw), "| Reason | Meaning |\n")
	if !found {
		t.Fatal("docs/troubleshooting.md has no table of reasons")
	}
	documented := map[string]bool{}
	for line := range strings.SplitSeq(rest, "\n") {
		if !strings.HasPrefix(line, "|") {
			break
		}
		firstCell, _, _ := strings.Cut(strings.TrimPrefix(line, "|"), "|")
		for _, m := range regexp.MustCompile("`([A-Za-z]+)`").FindAllStringSubmatch(firstCell, -1) {
			documented[m[1]] = true
		}
	}

	for reason := range reasons {
		if !documented[reason] {
			t.Errorf("the table of reasons in docs/troubleshooting.md has no row for %s", reason)
		}
	}
	for reason := range documented {
		if !reasons[reason] {
			t.Errorf("docs/troubleshooting.md explains the reason %s, which the operator never reports", reason)
		}
	}
}

// The intervals the documentation states.
func TestStatusIntervals(t *testing.T) {
	for name, tc := range map[string]struct{ got, want time.Duration }{
		"a collector on its way up":            {notReadyRequeue, 15 * time.Second},
		"a collector that is held up":          {stuckRequeue, time.Minute},
		"a missing library":                    {libraryRetry, 30 * time.Second},
		"an object the API server refused":     {rejectedRetry, 5 * time.Minute},
		"a read the cache had not caught up":   {staleReadRetry, time.Second},
		"a missing Secret":                     {defaultCredentialsRetry, 30 * time.Second},
		"the credentials of a healthy monitor": {defaultCredentialsRecheck, 5 * time.Minute},
	} {
		if tc.got != tc.want {
			t.Errorf("%s is looked at again after %s, the documentation says %s", name, tc.got, tc.want)
		}
	}
}
