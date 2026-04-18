package builder

import (
	"testing"
)

func TestBuildServiceMonitor_BasicShape(t *testing.T) {
	monitor := newTestMonitor()
	sm := BuildServiceMonitor(monitor)

	if sm.Name != "pgpilot-test-db" {
		t.Errorf("name: got %q, want %q", sm.Name, "pgpilot-test-db")
	}
	if sm.Namespace != "team-test" {
		t.Errorf("namespace: got %q, want %q", sm.Namespace, "team-test")
	}
}

func TestBuildServiceMonitor_Selector(t *testing.T) {
	monitor := newTestMonitor()
	sm := BuildServiceMonitor(monitor)

	sel := sm.Spec.Selector.MatchLabels
	if sel[LabelAppInstance] != "test-db" {
		t.Errorf("selector instance: got %q", sel[LabelAppInstance])
	}
	if sel[LabelAppName] != AppName {
		t.Errorf("selector name: got %q", sel[LabelAppName])
	}
}

func TestBuildServiceMonitor_Endpoint(t *testing.T) {
	monitor := newTestMonitor()
	sm := BuildServiceMonitor(monitor)

	if len(sm.Spec.Endpoints) != 1 {
		t.Fatalf("endpoints: got %d, want 1", len(sm.Spec.Endpoints))
	}
	ep := sm.Spec.Endpoints[0]
	if ep.Port != "metrics" {
		t.Errorf("port name: got %q, want %q", ep.Port, "metrics")
	}
	if ep.Interval != "30s" {
		t.Errorf("interval: got %q, want %q", string(ep.Interval), "30s")
	}
	if ep.ScrapeTimeout != "10s" {
		t.Errorf("scrapeTimeout: got %q, want %q", string(ep.ScrapeTimeout), "10s")
	}
}

func TestBuildServiceMonitor_NamespaceSelector(t *testing.T) {
	monitor := newTestMonitor()
	sm := BuildServiceMonitor(monitor)

	ns := sm.Spec.NamespaceSelector.MatchNames
	if len(ns) != 1 || ns[0] != "team-test" {
		t.Errorf("namespace selector: got %v, want [team-test]", ns)
	}
}

func TestBuildServiceMonitor_Labels_MergeServiceMetadata(t *testing.T) {
	monitor := newTestMonitor()
	monitor.Spec.ServiceMetadata.Labels = map[string]string{"team": "ops"}
	sm := BuildServiceMonitor(monitor)

	if sm.Labels["team"] != "ops" {
		t.Error("user label not merged onto ServiceMonitor")
	}
	if sm.Labels[LabelAppName] != AppName {
		t.Error("system label missing from ServiceMonitor")
	}
}
