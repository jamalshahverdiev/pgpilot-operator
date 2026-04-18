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
)

// MetricDefinition describes one pgwatch SQL metric. It is the unit shared
// between PgpilotMetricLibrary (reusable sets) and PgpilotMonitor.spec.metrics.custom
// (inline one-offs). The operator merges these with the pgwatch built-in
// preset selected by the user and materialises a metrics.yaml for pgwatch.
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

	// interval is the collection period (e.g. "60s", "5m"). If omitted,
	// pgwatch defaults apply.
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// sqls maps PostgreSQL major version (as a string, e.g. "13", "14")
	// to the SQL statement pgwatch will execute on that version. The key
	// "*" (or the lowest-version entry) acts as a fallback — exact semantics
	// match pgwatch's own metrics.yaml.
	// +kubebuilder:validation:MinProperties=1
	SQLs map[string]string `json:"sqls"`

	// gauges lists the result columns treated as gauges (vs counters).
	// Use ["*"] to mark every non-tag column as a gauge — this matches
	// how pgwatch built-in metrics are written.
	// +optional
	Gauges []string `json:"gauges,omitempty"`

	// statementTimeoutSeconds limits execution time for the SQL. 0 means
	// no explicit timeout. pgwatch enforces this per-query.
	// +kubebuilder:validation:Minimum=0
	// +optional
	StatementTimeoutSeconds int32 `json:"statementTimeoutSeconds,omitempty"`

	// masterOnly restricts collection to primary PostgreSQL instances.
	// +optional
	MasterOnly bool `json:"masterOnly,omitempty"`

	// standbyOnly restricts collection to standby (replica) instances.
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
	// +kubebuilder:validation:MinItems=1
	Metrics []MetricDefinition `json:"metrics"`
}

// PgpilotMetricLibraryStatus is the observed state of PgpilotMetricLibrary.
type PgpilotMetricLibraryStatus struct {
	// observedGeneration is the .metadata.generation the controller last
	// reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

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
// +kubebuilder:printcolumn:name="Metrics",type="integer",JSONPath=".spec.metrics[*].name",description="Number of metric definitions",priority=0
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
	SchemeBuilder.Register(&PgpilotMetricLibrary{}, &PgpilotMetricLibraryList{})
}
