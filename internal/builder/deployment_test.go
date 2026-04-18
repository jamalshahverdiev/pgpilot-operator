package builder

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	pgpilotv1 "github.com/jamalshahverdiyev/pgpilot-operator/api/v1"
)

func TestBuildDeployment_BasicShape(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{Preset: "basic"}
	dep := BuildDeployment(monitor, merged, "abc123")

	if dep.Name != "pgpilot-test-db" {
		t.Errorf("name: got %q, want %q", dep.Name, "pgpilot-test-db")
	}
	if dep.Namespace != "team-test" {
		t.Errorf("namespace: got %q, want %q", dep.Namespace, "team-test")
	}
	if *dep.Spec.Replicas != 1 {
		t.Errorf("replicas: got %d, want 1", *dep.Spec.Replicas)
	}
}

func TestBuildDeployment_ContainerImage(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{}
	dep := BuildDeployment(monitor, merged, "h")

	img := dep.Spec.Template.Spec.Containers[0].Image
	if img != "cybertecpostgresql/pgwatch:5.1.0" {
		t.Errorf("image: got %q, want default", img)
	}

	monitor.Spec.Image.Repository = "myrepo/pgwatch"
	monitor.Spec.Image.Tag = "6.0.0"
	dep = BuildDeployment(monitor, merged, "h")
	img = dep.Spec.Template.Spec.Containers[0].Image
	if img != "myrepo/pgwatch:6.0.0" {
		t.Errorf("image override: got %q", img)
	}
}

func TestBuildDeployment_Args_PrometheusSink(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{Preset: "basic"}
	dep := BuildDeployment(monitor, merged, "h")

	args := dep.Spec.Template.Spec.Containers[0].Args
	found := false
	for i, a := range args {
		if a == "--sink" && i+1 < len(args) && strings.HasPrefix(args[i+1], "prometheus://") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("prometheus sink not found in args: %v", args)
	}
}

func TestBuildDeployment_Args_GRPCSink(t *testing.T) {
	monitor := newTestMonitor()
	monitor.Spec.Sinks.GRPC = &pgpilotv1.GRPCSink{
		Enabled:  true,
		Endpoint: "receiver.pgpilot:9000",
	}
	merged := MergedMetrics{}
	dep := BuildDeployment(monitor, merged, "h")

	args := dep.Spec.Template.Spec.Containers[0].Args
	found := false
	for i, a := range args {
		if a == "--sink" && i+1 < len(args) && args[i+1] == "grpc://receiver.pgpilot:9000" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("grpc sink not found in args: %v", args)
	}
}

func TestBuildDeployment_Args_MetricsFile_WhenPresetOrCustom(t *testing.T) {
	monitor := newTestMonitor()

	// No preset, no custom → no --metrics arg.
	merged := MergedMetrics{}
	dep := BuildDeployment(monitor, merged, "h")
	for _, a := range dep.Spec.Template.Spec.Containers[0].Args {
		if a == "--metrics" {
			t.Error("--metrics arg should not be present with empty merged")
		}
	}

	// Preset alone → --metrics arg present (we ship the built-in registry).
	merged = MergedMetrics{Preset: "basic"}
	dep = BuildDeployment(monitor, merged, "h")
	found := false
	for _, a := range dep.Spec.Template.Spec.Containers[0].Args {
		if a == "--metrics" {
			found = true
			break
		}
	}
	if !found {
		t.Error("--metrics arg should be present when preset is set")
	}
}

func TestBuildDeployment_EnvVars(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{}
	dep := BuildDeployment(monitor, merged, "h")

	envs := dep.Spec.Template.Spec.Containers[0].Env
	var userEnv, passEnv *corev1.EnvVar
	for i := range envs {
		switch envs[i].Name {
		case "PGUSER":
			userEnv = &envs[i]
		case "PGPASSWORD":
			passEnv = &envs[i]
		}
	}

	if userEnv == nil || userEnv.ValueFrom.SecretKeyRef.Name != "pg-creds" {
		t.Error("PGUSER env not pointing to correct secret")
	}
	if passEnv == nil || passEnv.ValueFrom.SecretKeyRef.Key != "password" {
		t.Error("PGPASSWORD env key mismatch")
	}
}

func TestBuildDeployment_EnvVars_Inline(t *testing.T) {
	monitor := newTestMonitor()
	monitor.Spec.Database.CredentialsSecret = nil
	monitor.Spec.Database.Username = "alice"
	monitor.Spec.Database.Password = "s3cret"
	merged := MergedMetrics{}
	dep := BuildDeployment(monitor, merged, "h")

	envs := dep.Spec.Template.Spec.Containers[0].Env
	var gotUser, gotPass string
	for _, e := range envs {
		if e.Name == "PGUSER" {
			gotUser = e.Value
			if e.ValueFrom != nil {
				t.Error("PGUSER should be literal value when credentials are inline")
			}
		}
		if e.Name == "PGPASSWORD" {
			gotPass = e.Value
			if e.ValueFrom != nil {
				t.Error("PGPASSWORD should be literal value when credentials are inline")
			}
		}
	}
	if gotUser != "alice" {
		t.Errorf("PGUSER: got %q, want %q", gotUser, "alice")
	}
	if gotPass != "s3cret" {
		t.Errorf("PGPASSWORD: got %q, want %q", gotPass, "s3cret")
	}
}

func TestBuildDeployment_ConfigHashAnnotation(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{}
	dep := BuildDeployment(monitor, merged, "deadbeef")

	ann := dep.Spec.Template.Annotations
	if ann[AnnotationConfigHash] != "deadbeef" {
		t.Errorf("config-hash: got %q, want %q", ann[AnnotationConfigHash], "deadbeef")
	}
}

func TestBuildDeployment_PodMetadata_Merge(t *testing.T) {
	monitor := newTestMonitor()
	monitor.Spec.PodMetadata.Labels = map[string]string{"team": "users"}
	monitor.Spec.PodMetadata.Annotations = map[string]string{"prometheus.io/scrape": "true"}
	merged := MergedMetrics{}
	dep := BuildDeployment(monitor, merged, "h")

	tmpl := dep.Spec.Template
	if tmpl.Labels["team"] != "users" {
		t.Error("user label not merged onto pod template")
	}
	if tmpl.Labels[LabelAppName] != AppName {
		t.Error("system label missing from pod template")
	}
	if tmpl.Annotations["prometheus.io/scrape"] != "true" {
		t.Error("user annotation not merged onto pod template")
	}
}

func TestBuildDeployment_SecurityContext(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{}
	dep := BuildDeployment(monitor, merged, "h")

	sc := dep.Spec.Template.Spec.Containers[0].SecurityContext
	if sc == nil {
		t.Fatal("securityContext is nil")
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("runAsNonRoot should be true")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem should be true")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation should be false")
	}
	if len(sc.Capabilities.Drop) == 0 || sc.Capabilities.Drop[0] != "ALL" {
		t.Error("capabilities should drop ALL")
	}
}

func TestBuildDeployment_Probes(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{}
	dep := BuildDeployment(monitor, merged, "h")

	c := dep.Spec.Template.Spec.Containers[0]
	if c.LivenessProbe == nil || c.LivenessProbe.TCPSocket == nil {
		t.Fatal("liveness probe should be a TCP socket probe")
	}
	if c.LivenessProbe.TCPSocket.Port.IntValue() != 9187 {
		t.Errorf("liveness port: got %d, want 9187", c.LivenessProbe.TCPSocket.Port.IntValue())
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.TCPSocket == nil {
		t.Fatal("readiness probe should be a TCP socket probe")
	}
	if c.ReadinessProbe.TCPSocket.Port.IntValue() != 9187 {
		t.Errorf("readiness port: got %d, want 9187", c.ReadinessProbe.TCPSocket.Port.IntValue())
	}
}
