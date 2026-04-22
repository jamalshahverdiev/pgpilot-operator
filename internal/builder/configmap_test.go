package builder

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

const (
	testMonitorName    = "test-db"
	testMonitorNS      = "team-test"
	testDeploymentName = "pgpilot-test-db"
)

func newTestMonitor() *pgpilotv1.PgpilotMonitor {
	return &pgpilotv1.PgpilotMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testMonitorName,
			Namespace: testMonitorNS,
		},
		Spec: pgpilotv1.PgpilotMonitorSpec{
			Database: pgpilotv1.DatabaseRef{
				Host:     "pg.example.com",
				Port:     5432,
				Database: "mydb",
				SSLMode:  pgpilotv1.SSLModeRequire,
				CredentialsSecret: &pgpilotv1.CredentialsSecretRef{
					Name: "pg-creds",
				},
				CustomTags: map[string]string{"env": "test"},
			},
			Sinks: pgpilotv1.SinksSpec{
				Prometheus: &pgpilotv1.PrometheusSink{
					Enabled: true,
					Port:    9187,
				},
			},
		},
	}
}

func TestBuildConfigMap_SourcesYAML_ContainsConnStr(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{Preset: "exhaustive"}

	cm, _, err := BuildConfigMap(monitor, merged)
	if err != nil {
		t.Fatalf("BuildConfigMap: %v", err)
	}

	sources := cm.Data[SourcesKey]
	if !strings.Contains(sources, "pg.example.com:5432") {
		t.Errorf("sources.yaml missing host:port, got:\n%s", sources)
	}
	if strings.Contains(sources, "PGUSER") || strings.Contains(sources, "PGPASSWORD") {
		t.Error("sources.yaml should not contain credentials — they are passed via PGUSER/PGPASSWORD env vars")
	}
	if !strings.Contains(sources, "sslmode=require") {
		t.Errorf("sources.yaml missing sslmode, got:\n%s", sources)
	}
	if !strings.Contains(sources, "preset_metrics: exhaustive") {
		t.Errorf("sources.yaml missing preset, got:\n%s", sources)
	}
}

func TestBuildConfigMap_NoMetricsYAML_WhenNoPresetAndNoCustom(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{}

	cm, _, err := BuildConfigMap(monitor, merged)
	if err != nil {
		t.Fatalf("BuildConfigMap: %v", err)
	}

	if _, ok := cm.Data[MetricsKey]; ok {
		t.Error("metrics.yaml should not be present when no preset and no custom definitions")
	}
}

func TestBuildConfigMap_MetricsYAML_WhenPresetOnly(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{Preset: "basic"}

	cm, _, err := BuildConfigMap(monitor, merged)
	if err != nil {
		t.Fatalf("BuildConfigMap: %v", err)
	}

	metricsYAML, ok := cm.Data[MetricsKey]
	if !ok {
		t.Fatal("metrics.yaml should be present when preset is set (to ship built-in registry)")
	}
	if !strings.Contains(metricsYAML, "db_stats") {
		t.Errorf("expected built-in metrics to be present, got first 200 chars:\n%s", metricsYAML[:min(200, len(metricsYAML))])
	}
	if !strings.Contains(metricsYAML, "presets:") {
		t.Error("expected presets section to be present when preset is set")
	}
}

func TestBuildConfigMap_MetricsYAML_PresetPlusCustom(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{
		Preset:    "exhaustive",
		Intervals: map[string]int{"my_metric": 30},
		Definitions: map[string]MetricDef{
			"my_metric": {SQLs: map[int]string{13: "SELECT 1"}},
		},
	}

	cm, _, err := BuildConfigMap(monitor, merged)
	if err != nil {
		t.Fatalf("BuildConfigMap: %v", err)
	}

	metricsYAML, ok := cm.Data[MetricsKey]
	if !ok {
		t.Fatal("metrics.yaml should be present")
	}
	if !strings.Contains(metricsYAML, "my_metric") {
		t.Error("custom metric missing")
	}
	if !strings.Contains(metricsYAML, "db_stats") {
		t.Error("built-in metric missing when preset is set alongside custom")
	}
}

func TestBuildConfigMap_MetricsYAML_WhenCustomPresent(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{
		Intervals: map[string]int{"my_metric": 30},
		Definitions: map[string]MetricDef{
			"my_metric": {
				SQLs:   map[int]string{13: "SELECT 1"},
				Gauges: []string{"*"},
			},
		},
	}

	cm, _, err := BuildConfigMap(monitor, merged)
	if err != nil {
		t.Fatalf("BuildConfigMap: %v", err)
	}

	metricsYAML, ok := cm.Data[MetricsKey]
	if !ok {
		t.Fatal("metrics.yaml should be present when custom definitions exist")
	}
	if !strings.Contains(metricsYAML, "my_metric") {
		t.Errorf("metrics.yaml missing metric name, got:\n%s", metricsYAML)
	}
}

func TestBuildConfigMap_HashStability(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{Preset: "basic"}

	_, hash1, _ := BuildConfigMap(monitor, merged)
	_, hash2, _ := BuildConfigMap(monitor, merged)

	if hash1 != hash2 {
		t.Errorf("hash not stable: %q != %q", hash1, hash2)
	}
}

func TestBuildConfigMap_HashChanges_OnSpecChange(t *testing.T) {
	monitor := newTestMonitor()
	merged1 := MergedMetrics{Preset: "basic"}
	merged2 := MergedMetrics{Preset: "exhaustive"}

	_, hash1, _ := BuildConfigMap(monitor, merged1)
	_, hash2, _ := BuildConfigMap(monitor, merged2)

	if hash1 == hash2 {
		t.Error("hash should change when preset changes")
	}
}

func TestBuildConfigMap_CustomTags(t *testing.T) {
	monitor := newTestMonitor()
	monitor.Spec.Database.CustomTags = map[string]string{"team": "users", "env": "prod"}
	merged := MergedMetrics{Preset: "basic"}

	cm, _, err := BuildConfigMap(monitor, merged)
	if err != nil {
		t.Fatalf("BuildConfigMap: %v", err)
	}

	sources := cm.Data[SourcesKey]
	if !strings.Contains(sources, "team: users") {
		t.Errorf("sources.yaml missing custom_tags, got:\n%s", sources)
	}
}

func TestBuildConfigMap_Labels(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{Preset: "basic"}

	cm, _, _ := BuildConfigMap(monitor, merged)

	if cm.Labels[LabelMonitorName] != testMonitorName {
		t.Errorf("label %s: got %q, want %q", LabelMonitorName, cm.Labels[LabelMonitorName], testMonitorName)
	}
	if cm.Name != "pgpilot-test-db-config" {
		t.Errorf("name: got %q, want %q", cm.Name, "pgpilot-test-db-config")
	}
	if cm.Namespace != testMonitorNS {
		t.Errorf("namespace: got %q, want %q", cm.Namespace, testMonitorNS)
	}
}

func TestBuildConfigMap_SourcesCustomMetrics(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{
		Intervals: map[string]int{"my_metric": 45},
		Definitions: map[string]MetricDef{
			"my_metric": {SQLs: map[int]string{13: "SELECT 1"}},
		},
	}

	cm, _, err := BuildConfigMap(monitor, merged)
	if err != nil {
		t.Fatalf("BuildConfigMap: %v", err)
	}

	sources := cm.Data[SourcesKey]
	if !strings.Contains(sources, "my_metric: 45") {
		t.Errorf("sources.yaml missing custom_metrics interval, got:\n%s", sources)
	}
}

// silence unused import
var _ = time.Second
