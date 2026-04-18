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
	// host of the PostgreSQL server (DNS name or IP).
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// port of the PostgreSQL server.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=5432
	// +optional
	Port int32 `json:"port,omitempty"`

	// database is the logical database name pgwatch connects to.
	// +kubebuilder:validation:MinLength=1
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
	// +optional
	UsernameKey string `json:"usernameKey,omitempty"`

	// passwordKey is the key inside the Secret containing the database
	// password. Defaults to "password".
	// +kubebuilder:default=password
	// +optional
	PasswordKey string `json:"passwordKey,omitempty"`
}

// MetricsSpec selects which metrics pgwatch collects. The effective
// metrics.yaml is the union of:
//
//   1. the chosen pgwatch built-in preset (if any),
//   2. all metrics from every referenced PgpilotMetricLibrary,
//   3. every inline custom metric.
//
// Later entries override earlier ones by metric name.
type MetricsSpec struct {
	// preset is the name of a pgwatch built-in preset (e.g. "basic",
	// "standard", "exhaustive", "recommendations", "rds", "aurora").
	// Leave empty to rely entirely on libraries and custom metrics.
	// +optional
	Preset string `json:"preset,omitempty"`

	// fromLibraries references PgpilotMetricLibrary CRs in the same
	// namespace whose metrics should be included.
	// +optional
	FromLibraries []LibraryRef `json:"fromLibraries,omitempty"`

	// custom is an inline list of metric definitions. Prefer libraries for
	// anything reused across monitors.
	// +optional
	Custom []MetricDefinition `json:"custom,omitempty"`
}

// LibraryRef points to a PgpilotMetricLibrary in the same namespace.
// Cross-namespace references are intentionally not supported in v1.0.0.
type LibraryRef struct {
	// name of the PgpilotMetricLibrary.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// SinksSpec configures where pgwatch ships measurements. At least one sink
// should be enabled — the operator rejects configurations where none are.
type SinksSpec struct {
	// prometheus configures the pull-based Prometheus sink (pgwatch
	// exposes /metrics on the pod).
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
type GRPCSink struct {
	// enabled toggles the sink.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled"`

	// endpoint is the host:port of the gRPC Receiver.
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

// ObjectMetadataOverrides holds labels and annotations the operator merges
// onto a generated resource. System-managed keys — those under
// `app.kubernetes.io/*` and `pgpilot.io/*` — always win and cannot be
// overridden by users.
type ObjectMetadataOverrides struct {
	// labels to add on top of system-managed labels.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// annotations to add on top of system-managed annotations.
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
	// passed its readiness probe.
	// +optional
	Ready bool `json:"ready,omitempty"`

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
	SchemeBuilder.Register(&PgpilotMonitor{}, &PgpilotMonitorList{})
}
