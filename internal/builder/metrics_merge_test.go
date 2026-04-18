package builder

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgpilotv1 "github.com/jamalshahverdiyev/pgpilot-operator/api/v1"
)

func duration(d time.Duration) *metav1.Duration {
	return &metav1.Duration{Duration: d}
}

func TestMergeMetrics_PresetOnly(t *testing.T) {
	spec := pgpilotv1.MetricsSpec{
		Preset: "exhaustive",
	}
	m := MergeMetrics(spec, nil)

	if m.Preset != "exhaustive" {
		t.Errorf("preset: got %q, want %q", m.Preset, "exhaustive")
	}
	if len(m.Intervals) != 0 {
		t.Errorf("intervals: got %d entries, want 0", len(m.Intervals))
	}
	if len(m.Definitions) != 0 {
		t.Errorf("definitions: got %d entries, want 0", len(m.Definitions))
	}
}

func TestMergeMetrics_CustomOnly(t *testing.T) {
	spec := pgpilotv1.MetricsSpec{
		Custom: []pgpilotv1.MetricDefinition{
			{
				Name:     "active_users",
				Interval: duration(30 * time.Second),
				SQLs:     map[string]string{"13": "SELECT 1"},
				Gauges:   []string{"*"},
			},
		},
	}
	m := MergeMetrics(spec, nil)

	if m.Preset != "" {
		t.Errorf("preset: got %q, want empty", m.Preset)
	}
	if m.Intervals["active_users"] != 30 {
		t.Errorf("interval: got %d, want 30", m.Intervals["active_users"])
	}
	def, ok := m.Definitions["active_users"]
	if !ok {
		t.Fatal("definition for active_users missing")
	}
	if def.SQLs[13] != "SELECT 1" {
		t.Errorf("sql[13]: got %q, want %q", def.SQLs[13], "SELECT 1")
	}
	if len(def.Gauges) != 1 || def.Gauges[0] != "*" {
		t.Errorf("gauges: got %v, want [*]", def.Gauges)
	}
}

func TestMergeMetrics_LibraryPlusCustom_OverrideByName(t *testing.T) {
	lib := pgpilotv1.PgpilotMetricLibrary{
		Spec: pgpilotv1.PgpilotMetricLibrarySpec{
			Metrics: []pgpilotv1.MetricDefinition{
				{
					Name:     "shared_metric",
					Interval: duration(60 * time.Second),
					SQLs:     map[string]string{"13": "SELECT 'from_lib'"},
				},
			},
		},
	}

	spec := pgpilotv1.MetricsSpec{
		Preset: "basic",
		Custom: []pgpilotv1.MetricDefinition{
			{
				Name:     "shared_metric",
				Interval: duration(15 * time.Second),
				SQLs:     map[string]string{"13": "SELECT 'from_custom'"},
			},
		},
	}

	m := MergeMetrics(spec, []pgpilotv1.PgpilotMetricLibrary{lib})

	if m.Preset != "basic" {
		t.Errorf("preset: got %q, want %q", m.Preset, "basic")
	}

	// Custom overrides library by name.
	if m.Intervals["shared_metric"] != 15 {
		t.Errorf("interval: got %d, want 15", m.Intervals["shared_metric"])
	}
	if m.Definitions["shared_metric"].SQLs[13] != "SELECT 'from_custom'" {
		t.Errorf("sql: got %q, want %q", m.Definitions["shared_metric"].SQLs[13], "SELECT 'from_custom'")
	}
}

func TestMergeMetrics_DefaultInterval(t *testing.T) {
	spec := pgpilotv1.MetricsSpec{
		Custom: []pgpilotv1.MetricDefinition{
			{
				Name: "no_interval",
				SQLs: map[string]string{"14": "SELECT 1"},
			},
		},
	}
	m := MergeMetrics(spec, nil)

	if m.Intervals["no_interval"] != defaultInterval {
		t.Errorf("interval: got %d, want %d", m.Intervals["no_interval"], defaultInterval)
	}
}

func TestMergeMetrics_MasterOnlyNodeStatus(t *testing.T) {
	spec := pgpilotv1.MetricsSpec{
		Custom: []pgpilotv1.MetricDefinition{
			{
				Name:       "primary_only",
				SQLs:       map[string]string{"13": "SELECT 1"},
				MasterOnly: true,
			},
			{
				Name:        "standby_only",
				SQLs:        map[string]string{"13": "SELECT 2"},
				StandbyOnly: true,
			},
		},
	}
	m := MergeMetrics(spec, nil)

	if m.Definitions["primary_only"].NodeStatus != "primary" {
		t.Errorf("nodeStatus: got %q, want %q", m.Definitions["primary_only"].NodeStatus, "primary")
	}
	if m.Definitions["standby_only"].NodeStatus != "standby" {
		t.Errorf("nodeStatus: got %q, want %q", m.Definitions["standby_only"].NodeStatus, "standby")
	}
}

func TestMergeMetrics_InvalidSQLVersion_Skipped(t *testing.T) {
	spec := pgpilotv1.MetricsSpec{
		Custom: []pgpilotv1.MetricDefinition{
			{
				Name: "bad_version",
				SQLs: map[string]string{
					"13":      "SELECT 1",
					"invalid": "SELECT 2",
				},
			},
		},
	}
	m := MergeMetrics(spec, nil)

	def := m.Definitions["bad_version"]
	if len(def.SQLs) != 1 {
		t.Errorf("sqls: got %d entries, want 1 (invalid version should be skipped)", len(def.SQLs))
	}
	if _, ok := def.SQLs[13]; !ok {
		t.Error("sqls[13] missing")
	}
}
