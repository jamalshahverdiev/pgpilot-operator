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
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/discovery"
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

	// CollectorRunAsUser is the user the pgwatch container runs as; see
	// ParseCollectorRunAsUser. The zero value is the default user.
	CollectorRunAsUser CollectorUser
}

// CollectorUser says which user the pgwatch container of every collector
// runs as. The zero value is the operator's default, builder.DefaultRunAsUser.
type CollectorUser struct {
	// uid is the user, when set; zero means the default.
	uid int64
	// platform leaves the user to the cluster: the pod names none.
	platform bool
	// exceptUnassigned names the default user after all in the namespaces
	// where OpenShift assigns none; see unassignedNamespaces.
	exceptUnassigned bool
}

// unassignedNamespaces are the namespaces in which OpenShift does not run
// the admission of security context constraints (its "run level" 0 and 1
// namespaces, by name). Nothing assigns a user to a pod there, and nothing
// refuses one that names its own: a collector that names none would not
// start, because the pgwatch image runs as root unless told otherwise.
//
// A namespace an administrator exempted with the label
// openshift.io/run-level is not known here; for such a cluster the user has
// to be named on the command line.
var unassignedNamespaces = map[string]bool{
	"default": true, "kube-system": true, "kube-public": true,
	"openshift": true, "openshift-infra": true, "openshift-node": true,
}

// RunAsUserIn is what goes into the security context of the container of a
// collector in the given namespace: a user, or nil for none.
func (u CollectorUser) RunAsUserIn(namespace string) *int64 {
	switch {
	case u.platform && (!u.exceptUnassigned || !unassignedNamespaces[namespace]):
		return nil
	case u.uid != 0:
		return &u.uid
	}
	uid := builder.DefaultRunAsUser
	return &uid
}

func (u CollectorUser) String() string {
	switch {
	case u.platform && u.exceptUnassigned:
		return fmt.Sprintf("assigned by the platform (%d in the namespaces where OpenShift assigns none)",
			builder.DefaultRunAsUser)
	case u.platform:
		return "assigned by the platform"
	}
	return strconv.FormatInt(*u.RunAsUserIn(""), 10)
}

// Values of the --collector-run-as-user flag other than a number.
const (
	CollectorUserAuto = "auto"
	CollectorUserNone = "none"
)

// ParseCollectorRunAsUser reads the --collector-run-as-user flag.
//
//   - a number: every collector runs as that user. Not 0: the pod is told to
//     refuse to run as root.
//   - "none": the pod names no user and the platform has to. That is what
//     OpenShift's restricted security context constraints want: they assign
//     a user from the range of the namespace and refuse any other. On a
//     cluster where nothing assigns one the pod does not start, because the
//     pgwatch image runs as root unless told otherwise.
//   - "auto" or empty: the default user, except on OpenShift, where the pod
//     names none in every namespace but those OpenShift assigns no user in.
func ParseCollectorRunAsUser(value string, openShift bool) (CollectorUser, error) {
	switch value {
	case "", CollectorUserAuto:
		return CollectorUser{platform: openShift, exceptUnassigned: openShift}, nil
	case CollectorUserNone:
		return CollectorUser{platform: true}, nil
	}
	uid, err := strconv.ParseInt(value, 10, 64)
	switch {
	case err != nil:
		return CollectorUser{}, fmt.Errorf("collector user %q is neither a user ID nor %q or %q",
			value, CollectorUserAuto, CollectorUserNone)
	case uid == 0:
		return CollectorUser{}, errors.New("collector user 0 is root, and the collector pod refuses to run as root")
	case uid < 0 || uid > math.MaxInt32:
		// What the API server takes for runAsUser; a larger one would be
		// accepted here and refused with every Deployment.
		return CollectorUser{}, fmt.Errorf("collector user %d is not a user ID: use one from 1 to %d", uid, math.MaxInt32)
	}
	return CollectorUser{uid: uid}, nil
}

// ResolveCollectorUser turns the --collector-run-as-user flag into the user
// of the collectors, asking the cluster what it is only where that decides
// something: with a user, or none, named on the command line it does not
// matter, and an API server that does not answer must not keep the operator
// from starting.
func ResolveCollectorUser(value string, dc discovery.DiscoveryInterface) (CollectorUser, error) {
	if value != "" && value != CollectorUserAuto {
		return ParseCollectorRunAsUser(value, false)
	}
	openShift, err := builder.IsOpenShift(dc)
	if err != nil {
		return CollectorUser{}, fmt.Errorf("find out whether this is OpenShift (name the user of the collector "+
			"with --collector-run-as-user to go on without asking): %w", err)
	}
	return ParseCollectorRunAsUser(value, openShift)
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

// monitorReconciler returns a reconciler of monitors with the settings of
// the command line, still without the clients a manager gives it.
func (o Options) monitorReconciler() *PgpilotMonitorReconciler {
	return &PgpilotMonitorReconciler{
		ServiceMonitorEnabled: o.ServiceMonitorEnabled,
		CredentialsRecheck:    o.CredentialsRecheck,
		CredentialsRetry:      o.CredentialsRetry,
		CollectorRunAsUser:    o.CollectorRunAsUser,
	}
}

// SetupWithManager registers the controllers of this package with mgr.
func SetupWithManager(mgr ctrl.Manager, opts Options) error {
	// Every write names the operator as its author. Without this the API
	// server records them under the name of the binary, "manager", which
	// every kubebuilder-based controller shares.
	owned := client.WithFieldOwner(mgr.GetClient(), fieldOwner)

	monitors := opts.monitorReconciler()
	monitors.Client = owned
	monitors.APIReader = mgr.GetAPIReader()
	monitors.Scheme = mgr.GetScheme()
	monitors.Recorder = mgr.GetEventRecorderFor("pgpilot-monitor") //nolint:staticcheck // SA1019
	if err := monitors.SetupWithManager(mgr); err != nil {
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
