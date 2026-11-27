package builder

import (
	"strconv"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

// MergedMetrics is the result of resolving all metric sources for a single
// PgpilotMonitor. The ConfigMap builder consumes this to produce pgwatch's
// sources.yaml and metrics.yaml files.
type MergedMetrics struct {
	// Preset is the pgwatch built-in preset name (e.g. "exhaustive").
	// Mapped to preset_metrics in sources.yaml. Empty if not set.
	Preset string

	// Intervals maps metric name → collection interval in seconds.
	// These go into custom_metrics in sources.yaml. Only populated for
	// metrics that come from libraries or inline custom definitions.
	Intervals map[string]int

	// IntervalUnset names the metrics whose definition gives no interval.
	// Intervals holds the default for them, which is what they are
	// collected at unless they stand in for a metric of the preset, whose
	// interval they then keep.
	IntervalUnset map[string]bool

	// Definitions holds the SQL definitions for custom metrics (from
	// libraries and inline custom). These go into metrics.yaml.
	// Built-in preset metrics are NOT included: they are taken from the
	// registry of the pgwatch release.
	Definitions map[string]MetricDef
}

// MetricDef mirrors pgwatch's internal/metrics.Metric, but only the fields
// we populate from CRD data.
type MetricDef struct {
	SQLs            map[int]string // PG major version → SQL
	Gauges          []string
	IsInstanceLevel bool
	NodeStatus      string // "primary", "standby", or "" (both)
	Description     string
}

// MergeMetrics resolves the effective metric set for a PgpilotMonitor.
//
// Merge order (later wins by metric name):
//  1. Metrics from referenced PgpilotMetricLibrary CRs (in order listed)
//  2. Inline custom metrics from the monitor spec
//
// The preset is passed through as-is: its metrics are resolved from the
// registry, by pgwatch or, next to metrics of the monitor's own, by
// expandPresetIntoIntervals.
func MergeMetrics(
	spec pgpilotv1.MetricsSpec,
	libraries []pgpilotv1.PgpilotMetricLibrary,
) MergedMetrics {
	m := MergedMetrics{
		Preset:        spec.Preset,
		Intervals:     make(map[string]int),
		IntervalUnset: make(map[string]bool),
		Definitions:   make(map[string]MetricDef),
	}

	for _, lib := range libraries {
		for _, md := range lib.Spec.Metrics {
			addMetric(&m, md)
		}
	}

	for _, md := range spec.Custom {
		addMetric(&m, md)
	}

	return m
}

func addMetric(m *MergedMetrics, md pgpilotv1.MetricDefinition) {
	interval := defaultInterval
	if md.Interval != nil {
		interval = int(md.Interval.Seconds())
	}
	if interval <= 0 {
		interval = defaultInterval
	}
	m.Intervals[md.Name] = interval
	// The later definition of a name replaces the earlier one whole,
	// including whether it names an interval.
	if md.Interval == nil {
		m.IntervalUnset[md.Name] = true
	} else {
		delete(m.IntervalUnset, md.Name)
	}

	sqls := make(map[int]string, len(md.SQLs))
	for verStr, sql := range md.SQLs {
		ver, err := strconv.Atoi(verStr)
		if err != nil {
			continue
		}
		sqls[ver] = sql
	}

	nodeStatus := ""
	if md.MasterOnly {
		nodeStatus = "primary"
	} else if md.StandbyOnly {
		nodeStatus = "standby"
	}

	m.Definitions[md.Name] = MetricDef{
		SQLs:            sqls,
		Gauges:          md.Gauges,
		IsInstanceLevel: md.IsInstanceLevel,
		NodeStatus:      nodeStatus,
		Description:     md.Description,
	}
}

const defaultInterval = 60
