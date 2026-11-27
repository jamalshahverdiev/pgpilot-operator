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
	"fmt"
	"time"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/jamalshahverdiev/pgpilot-operator/internal/builder"
)

// Options are the settings of the controllers that come from the command line.
type Options struct {
	// ServiceMonitorEnabled is whether the prometheus-operator ServiceMonitor
	// API is served by the cluster.
	ServiceMonitorEnabled bool
	// CredentialsRecheck and CredentialsRetry are described on
	// PgpilotMonitorReconciler; zero means the default.
	CredentialsRecheck time.Duration
	CredentialsRetry   time.Duration

	// KeepStoredImageTag turns off StoredImageMigration: monitors keep the
	// pgwatch image tag an operator release up to v1.0.2 wrote into their
	// spec.
	KeepStoredImageTag bool
}

// MinCredentialsRecheck is the shortest recheck interval Validate accepts.
// Each recheck reads the Secret and re-applies the generated objects, so a
// very short interval turns into a busy loop for every monitor.
const MinCredentialsRecheck = 10 * time.Second

// Validate reports settings the controllers cannot sensibly run with.
func (o Options) Validate() error {
	// Zero is allowed and means the default; anything else has to be a
	// usable interval rather than silently becoming the default as well.
	if o.CredentialsRecheck != 0 && o.CredentialsRecheck < MinCredentialsRecheck {
		return fmt.Errorf("credentials recheck interval %s is too short: use at least %s, or 0 for the default of %s",
			o.CredentialsRecheck, MinCredentialsRecheck, defaultCredentialsRecheck)
	}
	if o.CredentialsRetry < 0 {
		return fmt.Errorf("credentials retry interval %s is negative", o.CredentialsRetry)
	}
	return nil
}

// ManagerOptions adds to base the cache and client settings a manager needs
// to run these controllers with the permissions the operator ships with.
//
// main and the test suite both build their manager through this function, so
// that what is tested is what runs: with a role that may neither list nor
// watch Secrets, a manager without these settings would hang on its first
// Secret.
func ManagerOptions(base ctrl.Options, opts Options) ctrl.Options {
	base.Cache = CacheOptions(opts.ServiceMonitorEnabled)
	base.Client = ClientOptions()
	return base
}

// SetupWithManager registers the controllers of this package with mgr.
func SetupWithManager(mgr ctrl.Manager, opts Options) error {
	// Every write names the operator as its author. Without this the API
	// server records them under the name of the binary, "manager", which
	// every kubebuilder-based controller shares.
	owned := client.WithFieldOwner(mgr.GetClient(), fieldOwner)

	if err := (&PgpilotMonitorReconciler{
		Client:                owned,
		APIReader:             mgr.GetAPIReader(),
		Scheme:                mgr.GetScheme(),
		Recorder:              mgr.GetEventRecorderFor("pgpilot-monitor"), //nolint:staticcheck // SA1019
		ServiceMonitorEnabled: opts.ServiceMonitorEnabled,
		CredentialsRecheck:    opts.CredentialsRecheck,
		CredentialsRetry:      opts.CredentialsRetry,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up the PgpilotMonitor controller: %w", err)
	}
	if err := (&PgpilotMetricLibraryReconciler{
		Client: owned,
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up the PgpilotMetricLibrary controller: %w", err)
	}
	if err := mgr.Add(migrations(mgr, mgr.GetClient(), opts)); err != nil {
		return fmt.Errorf("set up the migrations of stored monitors: %w", err)
	}
	return nil
}

// migrations returns what the manager runs when it starts leading to undo
// what earlier operator releases left on stored monitors.
func migrations(src migrationSource, c client.Client, opts Options) *legacyMigrations {
	return &legacyMigrations{
		image: storedImageMigration(src, c, opts),
		finalizers: &FinalizerRelease{
			Reader: src.GetAPIReader(),
			Client: client.WithFieldOwner(c, fieldOwner),
		},
	}
}

// migrationSource is the part of a manager the migration is built from.
type migrationSource interface {
	GetAPIReader() client.Reader
	GetEventRecorderFor(name string) record.EventRecorder
}

// storedImageMigration returns the image migration, or nil when opts turn it
// off. It reads through the manager's
// uncached reader, because the cache holds no managedFields, and writes
// through c under the operator's own name.
func storedImageMigration(src migrationSource, c client.Client, opts Options) *StoredImageMigration {
	if opts.KeepStoredImageTag {
		return nil
	}
	return &StoredImageMigration{
		Reader:   src.GetAPIReader(),
		Client:   client.WithFieldOwner(c, fieldOwner),
		Recorder: src.GetEventRecorderFor("pgpilot-monitor"), //nolint:staticcheck // SA1019
	}
}

// CacheOptions limits what the manager keeps in memory to the objects the
// operator creates itself.
//
// Without it every ConfigMap, Deployment, Service and Pod of the cluster is
// held in full: on a cluster of any size that is far more than the operator's
// own memory limit allows, for data it never looks at. The generated objects
// all carry the managed-by label, including the pods through their template.
//
// The same options have to be used wherever a manager for these controllers
// is created, which is why they live here and not in main.
func CacheOptions(serviceMonitorEnabled bool) cache.Options {
	managed := cache.ByObject{
		Label: labels.SelectorFromSet(labels.Set{builder.LabelAppManagedBy: builder.ManagedBy}),
	}
	byObject := map[client.Object]cache.ByObject{
		&corev1.ConfigMap{}:  managed,
		&appsv1.Deployment{}: managed,
		&corev1.Service{}:    managed,
		&corev1.Pod{}:        managed,
	}
	if serviceMonitorEnabled {
		byObject[&monitoringv1.ServiceMonitor{}] = managed
	}
	return cache.Options{
		// managedFields can be as large as the object itself and nothing
		// here reads them.
		DefaultTransform: cache.TransformStripManagedFields(),
		ByObject:         byObject,
	}
}

// ClientOptions makes the manager's client read Secrets straight from the API
// server, one at a time and by name.
//
// A cached read would start a watch on every Secret of the cluster and keep
// them all, with their data, in the operator's memory. The operator only ever
// needs the one Secret a monitor points at, and its role grants neither
// `list` nor `watch` on Secrets.
func ClientOptions() client.Options {
	return client.Options{
		Cache: &client.CacheOptions{
			DisableFor: []client.Object{&corev1.Secret{}},
		},
	}
}
