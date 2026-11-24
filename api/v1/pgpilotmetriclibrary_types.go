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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// MetricDefinition describes one pgwatch SQL metric. It is the unit shared
// between PgpilotMetricLibrary (reusable sets) and PgpilotMonitor.spec.metrics.custom
// (inline one-offs). The operator merges these with the pgwatch built-in
// preset selected by the user and materialises a metrics.yaml for pgwatch.
// +kubebuilder:validation:XValidation:rule="!(has(self.masterOnly) && self.masterOnly && has(self.standbyOnly) && self.standbyOnly)",message="masterOnly and standbyOnly are mutually exclusive"
type MetricDefinition struct {
	// name is the pgwatch metric name. Used as the key in metrics.yaml and
	// as the series name when shipped to Prometheus / gRPC sinks.
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// description is a free-form human-readable hint. Not used by pgwatch.
	// +optional
	Description string `json:"description,omitempty"`

	// interval is the collection period as a Go duration (e.g. "30s", "5m",
	// "1h"). pgwatch schedules in whole seconds, so fractions are truncated;
	// the minimum is 1s. If omitted, the operator uses 60s.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s')",message="interval must be a duration of at least 1s, e.g. 30s, 5m or 1h"
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// sqls maps a PostgreSQL major version to the SQL statement pgwatch
	// executes from that version on. For a given server pgwatch picks the
	// entry with the highest key that is not above the server's major
	// version, so "14" covers 14 and everything newer until a higher key
	// is defined, and "0" covers every version. A server older than the
	// lowest key gets no data for the metric.
	//
	// Keys are major versions written as plain integers in strings ("0",
	// "14"), not server_version_num values such as 170000, and there is no
	// wildcard key. Statements must not be empty.
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^(0|[1-9][0-9]{0,2})$'))",message="sqls keys must be PostgreSQL major versions written as plain integers, e.g. \"14\" (\"0\" matches every version)"
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(self[k]) > 0)",message="sqls statements must not be empty"
	SQLs map[string]string `json:"sqls"`

	// gauges lists the result columns treated as gauges (vs counters).
	// Use ["*"] to mark every non-tag column as a gauge — this matches
	// how pgwatch built-in metrics are written.
	// +optional
	Gauges []string `json:"gauges,omitempty"`

	// statementTimeoutSeconds is accepted for compatibility and ignored.
	//
	// Deprecated: pgwatch has no configurable per-metric timeout, so the
	// value was never applied. From pgwatch 6 on the collector cancels
	// metric queries on its own: those due together after 30 seconds, and
	// one retried alone after its interval (at least 30 seconds). For a
	// limit of your choosing set it on the database side, e.g.
	// `ALTER ROLE <monitoring role> SET statement_timeout = '5s'`.
	// +kubebuilder:validation:Minimum=0
	// +optional
	StatementTimeoutSeconds int32 `json:"statementTimeoutSeconds,omitempty"`

	// masterOnly restricts collection to primary PostgreSQL instances.
	// Cannot be combined with standbyOnly.
	// +optional
	MasterOnly bool `json:"masterOnly,omitempty"`

	// standbyOnly restricts collection to standby (replica) instances.
	// Cannot be combined with masterOnly.
	// +optional
	StandbyOnly bool `json:"standbyOnly,omitempty"`

	// isInstanceLevel marks the metric as instance-wide (not database-scoped).
	// +optional
	IsInstanceLevel bool `json:"isInstanceLevel,omitempty"`
}

// PgpilotMetricLibrarySpec defines a reusable set of SQL metric definitions
// that PgpilotMonitor instances in the same namespace can reference.
type PgpilotMetricLibrarySpec struct {
	// metrics is the list of metric definitions provided by this library.
	// Metric names must be unique within the list.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:XValidation:rule="self.all(x, self.exists_one(y, y.name == x.name))",message="metric names must be unique"
	// +listType=atomic
	Metrics []MetricDefinition `json:"metrics"`
}

// PgpilotMetricLibraryStatus is the observed state of PgpilotMetricLibrary.
type PgpilotMetricLibraryStatus struct {
	// observedGeneration is the .metadata.generation the controller last
	// reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// metricCount is the number of metric definitions in the library as
	// of observedGeneration.
	// +optional
	MetricCount int32 `json:"metricCount,omitempty"`

	// conditions reflects the current state of the library.
	// Typical types: "Valid" (syntactic validation of SQL/markers passes).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=pml
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Metrics",type="integer",JSONPath=".status.metricCount",description="Number of metric definitions",priority=0
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// PgpilotMetricLibrary is a namespace-scoped, reusable collection of
// pgwatch-compatible metric definitions. A PgpilotMonitor references one
// or more libraries to compose its effective metrics.yaml without having
// to inline every custom SQL.
type PgpilotMetricLibrary struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of PgpilotMetricLibrary
	// +required
	Spec PgpilotMetricLibrarySpec `json:"spec"`

	// status defines the observed state of PgpilotMetricLibrary
	// +optional
	Status PgpilotMetricLibraryStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// PgpilotMetricLibraryList contains a list of PgpilotMetricLibrary.
type PgpilotMetricLibraryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []PgpilotMetricLibrary `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &PgpilotMetricLibrary{}, &PgpilotMetricLibraryList{})
		return nil
	})
}
