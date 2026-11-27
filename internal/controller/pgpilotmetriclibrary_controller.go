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

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

const (
	// conditionLibraryValid reports whether the definitions of a library can
	// be handed to a collector.
	conditionLibraryValid = "Valid"

	libraryReasonValid = "Valid"
	// libraryReasonEmpty: a library without a single definition. The CRD
	// refuses that today; a library stored before it did can still be one.
	libraryReasonEmpty = "Empty"
)

// PgpilotMetricLibraryReconciler reports on a PgpilotMetricLibrary in its
// status: whether its definitions can be used, and how many there are. It
// changes nothing else. The monitors that use a library are brought up to
// date by the PgpilotMonitor controller, which watches libraries itself.
type PgpilotMetricLibraryReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=pgpilot.io,resources=pgpilotmetriclibraries,verbs=get;list;watch
// +kubebuilder:rbac:groups=pgpilot.io,resources=pgpilotmetriclibraries/status,verbs=get;update;patch

func (r *PgpilotMetricLibraryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var lib pgpilotv1.PgpilotMetricLibrary
	if err := r.Get(ctx, req.NamespacedName, &lib); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	stored := lib.DeepCopy()

	valid := libraryValidity(&lib)
	valid.ObservedGeneration = lib.Generation
	meta.SetStatusCondition(&lib.Status.Conditions, valid)
	lib.Status.ObservedGeneration = lib.Generation
	lib.Status.MetricCount = int32(len(lib.Spec.Metrics)) //nolint:gosec // no stored object holds 2^31 entries

	if apiequality.Semantic.DeepEqual(stored.Status, lib.Status) {
		return ctrl.Result{}, nil
	}
	// A patch of the status alone, against the version it was worked out
	// from. A full update would send the spec back as the Go struct
	// serialises it, and a status worked out from a copy the cache had not
	// caught up on would replace a newer one.
	patch := client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{})
	switch err := r.Status().Patch(ctx, &lib, patch); {
	case apierrors.IsNotFound(err):
		return ctrl.Result{}, nil
	case apierrors.IsConflict(err):
		// The library changed since it was read. Looked at again in a
		// moment, as it is then; no failure to count or to log.
		return ctrl.Result{RequeueAfter: staleReadRetry}, nil
	case err != nil:
		// This includes the status of a library that breaks a rule, which
		// the API server refuses as invalid before Kubernetes 1.33. The
		// error in the log is the one trace of that on this side; the
		// monitors that use the library say what is wrong with it.
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}
	return ctrl.Result{}, nil
}

// libraryValidity works out the Valid condition of a library, by the rules
// the monitor reconciler applies before it renders the definitions.
func libraryValidity(lib *pgpilotv1.PgpilotMetricLibrary) metav1.Condition {
	cond := metav1.Condition{Type: conditionLibraryValid, Status: metav1.ConditionFalse}
	switch err := builder.ValidateMetricDefinitions(lib.Spec.Metrics); {
	case len(lib.Spec.Metrics) == 0:
		cond.Reason = libraryReasonEmpty
		cond.Message = "The library has no metric definitions"
	case err != nil:
		cond.Reason = reasonInvalidMetric
		cond.Message = fmt.Sprintf("%v. Monitors that use this library report %s and are not changed until it is corrected",
			err, reasonInvalidMetric)
	default:
		cond.Status = metav1.ConditionTrue
		cond.Reason = libraryReasonValid
		cond.Message = fmt.Sprintf("Metric definitions that can be used: %d", len(lib.Spec.Metrics))
	}
	return cond
}

// SetupWithManager sets up the controller with the Manager.
func (r *PgpilotMetricLibraryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&pgpilotv1.PgpilotMetricLibrary{}).
		Named("pgpilotmetriclibrary").
		Complete(r)
}
