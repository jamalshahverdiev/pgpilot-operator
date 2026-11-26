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
	"errors"
	"fmt"
	"strings"
	"sync"
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

	// fieldOwner is the name under which the operator writes to the API
	// server, for server-side apply and for every other write.
	fieldOwner = "pgpilot-operator"

	// conditionCredentials reports whether the monitor has credentials the
	// collector pod can start with.
	conditionCredentials = "CredentialsAvailable"
	// conditionConfigGenerated reports whether the collector's
	// configuration could be rendered from the spec.
	conditionConfigGenerated = "ConfigGenerated"
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

	// replaced holds, per monitor, the resourceVersion the operator's last
	// status write replaced; see readIsStale.
	replaced sync.Map
}

// buildConfigMap renders the collector's configuration. A variable so that a
// test can make the rendering fail, which the embedded metric definitions
// never do.
var buildConfigMap = builder.BuildConfigMap

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
	var monitor pgpilotv1.PgpilotMonitor
	if err := r.Get(ctx, req.NamespacedName, &monitor); err != nil {
		if apierrors.IsNotFound(err) {
			r.replaced.Delete(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	// The cache still shows the monitor from before the operator's own last
	// status write: whatever is concluded from it would be compared with a
	// status that is no longer the stored one.
	if r.readIsStale(&monitor) {
		return ctrl.Result{RequeueAfter: staleReadRetry}, nil
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

	// The status is worked out in memory and written once, in finish; stored
	// is what it is compared with.
	stored := monitor.DeepCopy()
	status := newMonitorStatus(&monitor)
	return r.finish(ctx, stored, status, r.sync(ctx, status))
}

// sync brings the generated objects in line with the monitor's spec and
// reports where the monitor stands. It records the conditions of the steps it
// gets through in status; it writes no status and records no Ready event
// itself.
func (r *PgpilotMonitorReconciler) sync(ctx context.Context, status *monitorStatus) outcome {
	monitor := status.monitor
	meta.RemoveStatusCondition(&monitor.Status.Conditions, legacyConditionDatabaseReachable)

	// --- Step 0: the name has to work for everything derived from it. ---
	if err := builder.ValidateMonitorName(monitor.Name); err != nil {
		status.set(conditionConfigGenerated, metav1.ConditionFalse, reasonInvalidName, err.Error())
		return outcome{reason: reasonInvalidName, message: err.Error()}
	}

	// --- Step 1: Validate credentials source. ---
	creds, stop := r.resolveCredentials(ctx, status)
	if stop != nil {
		return *stop
	}

	// --- Step 2: Resolve metric libraries. ---
	libraries, err := r.resolveLibraries(ctx, monitor)
	if err != nil {
		message := "Failed to resolve metric libraries: " + err.Error()
		status.set(conditionConfigGenerated, metav1.ConditionFalse, reasonLibraryResolveFailed, message)
		out := outcome{reason: reasonLibraryResolveFailed, message: message, requeueAfter: libraryRetry}
		if !apierrors.IsNotFound(err) {
			// Not a library that is missing, but one that could not be
			// read: that is the operator's problem, to be retried.
			out.err, out.requeueAfter = err, 0
		}
		return out
	}

	// --- Step 3: Merge metrics. ---
	merged := builder.MergeMetrics(monitor.Spec.Metrics, libraries)

	// --- Step 4: Build ConfigMap. ---
	desiredCM, configHash, err := buildConfigMap(monitor, merged)
	switch {
	case errors.Is(err, builder.ErrInvalidPreset):
		// A mistake in the spec: retrying cannot fix it, and the next
		// change of the spec starts a new reconcile anyway.
		status.set(conditionConfigGenerated, metav1.ConditionFalse, reasonInvalidPreset, err.Error())
		return outcome{reason: reasonInvalidPreset, message: err.Error()}
	case err != nil:
		message := "The collector configuration could not be rendered: " + err.Error()
		status.set(conditionConfigGenerated, metav1.ConditionFalse, reasonConfigBuildFailed, message)
		return outcome{reason: reasonConfigBuildFailed, message: message, err: fmt.Errorf("build configmap: %w", err)}
	}
	if stop := r.apply(ctx, monitor, desiredCM); stop != nil {
		status.set(conditionConfigGenerated, metav1.ConditionFalse, stop.reason, stop.message)
		return *stop
	}

	status.set(conditionConfigGenerated, metav1.ConditionTrue, "ConfigMapUpdated",
		fmt.Sprintf("ConfigMap %s up to date (hash=%s)", desiredCM.Name, configHash[:12]))
	if monitor.Status.ConfigHash != configHash {
		status.event(corev1.EventTypeNormal, "ConfigMapUpdated",
			"ConfigMap %s reconciled (hash=%s)", desiredCM.Name, configHash[:12])
		monitor.Status.ConfigHash = configHash
	}

	// --- Step 5: Build Deployment. ---
	deployment := builder.BuildDeployment(monitor, merged, builder.PodTemplateVersions{
		Config:      configHash,
		Credentials: creds.version,
	})
	if stop := r.apply(ctx, monitor, deployment); stop != nil {
		return *stop
	}

	// --- Step 6: Build Service. ---
	if stop := r.apply(ctx, monitor, builder.BuildService(monitor)); stop != nil {
		return *stop
	}

	// --- Step 6b: Build ServiceMonitor (if prometheus-operator CRD present). ---
	if r.ServiceMonitorEnabled {
		if stop := r.apply(ctx, monitor, builder.BuildServiceMonitor(monitor)); stop != nil {
			return *stop
		}
	}

	// --- Step 7: Where does the rollout stand? ---
	// The apply above returned the Deployment as the API server holds it
	// now. A read from the cache could still show the Deployment from before
	// the apply, complete and available, and pass it off as the new one.
	pods, _ := r.collectorPods(ctx, status)
	ready, reason, message := collectorState(deployment, pods)
	out := outcome{ready: ready, reason: reason, message: message}
	switch {
	case !ready && transitional[reason]:
		out.requeueAfter = notReadyRequeue
	case !ready:
		out.requeueAfter = stuckRequeue
	case creds.fromSecret:
		// Come back to read the Secret again; see CredentialsRecheck. The
		// jitter keeps the monitors from all coming due in the same instant
		// after the operator has started.
		out.requeueAfter = wait.Jitter(r.credentialsRecheck(), recheckJitter)
	}
	return out
}

// apply writes one generated object, owned by the monitor. It returns nil
// when the object is in place and otherwise how the reconcile ends.
//
// An object the API server refuses as invalid will be refused again however
// often it is sent, as long as nothing changes: that is reported as a state,
// not as a failed reconcile. It is sent again when the spec changes and
// otherwise every rejectedRetry, because what refused it need not be the
// spec: an admission policy answers the same way. Any other failure is handed
// to the work queue to be retried with backoff.
func (r *PgpilotMonitorReconciler) apply(
	ctx context.Context,
	monitor *pgpilotv1.PgpilotMonitor,
	obj client.Object,
) *outcome {
	// Read before the object is sent: the client clears it in the answer.
	kind := obj.GetObjectKind().GroupVersionKind().Kind
	what := fmt.Sprintf("%s %s", kind, obj.GetName())

	err := ctrl.SetControllerReference(monitor, obj, r.Scheme)
	if err == nil {
		err = r.serverSideApply(ctx, obj)
	}
	switch {
	case err == nil:
		return nil
	case apierrors.IsInvalid(err):
		return &outcome{
			reason:       reasonApplyRejected,
			message:      fmt.Sprintf("%s was rejected by the API server: %v", what, err),
			requeueAfter: wait.Jitter(rejectedRetry, recheckJitter),
		}
	default:
		return &outcome{
			reason:  reasonApplyFailed,
			message: fmt.Sprintf("%s could not be applied: %v", what, err),
			err:     fmt.Errorf("reconcile %s: %w", strings.ToLower(kind), err),
		}
	}
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
	// fromSecret is true when the credentials come from a referenced Secret.
	fromSecret bool
	// version changes whenever that Secret changes; empty for inline
	// credentials.
	version string
}

// resolveCredentials checks that the monitor has credentials the collector
// pod can start with: an inline username and password, or a Secret in the
// monitor's namespace that holds a value under both configured keys. When it
// has not, the second result says how the reconcile ends.
//
// It does not check that the database accepts them. pgwatch keeps running and
// retrying when it cannot log in, so that only shows in the pod's log and in
// the pgwatch_instance_up metric.
//
// A missing Secret or key is a state the user has to change: it is reported
// in status and looked at again after CredentialsRetry. The reconcile fails
// only when the Secret could not be read at all, so that the work queue
// retries with backoff and the failure counts as a reconcile error.
func (r *PgpilotMonitorReconciler) resolveCredentials(
	ctx context.Context,
	status *monitorStatus,
) (credentials, *outcome) {
	monitor := status.monitor
	db := monitor.Spec.Database

	// unavailable reports credentials the collector cannot start with. A
	// collector started earlier may well still be running, but the monitor
	// is not in the state its spec asks for.
	unavailable := func(reason, message string) *outcome {
		status.set(conditionCredentials, metav1.ConditionFalse, reason, message)
		return &outcome{reason: reason, message: message, requeueAfter: r.credentialsRetry()}
	}

	switch {
	case db.Username != "" && db.Password != "":
		status.set(conditionCredentials, metav1.ConditionTrue, "InlineCredentials",
			"Inline username and password are set")
		return credentials{}, nil

	case db.CredentialsSecret != nil:
		ref := db.CredentialsSecret
		var secret corev1.Secret
		err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: monitor.Namespace}, &secret)
		switch {
		case apierrors.IsNotFound(err):
			return credentials{}, unavailable(reasonSecretNotFound,
				fmt.Sprintf("Secret %q not found in namespace %q", ref.Name, monitor.Namespace))
		case err != nil:
			stop := unavailable(reasonSecretReadFailed, fmt.Sprintf("Secret %q could not be read: %v", ref.Name, err))
			stop.requeueAfter = 0
			stop.err = fmt.Errorf("read credentials secret %q: %w", ref.Name, err)
			return credentials{}, stop
		}

		// A key that is absent leaves the pod in CreateContainerConfigError.
		// An empty username can never log in; an empty password can (trust
		// or peer authentication), so only its presence is required.
		usernameKey, passwordKey := builder.CredentialKeys(ref)
		if len(secret.Data[usernameKey]) == 0 {
			return credentials{}, unavailable(reasonSecretKeyMissing,
				fmt.Sprintf("Secret %q has no value under key %q", ref.Name, usernameKey))
		}
		if _, present := secret.Data[passwordKey]; !present {
			return credentials{}, unavailable(reasonSecretKeyMissing,
				fmt.Sprintf("Secret %q has no key %q", ref.Name, passwordKey))
		}

		status.set(conditionCredentials, metav1.ConditionTrue, "CredentialsFound",
			fmt.Sprintf("Secret %q provides keys %q and %q", ref.Name, usernameKey, passwordKey))
		return credentials{fromSecret: true, version: credentialsVersion(&secret)}, nil

	default:
		return credentials{}, unavailable(reasonNoCredentials,
			"spec.database has no inline username/password and no credentialsSecret")
	}
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
	return r.Patch(ctx, obj, client.Apply, client.FieldOwner(fieldOwner), client.ForceOwnership)
}

// monitorChanged selects the updates of a PgpilotMonitor that are worth a
// reconcile.
//
// A status write, made whenever the status changes, comes back through the
// watch as an update of the monitor: without a filter each such write caused
// another reconcile. A change of the spec moves the generation, as does the
// start of a deletion. Labels and annotations are let through so that
// touching a monitor still reconciles it at once.
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
		// Nothing can be enqueued, and nothing retries this: say so.
		logf.FromContext(ctx).Error(err, "a changed metric library could not be mapped to its monitors; "+
			"they pick the change up at their next reconcile", "library", lib.Name, "namespace", lib.Namespace)
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
