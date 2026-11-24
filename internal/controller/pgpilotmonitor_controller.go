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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

const (
	finalizerName = "pgpilot.io/finalizer"

	// conditionCredentials reports whether the monitor has credentials the
	// collector pod can start with.
	conditionCredentials = "CredentialsAvailable"
	// legacyConditionDatabaseReachable is the name the condition above had
	// in earlier releases. Nothing ever connected to the database to set it,
	// so the name promised more than it checked.
	legacyConditionDatabaseReachable = "DatabaseReachable"

	// defaultCredentialsRecheck and defaultCredentialsRetry are used when the
	// reconciler's intervals are left at zero.
	defaultCredentialsRecheck = 5 * time.Minute
	defaultCredentialsRetry   = 30 * time.Second

	// recheckJitter spreads a recheck over [interval, interval*(1+jitter)].
	recheckJitter = 0.1
)

// PgpilotMonitorReconciler reconciles a PgpilotMonitor object.
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

type PgpilotMonitorReconciler struct {
	client.Client
	Scheme                *runtime.Scheme
	Recorder              record.EventRecorder
	ServiceMonitorEnabled bool

	// CredentialsRecheck is how often a monitor that takes its credentials
	// from a Secret reads that Secret again. Secrets are not watched (that
	// would need permission to list every Secret in the cluster), so this is
	// what notices a rotated password and rolls the collector pod. Zero
	// means five minutes.
	CredentialsRecheck time.Duration
	// CredentialsRetry is how long to wait before looking again for a Secret
	// that is missing or lacks a key. Zero means thirty seconds.
	CredentialsRetry time.Duration
}

func (r *PgpilotMonitorReconciler) credentialsRecheck() time.Duration {
	if r.CredentialsRecheck > 0 {
		return r.CredentialsRecheck
	}
	return defaultCredentialsRecheck
}

func (r *PgpilotMonitorReconciler) credentialsRetry() time.Duration {
	if r.CredentialsRetry > 0 {
		return r.CredentialsRetry
	}
	return defaultCredentialsRetry
}

// +kubebuilder:rbac:groups=pgpilot.io,resources=pgpilotmonitors,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=pgpilot.io,resources=pgpilotmonitors/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pgpilot.io,resources=pgpilotmonitors/finalizers,verbs=update
// +kubebuilder:rbac:groups=pgpilot.io,resources=pgpilotmetriclibraries,verbs=get;list;watch
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// Secrets are only ever read one at a time, by name: no list, no watch. The
// manager's client is configured not to cache them (see ClientOptions).
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete

func (r *PgpilotMonitorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var monitor pgpilotv1.PgpilotMonitor
	if err := r.Get(ctx, req.NamespacedName, &monitor); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion via finalizer.
	if !monitor.DeletionTimestamp.IsZero() {
		if err := r.patchFinalizer(ctx, &monitor, controllerutil.RemoveFinalizer); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is set.
	if err := r.patchFinalizer(ctx, &monitor, controllerutil.AddFinalizer); err != nil {
		return ctrl.Result{}, err
	}

	// --- Step 1: Validate credentials source. ---
	creds, result, err := r.resolveCredentials(ctx, &monitor)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !creds.usable {
		return result, nil
	}

	// --- Step 2: Resolve metric libraries. ---
	libraries, err := r.resolveLibraries(ctx, &monitor)
	if err != nil {
		log.Error(err, "failed to resolve metric libraries")
		r.setCondition(&monitor, "Ready", metav1.ConditionFalse, "LibraryResolveFailed", err.Error())
		r.Recorder.Eventf(&monitor, corev1.EventTypeWarning, "LibraryResolveFailed",
			"Failed to resolve metric libraries: %v", err)
		if statusErr := r.patchStatus(ctx, &monitor); statusErr != nil {
			log.Error(statusErr, "failed to update status")
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// --- Step 3: Merge metrics. ---
	merged := builder.MergeMetrics(monitor.Spec.Metrics, libraries)

	// --- Step 4: Build ConfigMap. ---
	desiredCM, configHash, err := builder.BuildConfigMap(&monitor, merged)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("build configmap: %w", err)
	}
	if err := ctrl.SetControllerReference(&monitor, desiredCM, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.serverSideApply(ctx, desiredCM); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile configmap: %w", err)
	}

	r.setCondition(&monitor, "ConfigGenerated", metav1.ConditionTrue, "ConfigMapUpdated",
		fmt.Sprintf("ConfigMap %s up to date (hash=%s)", desiredCM.Name, configHash[:12]))
	if monitor.Status.ConfigHash != configHash {
		r.Recorder.Eventf(&monitor, corev1.EventTypeNormal, "ConfigMapUpdated",
			"ConfigMap %s reconciled (hash=%s)", desiredCM.Name, configHash[:12])
		monitor.Status.ConfigHash = configHash
	}

	// --- Step 5: Build Deployment. ---
	desiredDep := builder.BuildDeployment(&monitor, merged, builder.PodTemplateVersions{
		Config:      configHash,
		Credentials: creds.version,
	})
	if err := ctrl.SetControllerReference(&monitor, desiredDep, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.serverSideApply(ctx, desiredDep); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile deployment: %w", err)
	}

	// --- Step 6: Build Service. ---
	desiredSvc := builder.BuildService(&monitor)
	if err := ctrl.SetControllerReference(&monitor, desiredSvc, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.serverSideApply(ctx, desiredSvc); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile service: %w", err)
	}

	// --- Step 6b: Build ServiceMonitor (if prometheus-operator CRD present). ---
	if r.ServiceMonitorEnabled {
		desiredSM := builder.BuildServiceMonitor(&monitor)
		if err := ctrl.SetControllerReference(&monitor, desiredSM, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.serverSideApply(ctx, desiredSM); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconcile servicemonitor: %w", err)
		}
	}

	// --- Step 7: Read Deployment status and update Monitor status. ---
	var currentDep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: desiredDep.Name, Namespace: desiredDep.Namespace}, &currentDep); err != nil {
		return ctrl.Result{}, err
	}

	wasReady := monitor.Status.Ready
	ready := currentDep.Status.ReadyReplicas > 0
	monitor.Status.Ready = ready
	monitor.Status.ObservedGeneration = monitor.Generation
	monitor.Status.LastReconciled = &metav1.Time{Time: time.Now()}

	if ready {
		r.setCondition(&monitor, "Ready", metav1.ConditionTrue, "PodRunning", "pgwatch pod running and scraping")
		if !wasReady {
			r.Recorder.Event(&monitor, corev1.EventTypeNormal, "PodRunning", "pgwatch pod running and scraping")
		}
	} else {
		r.setCondition(&monitor, "Ready", metav1.ConditionFalse, "PodNotReady", "pgwatch pod is not ready yet")
		if wasReady {
			r.Recorder.Event(&monitor, corev1.EventTypeWarning, "PodNotReady", "pgwatch pod is not ready yet")
		}
	}

	// Try to find the pod name.
	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(monitor.Namespace),
		client.MatchingLabels{
			builder.LabelAppInstance: monitor.Name,
			builder.LabelAppName:     builder.AppName,
		},
	); err == nil && len(podList.Items) > 0 {
		monitor.Status.PodName = podList.Items[0].Name
	}

	if err := r.patchStatus(ctx, &monitor); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}

	if !ready {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	if creds.fromSecret {
		// Come back to read the Secret again; see CredentialsRecheck. The
		// jitter keeps the monitors from all coming due in the same instant
		// after the operator has started.
		return ctrl.Result{RequeueAfter: wait.Jitter(r.credentialsRecheck(), recheckJitter)}, nil
	}
	return ctrl.Result{}, nil
}

// patchFinalizer adds or removes the operator's finalizer with a patch that
// carries metadata.finalizers and nothing else.
//
// It must not be a full Update of the object. An Update sends the spec back
// as the Go struct serialises it: optional structs turn into `{}` (which the
// API server then fills with defaults, so the operator ends up owning fields
// the user never set and the generation moves), and explicit zero values such
// as `endpoint: ""` are dropped. To the API server that is a changed spec, so
// every validation rule is evaluated again - and an object stored before a
// rule existed could no longer lose its finalizer, leaving it stuck in
// Terminating.
func (r *PgpilotMonitorReconciler) patchFinalizer(
	ctx context.Context,
	monitor *pgpilotv1.PgpilotMonitor,
	change func(client.Object, string) bool,
) error {
	base := monitor.DeepCopy()
	if !change(monitor, finalizerName) {
		return nil
	}
	// The optimistic lock makes a concurrent change to the finalizer list
	// surface as a conflict (and a retry) instead of being overwritten.
	return r.Patch(ctx, monitor, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// credentials is what resolveCredentials found out about a monitor.
type credentials struct {
	// usable is false when the collector pod could not start with what is
	// configured; the reconcile then stops and is retried later.
	usable bool
	// fromSecret is true when the credentials come from a referenced Secret.
	fromSecret bool
	// version changes whenever that Secret changes; empty for inline
	// credentials.
	version string
}

// resolveCredentials checks that the monitor has credentials the collector
// pod can start with: an inline username and password, or a Secret in the
// monitor's namespace that holds a value under both configured keys.
//
// It does not check that the database accepts them. pgwatch keeps running and
// retrying when it cannot log in, so that only shows in the pod's log and in
// the pgwatch_instance_up metric.
//
// A missing Secret or key is a state the user has to change: it is reported
// in status and looked at again after CredentialsRetry. An error is returned
// only when the Secret could not be read at all, so that the work queue
// retries with backoff and the failure counts as a reconcile error.
func (r *PgpilotMonitorReconciler) resolveCredentials(
	ctx context.Context,
	monitor *pgpilotv1.PgpilotMonitor,
) (credentials, ctrl.Result, error) {
	db := monitor.Spec.Database
	meta.RemoveStatusCondition(&monitor.Status.Conditions, legacyConditionDatabaseReachable)

	switch {
	case db.Username != "" && db.Password != "":
		r.setCondition(monitor, conditionCredentials, metav1.ConditionTrue, "InlineCredentials",
			"Inline username and password are set")
		return credentials{usable: true}, ctrl.Result{}, nil

	case db.CredentialsSecret != nil:
		ref := db.CredentialsSecret
		var secret corev1.Secret
		err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: monitor.Namespace}, &secret)
		switch {
		case apierrors.IsNotFound(err):
			return credentials{}, r.credentialsUnavailable(ctx, monitor, "SecretNotFound",
				fmt.Sprintf("Secret %q not found in namespace %q", ref.Name, monitor.Namespace)), nil
		case err != nil:
			r.credentialsUnavailable(ctx, monitor, "SecretReadFailed",
				fmt.Sprintf("Secret %q could not be read: %v", ref.Name, err))
			return credentials{}, ctrl.Result{}, fmt.Errorf("read credentials secret %q: %w", ref.Name, err)
		}

		// A key that is absent leaves the pod in CreateContainerConfigError.
		// An empty username can never log in; an empty password can (trust
		// or peer authentication), so only its presence is required.
		usernameKey, passwordKey := builder.CredentialKeys(ref)
		if len(secret.Data[usernameKey]) == 0 {
			return credentials{}, r.credentialsUnavailable(ctx, monitor, "SecretKeyMissing",
				fmt.Sprintf("Secret %q has no value under key %q", ref.Name, usernameKey)), nil
		}
		if _, present := secret.Data[passwordKey]; !present {
			return credentials{}, r.credentialsUnavailable(ctx, monitor, "SecretKeyMissing",
				fmt.Sprintf("Secret %q has no key %q", ref.Name, passwordKey)), nil
		}

		r.setCondition(monitor, conditionCredentials, metav1.ConditionTrue, "CredentialsFound",
			fmt.Sprintf("Secret %q provides keys %q and %q", ref.Name, usernameKey, passwordKey))
		return credentials{usable: true, fromSecret: true, version: credentialsVersion(&secret)}, ctrl.Result{}, nil

	default:
		return credentials{}, r.credentialsUnavailable(ctx, monitor, "NoCredentials",
			"spec.database has no inline username/password and no credentialsSecret"), nil
	}
}

// credentialsUnavailable records why the monitor cannot run in its conditions
// and as an event, and returns when to look again.
func (r *PgpilotMonitorReconciler) credentialsUnavailable(
	ctx context.Context,
	monitor *pgpilotv1.PgpilotMonitor,
	reason, message string,
) ctrl.Result {
	r.setCondition(monitor, "Ready", metav1.ConditionFalse, reason, message)
	r.setCondition(monitor, conditionCredentials, metav1.ConditionFalse, reason, message)
	// The READY column reads status.ready. A collector started earlier may
	// well still be running, but the monitor is not in the state its spec
	// asks for, and the two must not contradict each other.
	monitor.Status.Ready = false
	monitor.Status.ObservedGeneration = monitor.Generation
	r.Recorder.Event(monitor, corev1.EventTypeWarning, reason, message)
	if err := r.patchStatus(ctx, monitor); err != nil {
		logf.FromContext(ctx).Error(err, "failed to update status")
	}
	return ctrl.Result{RequeueAfter: r.credentialsRetry()}
}

// credentialsVersion returns a token that changes whenever the Secret object
// changes. The collector resolves the Secret's values into its environment
// when it starts, so a rotated password only takes effect in a new pod; the
// token goes onto the pod template to cause that.
//
// It is derived from the Secret's identity and resourceVersion rather than
// from the values. A hash of the password on an annotation would be readable
// by everyone who may read pods and could be used to confirm guesses. The
// price is a restart of the collector also when only the Secret's labels or
// annotations change.
func credentialsVersion(secret *corev1.Secret) string {
	sum := sha256.Sum256([]byte(string(secret.UID) + "/" + secret.ResourceVersion))
	return hex.EncodeToString(sum[:8])
}

func (r *PgpilotMonitorReconciler) resolveLibraries(
	ctx context.Context,
	monitor *pgpilotv1.PgpilotMonitor,
) ([]pgpilotv1.PgpilotMetricLibrary, error) {
	refs := monitor.Spec.Metrics.FromLibraries
	if len(refs) == 0 {
		return nil, nil
	}

	libs := make([]pgpilotv1.PgpilotMetricLibrary, 0, len(refs))
	for _, ref := range refs {
		var lib pgpilotv1.PgpilotMetricLibrary
		if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: monitor.Namespace}, &lib); err != nil {
			return nil, fmt.Errorf("library %q: %w", ref.Name, err)
		}
		libs = append(libs, lib)
	}
	return libs, nil
}

func (r *PgpilotMonitorReconciler) serverSideApply(ctx context.Context, obj client.Object) error {
	obj.SetManagedFields(nil)
	obj.SetResourceVersion("")
	// controller-runtime v0.23+ added a typed Client.Apply() method that
	// takes runtime.ApplyConfiguration. Migrating would require generating
	// ApplyConfiguration types for every resource we produce (k8s
	// code-generator). Until then, the classic Patch+client.Apply pattern
	// still works and is what almost every operator in the wild uses.
	// Tracked for v1.1.
	//nolint:staticcheck // SA1019: client.Apply typed Apply migration deferred to v1.1
	return r.Patch(ctx, obj, client.Apply, client.FieldOwner("pgpilot-operator"), client.ForceOwnership)
}

// patchStatus updates the PgpilotMonitor status subresource, re-fetching and
// retrying on conflict so we don't spam ERROR logs for transient races with
// other controllers touching the same object.
func (r *PgpilotMonitorReconciler) patchStatus(ctx context.Context, monitor *pgpilotv1.PgpilotMonitor) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &pgpilotv1.PgpilotMonitor{}
		if err := r.Get(ctx, types.NamespacedName{Name: monitor.Name, Namespace: monitor.Namespace}, latest); err != nil {
			return err
		}
		latest.Status = monitor.Status
		return r.Status().Update(ctx, latest)
	})
}

func (r *PgpilotMonitorReconciler) setCondition(
	monitor *pgpilotv1.PgpilotMonitor,
	condType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	meta.SetStatusCondition(&monitor.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: monitor.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// monitorChanged selects the updates of a PgpilotMonitor that are worth a
// reconcile.
//
// Every reconcile writes status (lastReconciled at the least), and that write
// comes back through the watch as an update of the monitor: without a filter
// each reconcile caused another one. A change of the spec moves the
// generation, as does the start of a deletion. Labels and annotations are let
// through so that touching a monitor still reconciles it at once.
func monitorChanged() predicate.Predicate {
	return predicate.Or(
		predicate.GenerationChangedPredicate{},
		predicate.AnnotationChangedPredicate{},
		predicate.LabelChangedPredicate{},
	)
}

// SetupWithManager sets up the controller with the Manager.
func (r *PgpilotMonitorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&pgpilotv1.PgpilotMonitor{}, ctrlbuilder.WithPredicates(monitorChanged())).
		Owns(&corev1.ConfigMap{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{})

	if r.ServiceMonitorEnabled {
		b = b.Owns(&monitoringv1.ServiceMonitor{})
	}

	return b.Watches(
		&pgpilotv1.PgpilotMetricLibrary{},
		handler.EnqueueRequestsFromMapFunc(r.libraryToMonitors),
	).
		Named("pgpilotmonitor").
		Complete(r)
}

// libraryToMonitors finds all PgpilotMonitors in the same namespace that
// reference the changed library, and enqueues them for reconciliation.
func (r *PgpilotMonitorReconciler) libraryToMonitors(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	lib, ok := obj.(*pgpilotv1.PgpilotMetricLibrary)
	if !ok {
		return nil
	}

	var monitors pgpilotv1.PgpilotMonitorList
	if err := r.List(ctx, &monitors, client.InNamespace(lib.Namespace)); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, m := range monitors.Items {
		for _, ref := range m.Spec.Metrics.FromLibraries {
			if ref.Name == lib.Name {
				requests = append(requests, reconcile.Request{
					Name:      m.Name,
					Namespace: m.Namespace,
				})
				break
			}
		}
	}
	return requests
}
