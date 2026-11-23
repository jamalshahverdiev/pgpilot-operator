package builder

import (
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

const sinkFlag = "--sink"

func TestBuildDeployment_BasicShape(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{Preset: "basic"}
	dep := BuildDeployment(monitor, merged, "abc123")

	if dep.Name != testDeploymentName {
		t.Errorf("name: got %q, want %q", dep.Name, testDeploymentName)
	}
	if dep.Namespace != testMonitorNS {
		t.Errorf("namespace: got %q, want %q", dep.Namespace, testMonitorNS)
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
		if a == sinkFlag && i+1 < len(args) && strings.HasPrefix(args[i+1], "prometheus://") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("prometheus sink not found in args: %v", args)
	}
}

// pgwatch's web UI / REST API must be off for every sink combination: it is
// unauthenticated and can be used to leak the database credentials.
func TestBuildDeployment_Args_WebServerDisabled(t *testing.T) {
	tests := []struct {
		name   string
		sinks  pgpilotv1.SinksSpec
		merged MergedMetrics
	}{
		{
			name:  "prometheus sink",
			sinks: pgpilotv1.SinksSpec{Prometheus: &pgpilotv1.PrometheusSink{Enabled: true, Port: 9187}},
		},
		{
			name:  "prometheus sink on pgwatch's default web port",
			sinks: pgpilotv1.SinksSpec{Prometheus: &pgpilotv1.PrometheusSink{Enabled: true, Port: 8080}},
		},
		{
			name:  "grpc sink only",
			sinks: pgpilotv1.SinksSpec{GRPC: &pgpilotv1.GRPCSink{Enabled: true, Endpoint: "receiver:9000"}},
		},
		{
			name: "both sinks",
			sinks: pgpilotv1.SinksSpec{
				Prometheus: &pgpilotv1.PrometheusSink{Enabled: true, Port: 9187},
				GRPC:       &pgpilotv1.GRPCSink{Enabled: true, Endpoint: "receiver:9000"},
			},
		},
		{
			name:  "prometheus sink disabled",
			sinks: pgpilotv1.SinksSpec{Prometheus: &pgpilotv1.PrometheusSink{Enabled: false}},
		},
		{
			name: "no sinks",
		},
		{
			name:   "preset with custom metrics",
			sinks:  pgpilotv1.SinksSpec{Prometheus: &pgpilotv1.PrometheusSink{Enabled: true, Port: 9187}},
			merged: MergedMetrics{Preset: "basic", Definitions: map[string]MetricDef{"m": {}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			monitor := newTestMonitor()
			monitor.Spec.Sinks = tt.sinks

			args := BuildDeployment(monitor, tt.merged, "h").Spec.Template.Spec.Containers[0].Args

			if got := countArg(args, "--web-disable=all"); got != 1 {
				t.Errorf("want exactly one --web-disable=all, got %d in %v", got, args)
			}
			// The value has to be part of the same token; a detached "all"
			// would be treated by pgwatch as a positional argument.
			if slices.Contains(args, "--web-disable") || slices.Contains(args, "all") {
				t.Errorf("--web-disable must use the single-token form, got %v", args)
			}
			// Nothing may re-enable or move the web server.
			for _, a := range args {
				if strings.HasPrefix(a, "--web-addr") || strings.HasPrefix(a, "--web-user") {
					t.Errorf("unexpected web server argument %q in %v", a, args)
				}
			}
		})
	}
}

// The web server flag must not displace the arguments pgwatch needs.
func TestBuildDeployment_Args_SourcesStillFirst(t *testing.T) {
	args := BuildDeployment(newTestMonitor(), MergedMetrics{}, "h").Spec.Template.Spec.Containers[0].Args

	if len(args) < 2 || args[0] != "--sources" || args[1] != "/etc/pgwatch/sources.yaml" {
		t.Fatalf("args must start with --sources <file>, got %v", args)
	}
	for i, a := range args {
		if a == sinkFlag || a == "--metrics" || a == "--sources" {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				t.Errorf("flag %q at index %d has no value in %v", a, i, args)
			}
		}
	}
}

func countArg(args []string, want string) int {
	n := 0
	for _, a := range args {
		if a == want {
			n++
		}
	}
	return n
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
		if a == sinkFlag && i+1 < len(args) && args[i+1] == "grpc://receiver.pgpilot:9000" {
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
	if !slices.Contains(dep.Spec.Template.Spec.Containers[0].Args, "--metrics") {
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
