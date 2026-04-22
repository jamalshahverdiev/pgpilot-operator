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
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

const (
	finalizerName = "pgpilot.io/finalizer"
)

// PgpilotMonitorReconciler reconciles a PgpilotMonitor object.
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

type PgpilotMonitorReconciler struct {
	client.Client
	Scheme                *runtime.Scheme
	Recorder              record.EventRecorder
	ServiceMonitorEnabled bool
}

// +kubebuilder:rbac:groups=pgpilot.io,resources=pgpilotmonitors,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=pgpilot.io,resources=pgpilotmonitors/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pgpilot.io,resources=pgpilotmonitors/finalizers,verbs=update
// +kubebuilder:rbac:groups=pgpilot.io,resources=pgpilotmetriclibraries,verbs=get;list;watch
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
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
		if controllerutil.ContainsFinalizer(&monitor, finalizerName) {
			controllerutil.RemoveFinalizer(&monitor, finalizerName)
			if err := r.Update(ctx, &monitor); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is set.
	if !controllerutil.ContainsFinalizer(&monitor, finalizerName) {
		controllerutil.AddFinalizer(&monitor, finalizerName)
		if err := r.Update(ctx, &monitor); err != nil {
			return ctrl.Result{}, err
		}
	}

	// --- Step 1: Validate credentials source. ---
	if result, ok := r.validateCredentials(ctx, &monitor); !ok {
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
	desiredDep := builder.BuildDeployment(&monitor, merged, configHash)
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
	return ctrl.Result{}, nil
}

// validateCredentials checks that PgpilotMonitor has a usable credentials
// source — either inline username+password or a referenced Secret that
// exists in the same namespace. CRD CEL validation already enforces
// "exactly one of the two is set", so the `default` branch is defensive.
// Returns ok=true to tell the caller "keep reconciling"; ok=false means
// this reconcile loop should return early with the returned Result.
func (r *PgpilotMonitorReconciler) validateCredentials(
	ctx context.Context,
	monitor *pgpilotv1.PgpilotMonitor,
) (ctrl.Result, bool) {
	log := logf.FromContext(ctx)
	db := monitor.Spec.Database

	switch {
	case db.Username != "" && db.Password != "":
		r.setCondition(monitor, "DatabaseReachable", metav1.ConditionTrue, "InlineCredentials",
			"Inline username/password configured")
		return ctrl.Result{}, true

	case db.CredentialsSecret != nil:
		var secret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Name: db.CredentialsSecret.Name, Namespace: monitor.Namespace}, &secret); err != nil {
			log.Error(err, "credentials secret not found", "secret", db.CredentialsSecret.Name)
			r.setCondition(monitor, "Ready", metav1.ConditionFalse, "SecretNotFound",
				fmt.Sprintf("Secret %q not found: %v", db.CredentialsSecret.Name, err))
			r.setCondition(monitor, "DatabaseReachable", metav1.ConditionFalse, "SecretNotFound",
				fmt.Sprintf("Credentials secret %q not found", db.CredentialsSecret.Name))
			r.Recorder.Eventf(monitor, corev1.EventTypeWarning, "SecretNotFound",
				"Credentials secret %q not found in namespace %q", db.CredentialsSecret.Name, monitor.Namespace)
			if statusErr := r.patchStatus(ctx, monitor); statusErr != nil {
				log.Error(statusErr, "failed to update status")
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, false
		}
		r.setCondition(monitor, "DatabaseReachable", metav1.ConditionTrue, "CredentialsFound",
			fmt.Sprintf("Credentials secret %q exists", db.CredentialsSecret.Name))
		return ctrl.Result{}, true

	default:
		r.setCondition(monitor, "Ready", metav1.ConditionFalse, "NoCredentials",
			"spec.database has no inline username/password and no credentialsSecret")
		if statusErr := r.patchStatus(ctx, monitor); statusErr != nil {
			log.Error(statusErr, "failed to update status")
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, false
	}
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

// SetupWithManager sets up the controller with the Manager.
func (r *PgpilotMonitorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&pgpilotv1.PgpilotMonitor{}).
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
					NamespacedName: types.NamespacedName{
						Name:      m.Name,
						Namespace: m.Namespace,
					},
				})
				break
			}
		}
	}
	return requests
}
