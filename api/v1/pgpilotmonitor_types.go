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

package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// SSLMode describes the TLS policy pgwatch uses when connecting to the target
// database. Values mirror libpq's sslmode.
// +kubebuilder:validation:Enum=disable;allow;prefer;require;verify-ca;verify-full
type SSLMode string

const (
	SSLModeDisable    SSLMode = "disable"
	SSLModeAllow      SSLMode = "allow"
	SSLModePrefer     SSLMode = "prefer"
	SSLModeRequire    SSLMode = "require"
	SSLModeVerifyCA   SSLMode = "verify-ca"
	SSLModeVerifyFull SSLMode = "verify-full"
)

// DatabaseRef describes how pgwatch should reach the monitored PostgreSQL.
//
// Credentials can be supplied in two ways:
//
//  1. Inline (for dev/test): set `username` and `password` directly. Values
//     are stored in etcd in clear text — avoid in production.
//  2. Secret-based (recommended): set `credentialsSecret` pointing at a
//     Secret in the same namespace. The operator only references the
//     Secret; it does not copy its contents.
//
// Exactly one of the two methods must be provided.
// +kubebuilder:validation:XValidation:rule="(has(self.username) && has(self.password)) != has(self.credentialsSecret)",message="set either inline username+password or credentialsSecret, not both (and not neither)"
// +kubebuilder:validation:XValidation:rule="!has(self.username) || has(self.password)",message="password is required when username is set inline"
// +kubebuilder:validation:XValidation:rule="!has(self.password) || has(self.username)",message="username is required when password is set inline"
type DatabaseRef struct {
	// host of the PostgreSQL server: a DNS name, an IPv4 address or an IPv6
	// address (with or without square brackets). The port goes into `port`,
	// not here, and characters that have a meaning in a connection URL are
	// not accepted.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^([A-Za-z0-9._-]+|\[?[0-9A-Fa-f]*:[0-9A-Fa-f:.]*:[0-9A-Fa-f:.]*\]?)$`
	Host string `json:"host"`

	// port of the PostgreSQL server.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=5432
	// +optional
	Port int32 `json:"port,omitempty"`

	// database is the logical database name pgwatch connects to.
	// PostgreSQL limits identifiers to 63 bytes.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Database string `json:"database"`

	// sslmode selects TLS behaviour (libpq sslmode semantics).
	// +kubebuilder:default=disable
	// +optional
	SSLMode SSLMode `json:"sslmode,omitempty"`

	// username is the database user. Prefer credentialsSecret for production
	// — values set here are stored in etcd in clear text.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Username string `json:"username,omitempty"`

	// password is the database password. See the note on `username` — only
	// suitable for dev/test.
	// +kubebuilder:validation:MinLength=1
	// +optional
	Password string `json:"password,omitempty"`

	// credentialsSecret references a Secret in the same namespace that
	// holds the connection credentials. Preferred over inline username/
	// password.
	// +optional
	CredentialsSecret *CredentialsSecretRef `json:"credentialsSecret,omitempty"`

	// customTags are added to every measurement as additional labels.
	// Useful for `env`, `team`, `tier`, etc. Passed through to pgwatch
	// source configuration verbatim.
	//
	// Keys become label names. On the Prometheus sink a character outside
	// `[a-zA-Z0-9_]` is exported as `_` (`team-name` becomes `team_name`),
	// so prefer plain label names. Three kinds of key cannot work and are
	// rejected: an empty key, a key starting with `__` (reserved by
	// Prometheus; pgwatch then fails to export every series of the
	// database) and `dbname`, which pgwatch sets to the monitor name.
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(k) > 0 && !k.startsWith('__') && k != 'dbname')",message="customTags keys must not be empty, must not start with __ and must not be dbname"
	// +optional
	CustomTags map[string]string `json:"customTags,omitempty"`
}

// CredentialsSecretRef points at a Secret in the same namespace as the
// PgpilotMonitor. Only the keys are read — the operator does not copy the
// Secret anywhere else.
type CredentialsSecretRef struct {
	// name of the Secret.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// usernameKey is the key inside the Secret containing the database
	// username. Defaults to "username".
	// +kubebuilder:default=username
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]*$`
	// +optional
	UsernameKey string `json:"usernameKey,omitempty"`

	// passwordKey is the key inside the Secret containing the database
	// password. Defaults to "password".
	// +kubebuilder:default=password
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]*$`
	// +optional
	PasswordKey string `json:"passwordKey,omitempty"`
}

// MetricsSpec selects which metrics pgwatch collects. The effective
// metrics.yaml is the union of:
//
//  1. the chosen pgwatch built-in preset (if any),
//  2. all metrics from every referenced PgpilotMetricLibrary,
//  3. every inline custom metric.
//
// Later entries override earlier ones by metric name. At least one of the
// three has to be set: a monitor without metrics collects nothing.
// +kubebuilder:validation:XValidation:rule="(has(self.preset) && size(self.preset) > 0) || (has(self.fromLibraries) && size(self.fromLibraries) > 0) || (has(self.custom) && size(self.custom) > 0)",message="at least one of preset, fromLibraries or custom must be set"
type MetricsSpec struct {
	// preset is the name of a pgwatch built-in preset (e.g. "basic",
	// "standard", "exhaustive", "recommendations", "rds", "aurora").
	// Leave empty to rely entirely on libraries and custom metrics.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^([a-z][a-z0-9_]*)?$`
	// +optional
	Preset string `json:"preset,omitempty"`

	// fromLibraries references PgpilotMetricLibrary CRs in the same
	// namespace whose metrics should be included. A library can be listed
	// only once.
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:XValidation:rule="self.all(x, self.exists_one(y, y.name == x.name))",message="a library can be listed only once"
	// +listType=atomic
	// +optional
	FromLibraries []LibraryRef `json:"fromLibraries,omitempty"`

	// custom is an inline list of metric definitions. Prefer libraries for
	// anything reused across monitors. Metric names must be unique within
	// the list.
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:XValidation:rule="self.all(x, self.exists_one(y, y.name == x.name))",message="metric names must be unique"
	// +listType=atomic
	// +optional
	Custom []MetricDefinition `json:"custom,omitempty"`
}

// LibraryRef points to a PgpilotMetricLibrary in the same namespace.
// Cross-namespace references are intentionally not supported in v1.0.0.
type LibraryRef struct {
	// name of the PgpilotMetricLibrary.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// SinksSpec configures where pgwatch ships measurements. At least one sink
// has to be enabled: pgwatch refuses to start without one.
// +kubebuilder:validation:XValidation:rule="(has(self.prometheus) && self.prometheus.enabled) || (has(self.grpc) && self.grpc.enabled)",message="at least one sink must be enabled"
type SinksSpec struct {
	// prometheus configures the pull-based Prometheus sink (pgwatch
	// exposes /metrics on the pod). It is enabled when the field is left
	// out; set `enabled: false` to run with the gRPC sink only.
	// +kubebuilder:default={enabled:true}
	// +optional
	Prometheus *PrometheusSink `json:"prometheus,omitempty"`

	// grpc configures the push-based gRPC sink. pgpilot-operator does
	// not ship a receiver — this is a pass-through for users that run
	// their own, or for future pgpilot action-operators.
	// +optional
	GRPC *GRPCSink `json:"grpc,omitempty"`
}

// PrometheusSink exposes pgwatch measurements as a /metrics endpoint on the
// pod. Scrape it with Prometheus, vmagent, or any compatible agent.
type PrometheusSink struct {
	// enabled toggles the sink.
	// +kubebuilder:default=true
	Enabled bool `json:"enabled"`

	// port the pgwatch process listens on for /metrics.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=9187
	// +optional
	Port int32 `json:"port,omitempty"`
}

// GRPCSink streams every measurement to a remote Receiver implementing the
// pgwatch gRPC protocol.
// +kubebuilder:validation:XValidation:rule="!self.enabled || (has(self.endpoint) && size(self.endpoint) > 0)",message="endpoint is required when the gRPC sink is enabled"
type GRPCSink struct {
	// enabled toggles the sink.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled"`

	// endpoint is the host:port of the gRPC Receiver. Required when the
	// sink is enabled.
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern=`^\S*$`
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// tls configures optional TLS for the gRPC connection. When nil, the
	// connection is plaintext.
	// +optional
	TLS *GRPCTLS `json:"tls,omitempty"`
}

// GRPCTLS holds TLS material for the gRPC sink.
type GRPCTLS struct {
	// caSecretRef is the name of a Secret in the same namespace that
	// contains the CA certificate under the key "ca.crt".
	// +optional
	CASecretRef string `json:"caSecretRef,omitempty"`
}

// ImageSpec overrides the pgwatch container image. Defaults are chosen so
// that most users can leave this unset entirely.
type ImageSpec struct {
	// repository of the pgwatch image.
	// +kubebuilder:default="cybertecpostgresql/pgwatch"
	// +optional
	Repository string `json:"repository,omitempty"`

	// tag of the pgwatch image.
	// +kubebuilder:default="5.1.0"
	// +optional
	Tag string `json:"tag,omitempty"`

	// pullPolicy for the image.
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	// +kubebuilder:default=IfNotPresent
	// +optional
	PullPolicy corev1.PullPolicy `json:"pullPolicy,omitempty"`
}

// LabelValue is the value of a Kubernetes label: empty, or at most 63
// alphanumeric characters, '-', '_' or '.', starting and ending with an
// alphanumeric character.
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:Pattern=`^([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?$`
type LabelValue string

// LabelMap returns the labels as the plain string map Kubernetes objects use.
// It returns nil when no labels are set.
func (o ObjectMetadataOverrides) LabelMap() map[string]string {
	if o.Labels == nil {
		return nil
	}
	out := make(map[string]string, len(o.Labels))
	for k, v := range o.Labels {
		out[k] = string(v)
	}
	return out
}

// ObjectMetadataOverrides holds labels and annotations the operator merges
// onto a generated resource.
//
// The labels and annotations the operator sets itself always win over a
// user-supplied entry with the same key. Keys under `pgpilot.io/` are
// reserved for the operator and are rejected.
type ObjectMetadataOverrides struct {
	// labels to add on top of system-managed labels. Keys and values must
	// be valid Kubernetes label keys and values.
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^([a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$') && k.indexOf('/') <= 253)",message="label keys must be valid Kubernetes label keys: an optional lower-case DNS subdomain prefix and '/', then at most 63 alphanumeric characters, '-', '_' or '.', starting and ending with an alphanumeric character"
	// +kubebuilder:validation:XValidation:rule="!self.exists(k, k.startsWith('pgpilot.io/'))",message="label keys under pgpilot.io/ are reserved for the operator"
	// +optional
	Labels map[string]LabelValue `json:"labels,omitempty"`

	// annotations to add on top of system-managed annotations. Keys must be
	// valid Kubernetes annotation keys.
	// +kubebuilder:validation:MaxProperties=64
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^([A-Za-z0-9]([-A-Za-z0-9]*[A-Za-z0-9])?([.][A-Za-z0-9]([-A-Za-z0-9]*[A-Za-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$') && k.indexOf('/') <= 253)",message="annotation keys must be valid Kubernetes annotation keys: an optional DNS subdomain prefix and '/', then at most 63 alphanumeric characters, '-', '_' or '.', starting and ending with an alphanumeric character"
	// +kubebuilder:validation:XValidation:rule="!self.exists(k, k.startsWith('pgpilot.io/'))",message="annotation keys under pgpilot.io/ are reserved for the operator"
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// PgpilotMonitorSpec defines the desired state of a PgpilotMonitor.
// One PgpilotMonitor produces exactly one pgwatch Deployment (1 replica)
// plus its ConfigMap and Service.
type PgpilotMonitorSpec struct {
	// database is the PostgreSQL target pgwatch will monitor.
	Database DatabaseRef `json:"database"`

	// metrics selects what pgwatch collects.
	Metrics MetricsSpec `json:"metrics"`

	// sinks configures where measurements go. At least one sink must be
	// enabled.
	Sinks SinksSpec `json:"sinks"`

	// resources overrides requests/limits on the pgwatch container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// image overrides the pgwatch image coordinates.
	// +optional
	Image ImageSpec `json:"image,omitempty"`

	// podMetadata extends labels and annotations on the generated Pod
	// template. Used for vmagent / Prometheus annotation-based discovery
	// (e.g. prometheus.io/scrape), team ownership, etc.
	// +optional
	PodMetadata ObjectMetadataOverrides `json:"podMetadata,omitempty"`

	// serviceMetadata extends labels and annotations on the generated
	// Service. Used for label-selector-based scrape configs and for
	// ServiceMonitor selection.
	// +optional
	ServiceMetadata ObjectMetadataOverrides `json:"serviceMetadata,omitempty"`
}

// PgpilotMonitorStatus is the observed state of a PgpilotMonitor.
type PgpilotMonitorStatus struct {
	// ready is true when the underlying pgwatch Pod is Running and has
	// passed its readiness probe. It is always written, so `false` shows
	// up in `kubectl get` instead of an empty column.
	// +optional
	Ready bool `json:"ready"`

	// podName is the name of the currently-running pgwatch Pod, if any.
	// +optional
	PodName string `json:"podName,omitempty"`

	// observedGeneration is the .metadata.generation the controller last
	// reconciled. Comparing this with metadata.generation lets tools
	// detect in-flight updates.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// configHash is the SHA-256 digest of the rendered sources.yaml +
	// metrics.yaml payload. A change here means the underlying pgwatch
	// Deployment has been (or will be) rolled.
	// +optional
	ConfigHash string `json:"configHash,omitempty"`

	// lastReconciled is the timestamp of the last successful reconcile.
	// +optional
	LastReconciled *metav1.Time `json:"lastReconciled,omitempty"`

	// conditions reflects the current state of the PgpilotMonitor.
	// Typical types:
	//   - "Ready"             — Pod is up and scraping
	//   - "ConfigGenerated"   — ConfigMap matches spec
	//   - "DatabaseReachable" — pgwatch can connect to the target DB
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=pm
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="boolean",JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="Pod",type="string",JSONPath=".status.podName",priority=1
// +kubebuilder:printcolumn:name="Host",type="string",JSONPath=".spec.database.host",priority=1
// +kubebuilder:printcolumn:name="DB",type="string",JSONPath=".spec.database.database",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// PgpilotMonitor declares that pgpilot-operator should run a pgwatch
// collector against the described PostgreSQL database and expose its
// metrics via the configured sinks. It is namespace-scoped and users may
// create any number of them in any namespace.
//
// The name is used to derive the names of the generated Service, Deployment
// and ConfigMap ("pgpilot-<name>") and as a label value. Keep it to at most
// 55 characters without dots; with a longer or dotted name the Service cannot
// be created.
//
// This is deliberately not a validation rule on the object: a rule on
// metadata.name is evaluated on every update, the name can never be changed,
// and so a monitor that already exists with such a name could no longer be
// updated at all - not even to remove its finalizer when it is deleted.
type PgpilotMonitor struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of PgpilotMonitor
	// +required
	Spec PgpilotMonitorSpec `json:"spec"`

	// status defines the observed state of PgpilotMonitor
	// +optional
	Status PgpilotMonitorStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PgpilotMonitorList contains a list of PgpilotMonitor.
type PgpilotMonitorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []PgpilotMonitor `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &PgpilotMonitor{}, &PgpilotMonitorList{})
		return nil
	})
}
