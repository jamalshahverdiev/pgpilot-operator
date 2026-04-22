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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

// PgpilotMetricLibraryReconciler reconciles a PgpilotMetricLibrary object.
// Its main purpose is to update the library's status. Dependent monitors
// are already enqueued by the PgpilotMonitor controller's Watches setup.
type PgpilotMetricLibraryReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=pgpilot.io,resources=pgpilotmetriclibraries,verbs=get;list;watch
// +kubebuilder:rbac:groups=pgpilot.io,resources=pgpilotmetriclibraries/status,verbs=get;update;patch

func (r *PgpilotMetricLibraryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var lib pgpilotv1.PgpilotMetricLibrary
	if err := r.Get(ctx, req.NamespacedName, &lib); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	valid := len(lib.Spec.Metrics) > 0
	status := metav1.ConditionTrue
	reason := "Valid"
	message := "Library contains metric definitions"
	if !valid {
		status = metav1.ConditionFalse
		reason = "Empty"
		message = "Library has no metric definitions"
	}

	meta.SetStatusCondition(&lib.Status.Conditions, metav1.Condition{
		Type:               "Valid",
		Status:             status,
		ObservedGeneration: lib.Generation,
		Reason:             reason,
		Message:            message,
	})
	lib.Status.ObservedGeneration = lib.Generation

	if err := r.Status().Update(ctx, &lib); err != nil {
		log.Error(err, "failed to update library status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PgpilotMetricLibraryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&pgpilotv1.PgpilotMetricLibrary{}).
		Named("pgpilotmetriclibrary").
		Complete(r)
}
