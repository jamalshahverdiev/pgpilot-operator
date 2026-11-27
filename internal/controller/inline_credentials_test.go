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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

const generatedSecret = "pgpilot-db-credentials"

func inlineUnitMonitor(password string) *pgpilotv1.PgpilotMonitor {
	monitor := unitMonitor()
	monitor.Spec.Database.CredentialsSecret = nil
	monitor.Spec.Database.Username, monitor.Spec.Database.Password = "alice", password
	return monitor
}

func (h *reconcileHarness) storedSecret(name string) (*corev1.Secret, error) {
	var secret corev1.Secret
	err := h.client.Get(context.Background(), types.NamespacedName{Namespace: "team", Name: name}, &secret)
	return &secret, err
}

func (h *reconcileHarness) storedDeployment() *appsv1.Deployment {
	h.t.Helper()
	var dep appsv1.Deployment
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: "team", Name: "pgpilot-db"}, &dep); err != nil {
		h.t.Fatal(err)
	}
	return &dep
}

// Inline credentials reach the pod through a Secret the operator generates,
// not as values in the pod template.
func TestReconcile_InlineCredentialsAreKeptInAGeneratedSecret(t *testing.T) {
	const password = "Pa$$w0rd-$(PGUSER)"
	h := newReconcileHarness(t, inlineUnitMonitor(password))
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	// The Secret first: a pod that starts before it exists cannot.
	if got := strings.Join(h.applied, ","); got != "Secret,ConfigMap,Deployment,Service" {
		t.Errorf("applied %q", got)
	}
	h.expectCondition("CredentialsAvailable", metav1.ConditionTrue, "InlineCredentials",
		`the collector reads them from Secret "pgpilot-db-credentials"`)

	secret, err := h.storedSecret(generatedSecret)
	if err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["username"]) != "alice" || string(secret.Data["password"]) != password {
		t.Errorf("data %q", secret.Data)
	}
	if owner := metav1.GetControllerOf(secret); owner == nil || owner.Kind != "PgpilotMonitor" || owner.Name != "db" {
		t.Errorf("the Secret is not the monitor's: %+v", secret.OwnerReferences)
	}

	dep := h.storedDeployment()
	for _, env := range dep.Spec.Template.Spec.Containers[0].Env {
		if env.Value != "" || env.ValueFrom == nil || env.ValueFrom.SecretKeyRef.Name != generatedSecret {
			t.Errorf("env %s: value %q, from %+v", env.Name, env.Value, env.ValueFrom)
		}
	}
	version := dep.Spec.Template.Annotations[builder.AnnotationCredentialsVersion]
	if version == "" {
		t.Errorf("the pod template does not say which state of the Secret it was made for: a changed password would not restart the pod")
	}

	// That an unchanged password leaves the version alone is for a real API
	// server to show (the fake one counts every apply as a change): see the
	// spec "keeps inline credentials in a Secret of its own".

	t.Run("a changed password reaches the Secret and replaces the pod", func(t *testing.T) {
		stored := h.monitor()
		stored.Spec.Database.Password = "another"
		stored.Generation++
		if err := h.client.Update(context.Background(), stored); err != nil {
			t.Fatal(err)
		}
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		secret, err := h.storedSecret(generatedSecret)
		if err != nil || string(secret.Data["password"]) != "another" {
			t.Errorf("the Secret holds %q (%v)", secret.Data["password"], err)
		}
		if got := h.storedDeployment().Spec.Template.Annotations[builder.AnnotationCredentialsVersion]; got == version || got == "" {
			t.Errorf("the credentials version is still %q", got)
		}
	})
}

// The API server asks for inline credentials or a Secret, not both. A monitor
// stored before it did can name both; the inline ones are used, by the
// controller as by the pod template, or the pod would read a Secret nobody
// looked at.
func TestReconcile_InlineCredentialsComeBeforeASecretThatIsNamedToo(t *testing.T) {
	monitor := inlineUnitMonitor("s3cret")
	monitor.Spec.Database.CredentialsSecret = &pgpilotv1.CredentialsSecretRef{Name: "db-creds"}
	h := newReconcileHarness(t, monitor) // the Secret that is named does not exist
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.applied, ","); got != "Secret,ConfigMap,Deployment,Service" {
		t.Errorf("applied %q", got)
	}
	h.expectCondition("CredentialsAvailable", metav1.ConditionTrue, "InlineCredentials", "")
	for _, env := range h.storedDeployment().Spec.Template.Spec.Containers[0].Env {
		if env.ValueFrom.SecretKeyRef.Name != generatedSecret {
			t.Errorf("env %s reads Secret %s", env.Name, env.ValueFrom.SecretKeyRef.Name)
		}
	}
	if len(h.deleted) != 0 {
		t.Errorf("deleted %v", h.deleted)
	}
}

// The name of the generated Secret is predictable. One that is there and is
// not the monitor's is not written to: it may be anybody's.
func TestReconcile_TheGeneratedSecretNeverReplacesAnotherOne(t *testing.T) {
	foreign := &corev1.Secret{Data: map[string][]byte{"token": []byte("not ours")}}
	foreign.Name, foreign.Namespace = generatedSecret, "team"
	h := newReconcileHarness(t, inlineUnitMonitor("s3cret"), foreign)

	result, err := h.reconcile()
	if err != nil || result.RequeueAfter != nameConflictRetry {
		t.Errorf("got %+v, %v", result, err)
	}
	if len(h.applied) != 0 {
		t.Errorf("applied %v", h.applied)
	}
	h.expectCondition("Ready", metav1.ConditionFalse, "NameConflict",
		"Secret pgpilot-db-credentials already exists and is not this monitor's: it has no owner")
	h.expectCondition("CredentialsAvailable", metav1.ConditionFalse, "NameConflict", "Secret pgpilot-db-credentials")
	secret, err := h.storedSecret(generatedSecret)
	if err != nil || string(secret.Data["token"]) != "not ours" || len(secret.Data) != 1 {
		t.Errorf("the Secret that was there: %q, %v", secret.Data, err)
	}
}

func TestReconcile_TheGeneratedSecretCannotBeWritten(t *testing.T) {
	h := newReconcileHarness(t, inlineUnitMonitor("s3cret"))
	boom := apierrors.NewForbidden(corev1.Resource("secrets"), generatedSecret, errors.New("no patch on secrets"))
	h.applyError = func(obj client.Object) error {
		if _, isSecret := obj.(*corev1.Secret); isSecret {
			return boom
		}
		return nil
	}
	_, err := h.reconcile()
	if !errors.Is(err, boom) {
		t.Errorf("got %v; a role that lacks the verb is the operator's failure, to be retried and counted", err)
	}
	if got := strings.Join(h.applied, ","); got != "Secret" {
		t.Errorf("applied %q: nothing is to follow a Secret that could not be written", got)
	}
	h.expectCondition("Ready", metav1.ConditionFalse, "ApplyFailed", "Secret pgpilot-db-credentials could not be applied")
	h.expectCondition("CredentialsAvailable", metav1.ConditionFalse, "ApplyFailed", "")
}

// toSecretRef turns a stored monitor from inline credentials to a Secret of
// the given name.
func toSecretRef(h *reconcileHarness, name string) {
	h.t.Helper()
	stored := h.monitor()
	stored.Spec.Database.Username, stored.Spec.Database.Password = "", ""
	stored.Spec.Database.CredentialsSecret = &pgpilotv1.CredentialsSecretRef{Name: name}
	stored.Generation++
	if err := h.client.Update(context.Background(), stored); err != nil {
		h.t.Fatal(err)
	}
}

func credentialsRemovals(h *reconcileHarness) []string {
	var out []string
	for _, e := range drain(h.recorder) {
		if strings.HasPrefix(e, "Normal CredentialsSecretRemoved ") {
			out = append(out, e)
		}
	}
	return out
}

// switched returns a monitor that was reconciled with inline credentials and
// then turned to its own Secret, db-creds.
func switched(t *testing.T, objs ...client.Object) *reconcileHarness {
	t.Helper()
	h := newReconcileHarness(t, inlineUnitMonitor("s3cret"), append(objs, runningPod("pgpilot-db-abc"))...)
	h.deploymentStatus = rolledOut
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	drain(h.recorder)
	toSecretRef(h, "db-creds")
	return h
}

// stillThere fails unless the generated Secret was left alone.
func stillThere(t *testing.T, h *reconcileHarness) {
	t.Helper()
	if len(h.deleted) != 0 {
		t.Errorf("deleted %v", h.deleted)
	}
	if _, err := h.storedSecret(generatedSecret); err != nil {
		t.Errorf("the generated Secret: %v", err)
	}
	if removed := credentialsRemovals(h); len(removed) != 0 {
		t.Errorf("events %q", removed)
	}
}

// A monitor that turns from inline credentials to a Secret of its own leaves
// a Secret with its old password behind. It goes once no pod reads it any
// more: the pod of the previous template names it for as long as it runs,
// and runs until the new one is available.
func TestReconcile_TheGeneratedSecretGoesWithTheInlineCredentials(t *testing.T) {
	t.Run("not while the pod of the previous template still runs", func(t *testing.T) {
		h := switched(t, unitSecret())
		for name, status := range map[string]*appsv1.DeploymentStatus{
			"the new pod is not there yet":      {Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1},
			"the new pod is not available yet":  {Replicas: 2, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1},
			"the new pod cannot start, for now": {Replicas: 2, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1, UnavailableReplicas: 1},
		} {
			h.deploymentStatus = status
			if _, err := h.reconcile(); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if got := strings.Join(h.applied, ","); got != "ConfigMap,Deployment,Service" {
				t.Errorf("%s: applied %q", name, got)
			}
			stillThere(t, h)
		}

		// The rollout is through: every pod reads the monitor's own Secret.
		h.deploymentStatus = rolledOut
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("Ready", metav1.ConditionTrue, "PodRunning", "")
		if got := strings.Join(h.deleted, ","); got != "*v1.Secret pgpilot-db-credentials" {
			t.Errorf("deleted %q", got)
		}
		if _, err := h.storedSecret(generatedSecret); !apierrors.IsNotFound(err) {
			t.Errorf("the generated Secret is still there: %v", err)
		}
		if _, err := h.storedSecret("db-creds"); err != nil {
			t.Errorf("the monitor's own Secret: %v", err)
		}
		for _, env := range h.storedDeployment().Spec.Template.Spec.Containers[0].Env {
			if env.ValueFrom.SecretKeyRef.Name != "db-creds" {
				t.Errorf("env %s still reads %s", env.Name, env.ValueFrom.SecretKeyRef.Name)
			}
		}
		removed := credentialsRemovals(h)
		if len(removed) != 1 || !strings.Contains(removed[0],
			"Secret pgpilot-db-credentials removed: the monitor no longer has inline credentials, and no pod reads it any more") {
			t.Errorf("events %q", removed)
		}

		// Gone: nothing more to remove, and nothing more to say.
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		if len(h.deleted) != 0 || len(credentialsRemovals(h)) != 0 {
			t.Errorf("deleted %v again", h.deleted)
		}
	})

	// The pod of the collector that runs still names the generated Secret,
	// and has to find it when it is started again.
	t.Run("kept while the monitor's Secret is missing", func(t *testing.T) {
		h := switched(t)
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("Ready", metav1.ConditionFalse, "SecretNotFound", "")
		stillThere(t, h)
	})

	for _, failing := range []string{"ConfigMap", "Deployment"} {
		t.Run("kept when the "+failing+" could not be applied", func(t *testing.T) {
			h := switched(t, unitSecret())
			boom := apierrors.NewServiceUnavailable("etcd leader changed")
			h.applyError = func(obj client.Object) error {
				if obj.GetObjectKind().GroupVersionKind().Kind == failing {
					return boom
				}
				return nil
			}
			if _, err := h.reconcile(); !errors.Is(err, boom) {
				t.Fatalf("got %v", err)
			}
			stillThere(t, h)
			// What is stored still reads it.
			for _, env := range h.storedDeployment().Spec.Template.Spec.Containers[0].Env {
				if failing == "Deployment" && env.ValueFrom.SecretKeyRef.Name != generatedSecret {
					t.Errorf("env %s reads %s", env.Name, env.ValueFrom.SecretKeyRef.Name)
				}
			}
		})
	}

}

// What the removal leaves alone, and what happens when it cannot be done.
func TestReconcile_RemovingTheGeneratedSecret(t *testing.T) {
	t.Run("a Secret of that name that is not the monitor's is left alone", func(t *testing.T) {
		foreign := &corev1.Secret{Data: map[string][]byte{"token": []byte("not ours")}}
		foreign.Name, foreign.Namespace = generatedSecret, "team"
		h := newReconcileHarness(t, unitMonitor(), unitSecret(), foreign, runningPod("pgpilot-db-abc"))
		h.deploymentStatus = rolledOut
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("Ready", metav1.ConditionTrue, "PodRunning", "")
		if len(h.deleted) != 0 {
			t.Errorf("deleted %v", h.deleted)
		}
		if got := strings.Join(h.applied, ","); got != "ConfigMap,Deployment,Service" {
			t.Errorf("applied %q", got)
		}
	})

	// The collector runs and the monitor says so; what failed is a piece of
	// tidying, which the work queue retries.
	t.Run("a Secret that cannot be removed does not make the monitor unready", func(t *testing.T) {
		h := switched(t, unitSecret())
		boom := apierrors.NewForbidden(corev1.Resource("secrets"), generatedSecret, errors.New("no delete on secrets"))
		h.deleteError = func(client.Object) error { return boom }
		result, err := h.reconcile()
		if !errors.Is(err, boom) || !strings.Contains(err.Error(), "remove secret") || result != (ctrl.Result{}) {
			t.Errorf("got %+v, %v", result, err)
		}
		h.expectCondition("Ready", metav1.ConditionTrue, "PodRunning", "")
		if got := strings.Join(h.applied, ","); got != "ConfigMap,Deployment,Service" {
			t.Errorf("applied %q", got)
		}
		if _, err := h.storedSecret(generatedSecret); err != nil {
			t.Errorf("the generated Secret: %v", err)
		}
	})

	// Nearly every monitor has no such Secret. A look for it that fails is
	// no removal that failed, and must not hold anything else back.
	t.Run("a look for it that fails says so, and leaves the monitor ready", func(t *testing.T) {
		h := newReconcileHarness(t, unitMonitor(), unitSecret(), runningPod("pgpilot-db-abc"))
		h.deploymentStatus = rolledOut
		boom := apierrors.NewServerTimeout(corev1.Resource("secrets"), "get", 1)
		h.getError = func(obj client.Object) error {
			if _, isSecret := obj.(*corev1.Secret); isSecret && obj.GetName() == generatedSecret {
				return boom
			}
			return nil
		}
		_, err := h.reconcile()
		if !errors.Is(err, boom) || !strings.Contains(err.Error(), "look for secret pgpilot-db-credentials") ||
			strings.Contains(err.Error(), "remove") {
			t.Errorf("got %v", err)
		}
		h.expectCondition("Ready", metav1.ConditionTrue, "PodRunning", "")
		if got := strings.Join(h.applied, ","); got != "ConfigMap,Deployment,Service" {
			t.Errorf("applied %q", got)
		}
	})
}

// The generated Secret has the keys a credentialsSecret has by default, and
// its name is in the monitor's own status. Naming it as the monitor's Secret
// looks like the way to take the password out of the spec, and would end
// with the operator deleting the only copy of it.
func TestReconcile_TheGeneratedSecretCannotBeTheMonitorsOwn(t *testing.T) {
	t.Run("it is refused, and nothing is touched", func(t *testing.T) {
		h := newReconcileHarness(t, inlineUnitMonitor("s3cret"), runningPod("pgpilot-db-abc"))
		h.deploymentStatus = rolledOut
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		drain(h.recorder)
		toSecretRef(h, generatedSecret)

		result, err := h.reconcile()
		// A mistake in the spec: looking again does not change it.
		if err != nil || result != (ctrl.Result{}) {
			t.Errorf("got %+v, %v", result, err)
		}
		if len(h.applied) != 0 || len(h.deleted) != 0 {
			t.Errorf("applied %v, deleted %v", h.applied, h.deleted)
		}
		for _, condition := range []string{"Ready", "CredentialsAvailable"} {
			h.expectCondition(condition, metav1.ConditionFalse, "GeneratedSecretReferenced",
				`Secret "pgpilot-db-credentials" is the one the operator generates from inline credentials`)
			h.expectCondition(condition, metav1.ConditionFalse, "GeneratedSecretReferenced", "copy it under another name")
		}
		secret, err := h.storedSecret(generatedSecret)
		if err != nil || string(secret.Data["password"]) != "s3cret" {
			t.Errorf("the generated Secret: %q, %v", secret.Data, err)
		}
		// The collector that runs still reads it.
		for _, env := range h.storedDeployment().Spec.Template.Spec.Containers[0].Env {
			if env.ValueFrom.SecretKeyRef.Name != generatedSecret {
				t.Errorf("env %s reads %s", env.Name, env.ValueFrom.SecretKeyRef.Name)
			}
		}
	})

	// A Secret of that name that the monitor does not control is the
	// user's, whatever it is called.
	t.Run("a Secret of that name that is the user's is as good as any other", func(t *testing.T) {
		own := unitSecret()
		own.Name = generatedSecret
		monitor := unitMonitor()
		monitor.Spec.Database.CredentialsSecret = &pgpilotv1.CredentialsSecretRef{Name: generatedSecret}
		h := newReconcileHarness(t, monitor, own, runningPod("pgpilot-db-abc"))
		h.deploymentStatus = rolledOut
		if _, err := h.reconcile(); err != nil {
			t.Fatal(err)
		}
		h.expectCondition("CredentialsAvailable", metav1.ConditionTrue, "CredentialsFound", "")
		h.expectCondition("Ready", metav1.ConditionTrue, "PodRunning", "")
		if len(h.deleted) != 0 {
			t.Errorf("deleted %v", h.deleted)
		}
	})
}

// The annotation of the pod template is read by everyone who may read pods.
// It says which Secret object the pod was made for, and nothing of what the
// Secret holds: a fingerprint of the password would let a guess be checked.
func TestReconcile_TheVersionOfInlineCredentialsIsThatOfTheGeneratedSecret(t *testing.T) {
	h := newReconcileHarness(t, inlineUnitMonitor("s3cret"))
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	secret, err := h.storedSecret(generatedSecret)
	if err != nil {
		t.Fatal(err)
	}
	got := h.storedDeployment().Spec.Template.Annotations[builder.AnnotationCredentialsVersion]
	if want := credentialsVersion(secret); got != want || got == "" {
		t.Errorf("credentials version %q, want that of the stored Secret, %q", got, want)
	}
}

// Someone, or some tool, wrote to the generated Secret. It is the operator's
// to say what it holds: the next reconcile puts the password of the spec
// back, and must not stop at a conflict over who owns the field.
func TestReconcile_AGeneratedSecretWrittenToBySomeoneElseIsTakenBack(t *testing.T) {
	monitor := inlineUnitMonitor("s3cret")
	monitor.UID = "the-monitor"
	edited := &corev1.Secret{Data: map[string][]byte{"username": []byte("alice"), "password": []byte("edited by hand")}}
	edited.Name, edited.Namespace = generatedSecret, "team"
	edited.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: pgpilotv1.GroupVersion.String(), Kind: "PgpilotMonitor", Name: "db", UID: "the-monitor",
		Controller: new(true),
	}}
	h := newReconcileHarness(t, monitor, edited)
	if _, err := h.reconcile(); err != nil {
		t.Fatal(err)
	}
	h.expectCondition("CredentialsAvailable", metav1.ConditionTrue, "InlineCredentials", "")
	secret, err := h.storedSecret(generatedSecret)
	if err != nil || string(secret.Data["password"]) != "s3cret" {
		t.Errorf("the Secret holds %q (%v)", secret.Data["password"], err)
	}
}

// One read of a Secret for each thing the operator has to know, and no more:
// they are not cached, every one is a request to the API server.
func TestReconcile_HowOftenSecretsAreRead(t *testing.T) {
	secretReads := func(h *reconcileHarness) int {
		count := 0
		for _, kind := range h.read {
			if kind == "Secret" {
				count++
			}
		}
		return count
	}

	t.Run("a monitor with a Secret of its own: that Secret, and a look for a generated one to remove", func(t *testing.T) {
		h := newReconcileHarness(t, unitMonitor(), unitSecret(), runningPod("pgpilot-db-abc"))
		h.deploymentStatus = rolledOut
		for range 2 {
			if _, err := h.reconcile(); err != nil {
				t.Fatal(err)
			}
		}
		if got := secretReads(h); got != 2 {
			t.Errorf("%d Secret reads in a reconcile of a healthy monitor: %v", got, h.read)
		}
	})

	t.Run("a monitor with inline credentials: the generated Secret, before it is written", func(t *testing.T) {
		h := newReconcileHarness(t, inlineUnitMonitor("s3cret"), runningPod("pgpilot-db-abc"))
		h.deploymentStatus = rolledOut
		for range 2 {
			if _, err := h.reconcile(); err != nil {
				t.Fatal(err)
			}
		}
		if got := secretReads(h); got != 1 {
			t.Errorf("%d Secret reads in a reconcile of a healthy monitor: %v", got, h.read)
		}
	})
}
