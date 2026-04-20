package builder

import (
	"testing"

	pgpilotv1 "github.com/jamalshahverdiyev/pgpilot-operator/api/v1"
)

func TestBuildService_BasicShape(t *testing.T) {
	monitor := newTestMonitor()
	svc := BuildService(monitor)

	if svc.Name != testDeploymentName {
		t.Errorf("name: got %q, want %q", svc.Name, testDeploymentName)
	}
	if svc.Namespace != testMonitorNS {
		t.Errorf("namespace: got %q, want %q", svc.Namespace, testMonitorNS)
	}
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("ports: got %d, want 1", len(svc.Spec.Ports))
	}
	if svc.Spec.Ports[0].Port != 9187 {
		t.Errorf("port: got %d, want 9187", svc.Spec.Ports[0].Port)
	}
}

func TestBuildService_Selector(t *testing.T) {
	monitor := newTestMonitor()
	svc := BuildService(monitor)

	if svc.Spec.Selector[LabelAppInstance] != testMonitorName {
		t.Errorf("selector instance: got %q", svc.Spec.Selector[LabelAppInstance])
	}
	if svc.Spec.Selector[LabelAppName] != AppName {
		t.Errorf("selector name: got %q", svc.Spec.Selector[LabelAppName])
	}
}

func TestBuildService_SystemLabels(t *testing.T) {
	monitor := newTestMonitor()
	svc := BuildService(monitor)

	if svc.Labels[LabelAppName] != AppName {
		t.Errorf("app name label: got %q", svc.Labels[LabelAppName])
	}
	if svc.Labels[LabelMonitorName] != testMonitorName {
		t.Errorf("monitor name label: got %q", svc.Labels[LabelMonitorName])
	}
}

func TestBuildService_ServiceMetadata_Merge(t *testing.T) {
	monitor := newTestMonitor()
	monitor.Spec.ServiceMetadata.Labels = map[string]string{"team": "data"}
	monitor.Spec.ServiceMetadata.Annotations = map[string]string{
		"prometheus.io/scrape": "true",
		"prometheus.io/port":   "9187",
	}
	svc := BuildService(monitor)

	if svc.Labels["team"] != "data" {
		t.Error("user label not merged")
	}
	// System label wins over user.
	if svc.Labels[LabelAppName] != AppName {
		t.Error("system label should not be overridden")
	}
	if svc.Annotations["prometheus.io/scrape"] != "true" {
		t.Error("user annotation not merged")
	}
}

func TestBuildService_CustomPort(t *testing.T) {
	monitor := newTestMonitor()
	monitor.Spec.Sinks.Prometheus = &pgpilotv1.PrometheusSink{
		Enabled: true,
		Port:    8080,
	}
	svc := BuildService(monitor)

	if svc.Spec.Ports[0].Port != 8080 {
		t.Errorf("port: got %d, want 8080", svc.Spec.Ports[0].Port)
	}
}
