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
	"slices"
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
	"k8s.io/apimachinery/pkg/util/validation"
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
	// fieldOwner is the name under which the operator writes to the API
	// server, for server-side apply and for every other write.
	fieldOwner = "pgpilot-operator"

	// conditionCredentials reports whether the monitor has credentials the
	// collector pod can start with.
	conditionCredentials = "CredentialsAvailable"
	// conditionConfigGenerated reports whether the collector's
	// configuration could be rendered from the spec.
	conditionConfigGenerated = "ConfigGenerated"
	// conditionServiceMonitor reports whether the ServiceMonitor is in
	// place. Only monitors that are to have one carry it: the operator found
	// the ServiceMonitor CRD when it started and the Prometheus sink is on.
	conditionServiceMonitor = "ServiceMonitorApplied"
	// reasonServiceMonitorAPIMissing: the cluster stopped serving
	// ServiceMonitors while the operator ran.
	reasonServiceMonitorAPIMissing = "ServiceMonitorAPIMissing"
	// eventNamespaceTagReplaced is recorded for a monitor whose customTags
	// name a namespace other than its own.
	eventNamespaceTagReplaced = "NamespaceTagReplaced"
	// eventScrapeTargetRemoved is recorded when the Service or the
	// ServiceMonitor of a monitor is deleted because its Prometheus sink was
	// turned off.
	eventScrapeTargetRemoved = "ScrapeTargetRemoved"
	// eventConfigMapRemoved is recorded when the ConfigMap of an earlier
	// configuration is removed, once no pod reads it any more.
	eventConfigMapRemoved = "ConfigMapRemoved"
	// eventCredentialsSecretRemoved is recorded when the Secret generated
	// from inline credentials is deleted because the monitor no longer has
	// any.
	eventCredentialsSecretRemoved = "CredentialsSecretRemoved"
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
	// APIReader reads from the API server. The Client's cache holds only
	// what the operator labelled as its own, so it cannot tell whether a
	// name is taken by something else. Nil means the Client itself reads
	// live.
	APIReader             client.Reader
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

	// CollectorRunAsUser is the user of the pgwatch container: a setting
	// of the operator, not of a monitor, because who may run as what is a
	// property of the cluster. On OpenShift it can differ by namespace; see
	// CollectorUser.RunAsUserIn.
	CollectorRunAsUser CollectorUser

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
// The one Secret the operator writes is the one it generates from the
// inline credentials of a monitor (builder.BuildCredentialsSecret): created
// and updated by server-side apply, which needs create and patch, and deleted
// when the monitor turns to a Secret of its own. RBAC cannot narrow these
// verbs to a name pattern, so the role allows them for every Secret of the
// cluster; what keeps the operator from other Secrets is its own code, which
// never writes to an object the monitor does not control (see nameIsFree).
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;patch;delete
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

	// A monitor that is being deleted needs nothing done: what was generated
	// for it is owned by it and collected with it. The operator puts no
	// finalizer on monitors any more; one left by an earlier release is let
	// go here, so that the deletion does not wait for FinalizerRelease.
	if !monitor.DeletionTimestamp.IsZero() {
		if err := r.releaseFinalizer(ctx, &monitor); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		return ctrl.Result{}, nil
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

	// A collector without a sink exits as soon as it starts. The API server
	// refuses a monitor without one; a monitor stored before it did still
	// gets here, and a Deployment that can only crash is not worth making.
	if !builder.HasSink(monitor) {
		const message = "No sink is in use: spec.sinks.prometheus is not enabled, and spec.sinks.grpc is not " +
			"enabled or names no endpoint. A collector without a sink exits as soon as it starts, so nothing " +
			"is changed for this monitor until one is enabled"
		status.set(conditionConfigGenerated, metav1.ConditionFalse, reasonNoSinkEnabled, message)
		return outcome{reason: reasonNoSinkEnabled, message: message}
	}

	// --- Step 1: Validate credentials source. ---
	creds, stop := r.resolveCredentials(ctx, status)
	if stop != nil {
		return *stop
	}
	if creds, stop = r.resolveCertificates(ctx, status, creds); stop != nil {
		return *stop
	}
	status.set(conditionCredentials, metav1.ConditionTrue, creds.reason, creds.message)

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
	// A definition the API server would refuse today can still be stored,
	// from before it did. What would be rendered from one is not what its
	// author meant; see builder.ValidateMetricDefinitions. A mistake in what
	// is stored: retrying cannot fix it, and a change of the library or of
	// the monitor starts a new reconcile.
	if message := unusableMetrics(monitor, libraries); message != "" {
		status.set(conditionConfigGenerated, metav1.ConditionFalse, reasonInvalidMetric, message)
		return outcome{reason: reasonInvalidMetric, message: message}
	}
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
		// Said where the configuration is announced, and so once for each
		// change of it: the API server accepts the key, and nothing else
		// tells the user that it is not what the series carry.
		if claimed, set := monitor.Spec.Database.CustomTags[builder.NamespaceTag]; set && claimed != monitor.Namespace {
			status.event(corev1.EventTypeWarning, eventNamespaceTagReplaced,
				"spec.database.customTags.%s is %q, and is not used: the tag is the operator's and carries the "+
					"namespace of the monitor, %q. Use another key for a value of your own",
				builder.NamespaceTag, claimed, monitor.Namespace)
		}
	}

	// --- Step 5: Build Deployment. ---
	deployment := builder.BuildDeployment(monitor, merged, builder.PodTemplateVersions{
		Config:      configHash,
		Credentials: creds.version,
	}, builder.RunAsUser(r.CollectorRunAsUser.RunAsUserIn(monitor.Namespace)))
	if stop := r.apply(ctx, monitor, deployment); stop != nil {
		return *stop
	}

	// --- Step 6: Service and ServiceMonitor, for the Prometheus sink. ---
	if stop := r.syncScrapeTargets(ctx, status); stop != nil {
		return *stop
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
	default:
		out.requeueAfter = r.nextLookAtAHealthyMonitor(monitor, creds)
	}

	// --- Step 8: The Secret of inline credentials the monitor had before. ---
	// Not before every pod reads its credentials elsewhere. The pod of the
	// previous template names the generated Secret for as long as it runs,
	// and it runs until the new one is available: for good, when the new one
	// cannot start. A pod that is started again asks for the Secret again.
	//
	// A removal that fails is the operator's to retry. It does not change
	// what the monitor is: the collector runs, and the status says so.
	if ready && !builder.HasInlineCredentials(monitor) {
		if stop := r.remove(ctx, monitor, builder.BuildCredentialsSecret(monitor), removal{
			event: eventCredentialsSecretRemoved,
			why:   "the monitor no longer has inline credentials, and no pod reads it any more",
		}); stop != nil {
			out.err = stop.err
		}
	}

	// --- Step 9: The ConfigMaps of the configurations the monitor had before. ---
	// For the same reason not before the rollout is complete: the pod of
	// the previous template mounts the ConfigMap of its own configuration.
	if ready {
		if err := r.removeEarlierConfigMaps(ctx, monitor, desiredCM.Name); err != nil {
			out.err = errors.Join(out.err, err)
		}
	}
	return out
}

// removeEarlierConfigMaps deletes the ConfigMaps of the monitor that hold a
// configuration other than the current one: those of earlier configurations,
// and the single ConfigMap of earlier releases. Only what the monitor
// controls and what carries one of its names goes; the cache holds all of
// those. Called once nothing mounts them any more.
func (r *PgpilotMonitorReconciler) removeEarlierConfigMaps(
	ctx context.Context,
	monitor *pgpilotv1.PgpilotMonitor,
	current string,
) error {
	var list corev1.ConfigMapList
	if err := r.List(ctx, &list,
		client.InNamespace(monitor.Namespace),
		client.MatchingLabels{builder.LabelMonitorName: monitor.Name},
	); err != nil {
		return fmt.Errorf("list the configmaps of the monitor, to remove those of earlier configurations: %w", err)
	}
	var failed []error
	for i := range list.Items {
		earlier := &list.Items[i]
		if earlier.Name == current || !builder.IsConfigMapName(monitor.Name, earlier.Name) ||
			!metav1.IsControlledBy(earlier, monitor) {
			continue
		}
		// Already told to go, and held by a finalizer of someone else's:
		// asking again changes nothing and would be reported each time.
		if !earlier.DeletionTimestamp.IsZero() {
			continue
		}
		// Only the object that was looked at: one created under the name
		// since is not this one.
		uid := earlier.UID
		err := r.Delete(ctx, earlier, client.Preconditions{UID: &uid})
		switch {
		case err == nil:
			why := "it holds an earlier configuration, and no pod reads it any more"
			logf.FromContext(ctx).Info("removed a generated object that is no longer wanted",
				"kind", "ConfigMap", "name", earlier.Name, "because", why)
			r.Recorder.Eventf(monitor, corev1.EventTypeNormal, eventConfigMapRemoved,
				"ConfigMap %s removed: %s", earlier.Name, why)
		case apiIsGone(err):
			// The cache is behind: it was removed at an earlier look.
		default:
			failed = append(failed, fmt.Errorf("remove configmap %s: %w", earlier.Name, err))
		}
	}
	return errors.Join(failed...)
}

// nextLookAtAHealthyMonitor says when a monitor whose collector runs is to be
// reconciled again without anything having changed; zero for never.
func (r *PgpilotMonitorReconciler) nextLookAtAHealthyMonitor(
	monitor *pgpilotv1.PgpilotMonitor,
	creds credentials,
) time.Duration {
	var next time.Duration
	if creds.fromSecret {
		// Come back to read the Secret again; see CredentialsRecheck. The
		// jitter keeps the monitors from all coming due in the same instant
		// after the operator has started.
		next = wait.Jitter(r.credentialsRecheck(), recheckJitter)
	}
	if meta.IsStatusConditionFalse(monitor.Status.Conditions, conditionServiceMonitor) {
		// The ServiceMonitor API may return. Nothing else brings the
		// operator back to a monitor with inline credentials, and the
		// recheck of a Secret can be hours away.
		if retry := wait.Jitter(rejectedRetry, recheckJitter); next == 0 || retry < next {
			next = retry
		}
	}
	return next
}

// syncScrapeTargets keeps the Service and the ServiceMonitor in line with the
// Prometheus sink. With the sink on they are applied; with it off nothing
// listens on the port they point at, so they are not created and those of an
// earlier spec are removed.
//
// The ServiceMonitor is only dealt with when the operator found its CRD at
// start. Should the cluster stop serving it later, that is reported in a
// condition of its own and the reconcile goes on: the collector does not
// depend on it.
func (r *PgpilotMonitorReconciler) syncScrapeTargets(ctx context.Context, status *monitorStatus) *outcome {
	monitor := status.monitor
	service, serviceMonitor := builder.BuildService(monitor), builder.BuildServiceMonitor(monitor)

	if !builder.PrometheusSinkEnabled(monitor) {
		status.drop(conditionServiceMonitor)
		sinkOff := removal{event: eventScrapeTargetRemoved, why: "the Prometheus sink is off"}
		if stop := r.remove(ctx, monitor, service, sinkOff); stop != nil {
			return stop
		}
		if r.ServiceMonitorEnabled {
			return r.remove(ctx, monitor, serviceMonitor, sinkOff)
		}
		return nil
	}

	if stop := r.apply(ctx, monitor, service); stop != nil {
		return stop
	}
	if !r.ServiceMonitorEnabled {
		status.drop(conditionServiceMonitor)
		return nil
	}
	err := r.write(ctx, monitor, serviceMonitor)
	if apiIsGone(err) {
		message := fmt.Sprintf("ServiceMonitor %s could not be applied: this cluster no longer serves %s (%v). "+
			"The collector is not affected. The operator tries again every few minutes; "+
			"if the prometheus-operator was removed for good, restart the operator",
			serviceMonitor.Name, monitoringv1.SchemeGroupVersion, err)
		// Said once, when it is found out: a reconcile that ended before it
		// got here leaves the condition as it is (see finish).
		if !meta.IsStatusConditionFalse(monitor.Status.Conditions, conditionServiceMonitor) {
			status.event(corev1.EventTypeWarning, reasonServiceMonitorAPIMissing, "%s", message)
		}
		status.set(conditionServiceMonitor, metav1.ConditionFalse, reasonServiceMonitorAPIMissing, message)
		return nil
	}
	if stop := applyOutcome(serviceMonitor, err); stop != nil {
		return stop
	}
	status.set(conditionServiceMonitor, metav1.ConditionTrue, "Applied",
		fmt.Sprintf("ServiceMonitor %s is in place", serviceMonitor.Name))
	return nil
}

// apiIsGone reports whether err says that the API server does not serve the
// kind at all: the mapping is gone, or the path it was known under answers
// with 404.
func apiIsGone(err error) bool {
	return meta.IsNoMatchError(err) || apierrors.IsNotFound(err)
}

// apply writes one generated object, owned by the monitor. It returns nil
// when the object is in place and otherwise how the reconcile ends.
func (r *PgpilotMonitorReconciler) apply(
	ctx context.Context,
	monitor *pgpilotv1.PgpilotMonitor,
	obj client.Object,
) *outcome {
	// The description is taken before the object is sent: the client clears
	// the kind in the answer.
	describe := describer(obj)
	return describe(r.write(ctx, monitor, obj))
}

// applyOutcome says how a reconcile ends when writing obj failed with err;
// nil when it did not fail.
func applyOutcome(obj client.Object, err error) *outcome {
	return describer(obj)(err)
}

// describer returns applyOutcome for obj as it is now.
//
// An object of the same name that is not the monitor's stays as it is, and
// so does the conflict until one of the two is gone.
//
// An object the API server refuses as invalid will be refused again however
// often it is sent, as long as nothing changes: that is reported as a state,
// not as a failed reconcile. It is sent again when the spec changes and
// otherwise every rejectedRetry, because what refused it need not be the
// spec: an admission policy answers the same way.
//
// Any other failure is handed to the work queue to be retried with backoff.
func describer(obj client.Object) func(error) *outcome {
	kind := obj.GetObjectKind().GroupVersionKind().Kind
	what := fmt.Sprintf("%s %s", kind, obj.GetName())
	return func(err error) *outcome {
		var taken *nameTakenError
		switch {
		case err == nil:
			return nil
		case errors.As(err, &taken):
			return &outcome{
				reason: reasonNameConflict,
				message: fmt.Sprintf("%s already exists and is not this monitor's: %s. It was left as it is; "+
					"delete it or recreate it under another name, or create the monitor under another name",
					what, taken.owner),
				requeueAfter: nameConflictRetry,
			}
		case apierrors.IsInvalid(err), unknownToTheCluster(err):
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
}

// unknownToTheCluster reports whether a server-side apply was refused before
// any validation, because the object could not be read by the schema of the
// API server: a field that this version of Kubernetes does not have. The
// monitor takes the pod fields of the version the operator is built with,
// and the cluster may be older.
//
// The API server answers that with a plain internal error, not with
// "invalid", although it is as final: the same object will be refused in the
// same way until the monitor is changed.
func unknownToTheCluster(err error) bool {
	return apierrors.IsInternalError(err) && strings.Contains(err.Error(), "failed to create typed patch object")
}

// write applies one generated object as the monitor's own, unless an object
// of that name exists that is not.
func (r *PgpilotMonitorReconciler) write(
	ctx context.Context,
	monitor *pgpilotv1.PgpilotMonitor,
	obj client.Object,
) error {
	if err := r.nameIsFree(ctx, monitor, obj); err != nil {
		return err
	}
	if err := ctrl.SetControllerReference(monitor, obj, r.Scheme); err != nil {
		return err
	}
	return r.serverSideApply(ctx, obj)
}

// nameTakenError says that the name of a generated object belongs to an
// object the monitor does not control.
type nameTakenError struct {
	// owner says who the object belongs to instead, as a clause about "it".
	owner string
}

func (e *nameTakenError) Error() string {
	return "the name is taken by an object that is not the monitor's: " + e.owner
}

// nameIsFree makes sure that applying obj would not take over an object that
// is not the monitor's. The generated names are predictable (pgpilot-<name>),
// and an apply that forces ownership asks no questions: a Service or
// ConfigMap that happened to carry the name would be rewritten, given the
// monitor as its owner, and deleted together with it.
//
// The cache answers for the objects the operator created. What it does not
// show is asked for at the API server: once before an object is created, and
// again at each retry while a name is taken.
//
// Looking and writing are two requests. An object that takes the name in the
// instant between them is not seen; one that was there before always is.
func (r *PgpilotMonitorReconciler) nameIsFree(
	ctx context.Context,
	monitor *pgpilotv1.PgpilotMonitor,
	obj client.Object,
) error {
	existing, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("%T is not an API object", obj)
	}
	key := client.ObjectKeyFromObject(obj)
	err := r.Get(ctx, key, existing)
	if apierrors.IsNotFound(err) && r.APIReader != nil {
		err = r.APIReader.Get(ctx, key, existing)
	}
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("look for an existing object: %w", err)
	case metav1.IsControlledBy(existing, monitor):
		return nil
	}
	owner := "it has no owner"
	if ref := metav1.GetControllerOf(existing); ref != nil {
		owner = fmt.Sprintf("it is owned by %s %s", ref.Kind, ref.Name)
		if ref.Kind == "PgpilotMonitor" && ref.Name == monitor.Name {
			owner += ", an earlier object of that name that is being removed"
		}
	}
	return &nameTakenError{owner: owner}
}

// removal says why a generated object is removed: the reason of the event
// that reports it, and the clause that ends its message.
type removal struct {
	event, why string
}

// remove deletes a generated object the spec no longer asks for. Only an
// object the monitor controls is deleted. The cache holds all of those
// except Secrets, which are read from the API server each time.
//
// What was removed is said in an event and in the log, there and then: the
// status of the monitor need not change with it, and finish records events
// only together with a status that did.
func (r *PgpilotMonitorReconciler) remove(
	ctx context.Context,
	monitor *pgpilotv1.PgpilotMonitor,
	obj client.Object,
	because removal,
) *outcome {
	kind := obj.GetObjectKind().GroupVersionKind().Kind
	existing, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return nil
	}
	err := r.Get(ctx, client.ObjectKeyFromObject(obj), existing)
	switch {
	case apiIsGone(err):
		return nil
	case err != nil:
		// Whether there is anything to remove is not known. Most of the
		// time there is not, and to say that something could not be
		// removed would be wrong.
		return &outcome{
			reason: reasonApplyFailed,
			message: fmt.Sprintf("%s %s could not be looked for, to remove it if the monitor still has it: %v",
				kind, obj.GetName(), err),
			err: fmt.Errorf("look for %s %s: %w", strings.ToLower(kind), obj.GetName(), err),
		}
	case !metav1.IsControlledBy(existing, monitor):
		return nil
	}
	// Only the object that was looked at: one created under the name since
	// is not this one.
	uid := existing.GetUID()
	err = r.Delete(ctx, existing, client.Preconditions{UID: &uid})
	switch {
	case err == nil:
		logf.FromContext(ctx).Info("removed a generated object that is no longer wanted",
			"kind", kind, "name", obj.GetName(), "because", because.why)
		r.Recorder.Eventf(monitor, corev1.EventTypeNormal, because.event,
			"%s %s removed: %s", kind, obj.GetName(), because.why)
		return nil
	case apiIsGone(err):
		return nil
	}
	return &outcome{
		reason:  reasonApplyFailed,
		message: fmt.Sprintf("%s %s is no longer wanted and could not be removed: %v", kind, obj.GetName(), err),
		err:     fmt.Errorf("remove %s: %w", strings.ToLower(kind), err),
	}
}

// releaseFinalizer removes the finalizer of earlier operator releases with a
// patch that carries metadata.finalizers and nothing else.
//
// It must not be a full Update of the object. An Update sends the spec back
// as the Go struct serialises it: optional structs turn into `{}` (which the
// API server then fills with defaults, so the operator ends up owning fields
// the user never set and the generation moves), and explicit zero values such
// as `endpoint: ""` are dropped. To the API server that is a changed spec, so
// every validation rule is evaluated again - and an object stored before a
// rule existed could no longer lose its finalizer, leaving it stuck in
// Terminating.
func (r *PgpilotMonitorReconciler) releaseFinalizer(ctx context.Context, monitor *pgpilotv1.PgpilotMonitor) error {
	base := monitor.DeepCopy()
	if !controllerutil.RemoveFinalizer(monitor, finalizerName) {
		return nil
	}
	// The optimistic lock makes a concurrent change to the finalizer list
	// surface as a conflict (and a retry) instead of being overwritten.
	return r.Patch(ctx, monitor, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

// credentials is what resolveCredentials found out about a monitor.
type credentials struct {
	// fromSecret is true when the credentials come from a referenced Secret,
	// which nothing but a periodic look tells the operator about.
	fromSecret bool
	// version changes whenever a Secret the pod reads changes: the
	// referenced one or the one generated from inline credentials, and
	// those it mounts certificates from.
	version string
	// reason and message of the CredentialsAvailable condition once
	// everything the pod takes from Secrets has been found. It is set in
	// one place, at the end: a condition that went to True for the
	// credentials and back to False for a certificate would have a new
	// transition time, and be written, at every look.
	reason, message string
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
//
// For inline credentials it does more than check: it writes the Secret the
// pod reads them from (builder.BuildCredentialsSecret), and ends like any
// other apply when that cannot be written: NameConflict for a Secret of that
// name that is not the monitor's, ApplyRejected or ApplyFailed otherwise.
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
	case builder.HasInlineCredentials(monitor):
		// The pod does not get them as plain values: see
		// builder.BuildCredentialsSecret. What the apply returns is the
		// Secret as stored, and its version moves when its content does.
		generated := builder.BuildCredentialsSecret(monitor)
		if stop := r.apply(ctx, monitor, generated); stop != nil {
			status.set(conditionCredentials, metav1.ConditionFalse, stop.reason, stop.message)
			return credentials{}, stop
		}
		return credentials{
			version: credentialsVersion(generated),
			reason:  "InlineCredentials",
			message: fmt.Sprintf("Inline username and password are set; the collector reads them from Secret %q",
				generated.Name),
		}, nil

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

		// The Secret the operator generated for this monitor, named as if
		// it were the user's. It is removed when a monitor has no inline
		// credentials, which is now: the pod would be left reading a Secret
		// that is gone. A Secret of that name that is not the monitor's is
		// the user's, and as good as any other.
		if ref.Name == builder.CredentialsSecretName(monitor.Name) && metav1.IsControlledBy(&secret, monitor) {
			stop := unavailable(reasonGeneratedSecretReferenced, fmt.Sprintf(
				"Secret %q is the one the operator generates from inline credentials, and removes once a "+
					"monitor has none. It cannot be the monitor's credentialsSecret: copy it under another name "+
					"and name that, or keep the inline credentials. Nothing is changed until then", ref.Name))
			// A mistake in the spec: looking again does not change it.
			stop.requeueAfter = 0
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

		return credentials{
			fromSecret: true,
			version:    credentialsVersion(&secret),
			reason:     "CredentialsFound",
			message:    fmt.Sprintf("Secret %q provides keys %q and %q", ref.Name, usernameKey, passwordKey),
		}, nil

	default:
		return credentials{}, unavailable(reasonNoCredentials,
			"spec.database has no inline username/password and no credentialsSecret")
	}
}

// resolveCertificates checks the Secrets the collector pod mounts
// certificates from (builder.MountedSecrets): the certificate authority of
// the database, a client certificate, the certificate authority of the gRPC
// sink. A pod that names a Secret or a key that is not there does not start,
// and one that is handed something that is no certificate fails later, where
// only its log says so; both are reported here instead, under the condition
// and the reasons of the credentials, of which these are a part.
//
// The pod reads the files once, when it starts. Their versions are folded
// into the token of the pod template, so that a renewed certificate replaces
// the pod, and the Secrets are looked at again like a credentials Secret.
// A monitor without certificates gets back what it came with.
func (r *PgpilotMonitorReconciler) resolveCertificates(
	ctx context.Context,
	status *monitorStatus,
	creds credentials,
) (credentials, *outcome) {
	monitor := status.monitor
	mounted := builder.MountedSecrets(monitor)
	if len(mounted) == 0 {
		return creds, nil
	}
	unavailable := func(reason, message string) *outcome {
		status.set(conditionCredentials, metav1.ConditionFalse, reason, message)
		return &outcome{reason: reason, message: message, requeueAfter: r.credentialsRetry()}
	}

	versions := make([]string, 0, len(mounted)+1)
	versions = append(versions, creds.version)
	names := make([]string, 0, len(mounted))
	for _, need := range mounted {
		// No name of a Secret is longer than 253 characters. The field of
		// the gRPC sink takes anything, a pasted certificate included, and
		// a message with all of it would be one the API server refuses.
		shown := need.Name
		if len(shown) > validation.DNS1123SubdomainMaxLength {
			shown = fmt.Sprintf("%s... (%d characters)", strings.ToValidUTF8(shown[:63], ""), len(shown))
		}
		what := fmt.Sprintf("Secret %q, named by %s,", shown, need.Field)
		// The name of the gRPC sink's authority is whatever was stored in
		// that field before it had a meaning. One with a "/" in it never
		// reaches the API server: the client refuses to ask, which would
		// look like a Secret that cannot be read, and be retried for ever.
		if problems := validation.IsDNS1123Subdomain(need.Name); len(problems) > 0 {
			stop := unavailable(reasonSecretNotFound, fmt.Sprintf(
				"%s cannot exist: that is not the name of a Secret (%s). The Secret has to be in the namespace "+
					"of the monitor, and is named without one", what, problems[0]))
			// A mistake in the spec: looking again does not change it.
			stop.requeueAfter = 0
			return creds, stop
		}
		var secret corev1.Secret
		err := r.Get(ctx, types.NamespacedName{Name: need.Name, Namespace: monitor.Namespace}, &secret)
		switch {
		case apierrors.IsNotFound(err):
			return creds, unavailable(reasonSecretNotFound,
				fmt.Sprintf("%s was not found in namespace %q", what, monitor.Namespace))
		case err != nil:
			stop := unavailable(reasonSecretReadFailed, fmt.Sprintf("%s could not be read: %v", what, err))
			stop.requeueAfter = 0
			stop.err = fmt.Errorf("read secret %q of %s: %w", need.Name, need.Field, err)
			return creds, stop
		}
		// The Secret the operator generates for inline credentials goes
		// when the monitor has none: a pod that mounts it would lose it.
		if need.Name == builder.CredentialsSecretName(monitor.Name) && metav1.IsControlledBy(&secret, monitor) {
			stop := unavailable(reasonGeneratedSecretReferenced, fmt.Sprintf(
				"%s is the one the operator generates from inline credentials, and removes once a monitor has "+
					"none. It cannot hold a certificate of the monitor: name a Secret of your own. Nothing is "+
					"changed until then", what))
			stop.requeueAfter = 0
			return creds, stop
		}
		for _, key := range need.Keys {
			if len(secret.Data[key]) == 0 {
				return creds, unavailable(reasonSecretKeyMissing, fmt.Sprintf("%s has no value under key %q", what, key))
			}
		}
		if err := need.CheckContent(secret.Data); err != nil {
			return creds, unavailable(reasonInvalidCertificate, fmt.Sprintf("%s cannot be used: %v", what, err))
		}
		versions = append(versions, credentialsVersion(&secret))
		if quoted := fmt.Sprintf("%q", need.Name); !slices.Contains(names, quoted) {
			names = append(names, quoted)
		}
	}

	// One token for everything the pod takes from Secrets. Each part is
	// already one that says nothing about what a Secret holds.
	sum := sha256.Sum256([]byte(strings.Join(versions, "/")))
	creds.fromSecret, creds.version = true, hex.EncodeToString(sum[:8])
	creds.message += "; certificates are mounted from " + strings.Join(names, ", ")
	return creds, nil
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

// unusableMetrics says which of the metric definitions a monitor is made of
// cannot be used, and where it is to be corrected: the first library in the
// order they are listed, then the monitor's own. It returns "" when all of
// them can be used.
func unusableMetrics(monitor *pgpilotv1.PgpilotMonitor, libraries []pgpilotv1.PgpilotMetricLibrary) string {
	const consequence = "Nothing is changed for this monitor until that is corrected; " +
		"a collector that runs keeps running as it was"
	for i := range libraries {
		if err := builder.ValidateMetricDefinitions(libraries[i].Spec.Metrics); err != nil {
			return fmt.Sprintf("PgpilotMetricLibrary %s cannot be used: %v. %s", libraries[i].Name, err, consequence)
		}
	}
	if err := builder.ValidateMetricDefinitions(monitor.Spec.Metrics.Custom); err != nil {
		return fmt.Sprintf("spec.metrics.custom cannot be used: %v. %s", err, consequence)
	}
	return ""
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
		// Nothing can be enqueued, and nothing retries this: say so. A
		// monitor that is looked at periodically finds the change then; one
		// that waits for this library to be corrected (InvalidMetric) does
		// not.
		logf.FromContext(ctx).Error(err, "a changed metric library could not be mapped to its monitors; "+
			"they pick the change up at their next reconcile, which for a monitor that is not looked at "+
			"periodically is the next change of the library or of the monitor",
			"library", lib.Name, "namespace", lib.Namespace)
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
