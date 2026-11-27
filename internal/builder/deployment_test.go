package builder

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

const sinkFlag = "--sink"

func TestBuildDeployment_BasicShape(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{Preset: "basic"}
	dep := BuildDeployment(monitor, merged, PodTemplateVersions{Config: "abc123"})

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
	dep := BuildDeployment(monitor, merged, PodTemplateVersions{Config: "h"})

	img := dep.Spec.Template.Spec.Containers[0].Image
	if img != "cybertecpostgresql/pgwatch:6.2.0" {
		t.Errorf("image: got %q, want default", img)
	}

	monitor.Spec.Image.Repository = "myrepo/pgwatch"
	monitor.Spec.Image.Tag = "6.0.0"
	dep = BuildDeployment(monitor, merged, PodTemplateVersions{Config: "h"})
	img = dep.Spec.Template.Spec.Containers[0].Image
	if img != "myrepo/pgwatch:6.0.0" {
		t.Errorf("image override: got %q", img)
	}
}

// The Service, the container port and the probes follow the port of the
// Prometheus sink, and without the sink there is no Service at all: pgwatch
// has to be started accordingly.
func TestBuildDeployment_Args_PrometheusSink(t *testing.T) {
	prometheusSinks := func(monitor *pgpilotv1.PgpilotMonitor) []string {
		args := BuildDeployment(monitor, MergedMetrics{Preset: "basic"}, PodTemplateVersions{Config: "h"}).
			Spec.Template.Spec.Containers[0].Args
		var found []string
		for i, a := range args {
			if a == sinkFlag && i+1 < len(args) && strings.HasPrefix(args[i+1], "prometheus://") {
				found = append(found, args[i+1])
			}
		}
		return found
	}
	grpc := &pgpilotv1.GRPCSink{Enabled: true, Endpoint: "receiver:9000"}

	for name, tc := range map[string]struct {
		sinks pgpilotv1.SinksSpec
		want  []string
	}{
		"on, default port": {
			pgpilotv1.SinksSpec{Prometheus: &pgpilotv1.PrometheusSink{Enabled: true}},
			[]string{"prometheus://0.0.0.0:9187"},
		},
		"on, its own port": {
			pgpilotv1.SinksSpec{Prometheus: &pgpilotv1.PrometheusSink{Enabled: true, Port: 9200}},
			[]string{"prometheus://0.0.0.0:9200"},
		},
		"turned off": {pgpilotv1.SinksSpec{Prometheus: &pgpilotv1.PrometheusSink{Enabled: false, Port: 9200}, GRPC: grpc}, nil},
		"not set":    {pgpilotv1.SinksSpec{GRPC: grpc}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			monitor := newTestMonitor()
			monitor.Spec.Sinks = tc.sinks
			if got := prometheusSinks(monitor); !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
			if got := PrometheusSinkEnabled(monitor); got != (len(tc.want) > 0) {
				t.Errorf("PrometheusSinkEnabled: %v", got)
			}
		})
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

			args := BuildDeployment(monitor, tt.merged, PodTemplateVersions{Config: "h"}).Spec.Template.Spec.Containers[0].Args

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
	args := BuildDeployment(newTestMonitor(), MergedMetrics{}, PodTemplateVersions{Config: "h"}).Spec.Template.Spec.Containers[0].Args

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
	dep := BuildDeployment(monitor, merged, PodTemplateVersions{Config: "h"})

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
	dep := BuildDeployment(monitor, merged, PodTemplateVersions{Config: "h"})
	for _, a := range dep.Spec.Template.Spec.Containers[0].Args {
		if a == "--metrics" {
			t.Error("--metrics arg should not be present with empty merged")
		}
	}

	// Preset alone → --metrics arg present (we ship the built-in registry).
	merged = MergedMetrics{Preset: "basic"}
	dep = BuildDeployment(monitor, merged, PodTemplateVersions{Config: "h"})
	if !slices.Contains(dep.Spec.Template.Spec.Containers[0].Args, "--metrics") {
		t.Error("--metrics arg should be present when preset is set")
	}
}

func TestBuildDeployment_EnvVars(t *testing.T) {
	monitor := newTestMonitor()
	merged := MergedMetrics{}
	dep := BuildDeployment(monitor, merged, PodTemplateVersions{Config: "h"})

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
	dep := BuildDeployment(monitor, merged, PodTemplateVersions{Config: "h"})

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
	dep := BuildDeployment(monitor, merged, PodTemplateVersions{Config: "deadbeef"})

	ann := dep.Spec.Template.Annotations
	if ann[AnnotationConfigHash] != "deadbeef" {
		t.Errorf("config-hash: got %q, want %q", ann[AnnotationConfigHash], "deadbeef")
	}
}

func TestBuildDeployment_PodMetadata_Merge(t *testing.T) {
	monitor := newTestMonitor()
	monitor.Spec.PodMetadata.Labels = map[string]pgpilotv1.LabelValue{"team": "users"}
	monitor.Spec.PodMetadata.Annotations = map[string]string{"prometheus.io/scrape": "true"}
	merged := MergedMetrics{}
	dep := BuildDeployment(monitor, merged, PodTemplateVersions{Config: "h"})

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
	dep := BuildDeployment(monitor, merged, PodTemplateVersions{Config: "h"})

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

// The port and the probes are those of the Prometheus sink. A collector
// without that sink listens nowhere: a probe of the port would never pass,
// and the kubelet would restart the container every minute for good.
func TestBuildDeployment_PortAndProbesFollowThePrometheusSink(t *testing.T) {
	grpc := &pgpilotv1.GRPCSink{Enabled: true, Endpoint: "receiver.monitoring:9000"}
	cases := map[string]struct {
		sinks    pgpilotv1.SinksSpec
		wantPort int32 // 0: neither a port nor probes
	}{
		"the sink on its default port": {pgpilotv1.SinksSpec{Prometheus: &pgpilotv1.PrometheusSink{Enabled: true}}, 9187},
		"the sink on another port":     {pgpilotv1.SinksSpec{Prometheus: &pgpilotv1.PrometheusSink{Enabled: true, Port: 9200}}, 9200},
		"next to the gRPC sink": {pgpilotv1.SinksSpec{
			Prometheus: &pgpilotv1.PrometheusSink{Enabled: true, Port: 9187}, GRPC: grpc,
		}, 9187},
		"the gRPC sink alone":     {pgpilotv1.SinksSpec{GRPC: grpc}, 0},
		"the sink turned off":     {pgpilotv1.SinksSpec{Prometheus: &pgpilotv1.PrometheusSink{Enabled: false, Port: 9187}, GRPC: grpc}, 0},
		"no sink at all (stored)": {pgpilotv1.SinksSpec{}, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			monitor := newTestMonitor()
			monitor.Spec.Sinks = tc.sinks
			dep := BuildDeployment(monitor, MergedMetrics{Preset: "basic"}, PodTemplateVersions{Config: "h"})
			c := dep.Spec.Template.Spec.Containers[0]

			if tc.wantPort == 0 {
				if len(c.Ports) != 0 || c.LivenessProbe != nil || c.ReadinessProbe != nil || c.StartupProbe != nil {
					t.Errorf("ports %+v, liveness %+v, readiness %+v, startup %+v: nothing listens in this pod",
						c.Ports, c.LivenessProbe, c.ReadinessProbe, c.StartupProbe)
				}
				// What stands in for readiness: having stayed up for longer
				// than pgwatch waits for a sink that does not answer (up to
				// 30 s for a name, then 20 s for the connection).
				if dep.Spec.MinReadySeconds != 60 {
					t.Errorf("minReadySeconds %d, want 60", dep.Spec.MinReadySeconds)
				}
				return
			}

			if len(c.Ports) != 1 || c.Ports[0].Name != "metrics" || c.Ports[0].ContainerPort != tc.wantPort ||
				c.Ports[0].Protocol != corev1.ProtocolTCP {
				t.Errorf("ports %+v", c.Ports)
			}
			for kind, probe := range map[string]*corev1.Probe{"liveness": c.LivenessProbe, "readiness": c.ReadinessProbe} {
				if probe == nil || probe.TCPSocket == nil || probe.TCPSocket.Port.IntValue() != int(tc.wantPort) {
					t.Errorf("the %s probe is %+v, want a TCP check of port %d", kind, probe, tc.wantPort)
				}
			}
			if c.LivenessProbe.InitialDelaySeconds != 15 || c.LivenessProbe.PeriodSeconds != 20 ||
				c.ReadinessProbe.InitialDelaySeconds != 5 || c.ReadinessProbe.PeriodSeconds != 10 {
				t.Errorf("liveness %+v, readiness %+v", c.LivenessProbe, c.ReadinessProbe)
			}
			// Two plain TCP checks and nothing else: one slow accept must
			// not restart the collector.
			if c.StartupProbe != nil {
				t.Errorf("startup probe %+v", c.StartupProbe)
			}
			for kind, probe := range map[string]*corev1.Probe{"liveness": c.LivenessProbe, "readiness": c.ReadinessProbe} {
				if probe.TimeoutSeconds != 5 || probe.FailureThreshold != 3 || probe.SuccessThreshold != 1 ||
					probe.HTTPGet != nil || probe.Exec != nil || probe.GRPC != nil {
					t.Errorf("the %s probe: %+v", kind, probe)
				}
			}
			// The readiness probe decides; nothing to wait for on top.
			if dep.Spec.MinReadySeconds != 0 {
				t.Errorf("minReadySeconds %d next to a readiness probe", dep.Spec.MinReadySeconds)
			}
		})
	}
}

func TestBuildDeployment_ImagePullPolicy(t *testing.T) {
	// "" is a monitor stored before the field had a default, or read
	// without it: the API server then decides by the tag.
	for _, policy := range []corev1.PullPolicy{corev1.PullAlways, corev1.PullNever, corev1.PullIfNotPresent, ""} {
		monitor := newTestMonitor()
		monitor.Spec.Image.PullPolicy = policy
		dep := BuildDeployment(monitor, MergedMetrics{}, PodTemplateVersions{Config: "h"})
		if got := dep.Spec.Template.Spec.Containers[0].ImagePullPolicy; got != policy {
			t.Errorf("spec.image.pullPolicy %q: the container has %q", policy, got)
		}
	}
}

// What the "restricted" Pod Security Standard asks of a pod, and what a
// process that never talks to the Kubernetes API does not need.
func TestBuildDeployment_PodHardening(t *testing.T) {
	for name, sinks := range map[string]pgpilotv1.SinksSpec{
		"with the Prometheus sink": {Prometheus: &pgpilotv1.PrometheusSink{Enabled: true}},
		"without":                  {GRPC: &pgpilotv1.GRPCSink{Enabled: true, Endpoint: "receiver:9000"}},
	} {
		t.Run(name, func(t *testing.T) {
			monitor := newTestMonitor()
			monitor.Spec.Sinks = sinks
			pod := BuildDeployment(monitor, MergedMetrics{}, PodTemplateVersions{Config: "h"}).Spec.Template.Spec

			sc := pod.SecurityContext
			if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
				t.Errorf("pod securityContext %+v: runAsNonRoot is not set", sc)
			}
			if sc == nil || sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault ||
				sc.SeccompProfile.LocalhostProfile != nil {
				t.Errorf("pod securityContext %+v: want seccomp profile RuntimeDefault", sc)
			}
			if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
				t.Errorf("automountServiceAccountToken %v: the collector gets a token it never uses", pod.AutomountServiceAccountToken)
			}
			if pod.ServiceAccountName != "" || pod.HostNetwork || pod.HostPID || pod.HostIPC {
				t.Errorf("serviceAccountName %q, hostNetwork %v, hostPID %v, hostIPC %v",
					pod.ServiceAccountName, pod.HostNetwork, pod.HostPID, pod.HostIPC)
			}
		})
	}
}

// Whatever the monitor says about resources is handed on as it is; the
// operator adds nothing of its own. A request made up here would override
// what a LimitRange of the namespace gives a container that names none, and
// one below the range's minimum would keep the pod from being created.
func TestBuildDeployment_ResourcesAreTheMonitors(t *testing.T) {
	build := func(resources corev1.ResourceRequirements) corev1.ResourceRequirements {
		monitor := newTestMonitor()
		monitor.Spec.Resources = resources
		return BuildDeployment(monitor, MergedMetrics{}, PodTemplateVersions{Config: "h"}).Spec.Template.Spec.Containers[0].Resources
	}
	if got := build(corev1.ResourceRequirements{}); len(got.Requests) != 0 || len(got.Limits) != 0 {
		t.Errorf("a monitor without resources got %+v", got)
	}
	set := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
	}
	if got := build(set); !got.Requests.Cpu().Equal(resource.MustParse("50m")) ||
		!got.Limits.Memory().Equal(resource.MustParse("256Mi")) || len(got.Requests) != 1 || len(got.Limits) != 1 {
		t.Errorf("got %+v, want %+v", got, set)
	}
}

// The cluster autoscaler does not remove a node that runs a pod with a
// disk-backed emptyDir unless the pod says the volume holds nothing to keep.
func TestBuildDeployment_SafeToEvictAnnotation(t *testing.T) {
	const key = "cluster-autoscaler.kubernetes.io/safe-to-evict-local-volumes"
	build := func(user map[string]string) *appsv1.Deployment {
		monitor := newTestMonitor()
		monitor.Spec.PodMetadata.Annotations = user
		return BuildDeployment(monitor, MergedMetrics{}, PodTemplateVersions{Config: "h", Credentials: "v1"})
	}

	t.Run("names the volume mounted at /tmp", func(t *testing.T) {
		dep := build(nil)
		got := dep.Spec.Template.Annotations[key]
		var emptyDirs []string
		for _, volume := range dep.Spec.Template.Spec.Volumes {
			if volume.EmptyDir != nil {
				emptyDirs = append(emptyDirs, volume.Name)
			}
		}
		if got != "tmp" || strings.Join(emptyDirs, ",") != got {
			t.Errorf("annotation %q, emptyDir volumes %q", got, emptyDirs)
		}
		mounts := dep.Spec.Template.Spec.Containers[0].VolumeMounts
		if !slices.ContainsFunc(mounts, func(m corev1.VolumeMount) bool { return m.Name == got && m.MountPath == "/tmp" }) {
			t.Errorf("volume %q is not mounted at /tmp: %+v", got, mounts)
		}
		// Not on the Deployment itself: the autoscaler reads pods.
		if _, onDeployment := dep.Annotations[key]; onDeployment {
			t.Errorf("the annotation is on the Deployment: %v", dep.Annotations)
		}
	})

	t.Run("gives way to what the user set", func(t *testing.T) {
		dep := build(map[string]string{key: "", "team": "data"})
		ann := dep.Spec.Template.Annotations
		if value, set := ann[key]; !set || value != "" || ann["team"] != "data" {
			t.Errorf("annotations %v", ann)
		}
	})

	t.Run("the operator's own markers still win over the user's", func(t *testing.T) {
		ann := build(map[string]string{AnnotationConfigHash: "mine", AnnotationCredentialsVersion: "mine"}).Spec.Template.Annotations
		if ann[AnnotationConfigHash] != "h" || ann[AnnotationCredentialsVersion] != "v1" || ann[key] != "tmp" {
			t.Errorf("annotations %v", ann)
		}
	})
}

// Two collectors for a moment, not none: a change that cannot start (an
// image that cannot be pulled, no room in a quota) leaves the collector that
// runs in place. The API server's default does that; a strategy set here
// would have to be kept in step with it.
func TestBuildDeployment_KeepsTheDefaultRolloutStrategy(t *testing.T) {
	dep := BuildDeployment(newTestMonitor(), MergedMetrics{}, PodTemplateVersions{Config: "h"})
	if dep.Spec.Strategy.Type != "" || dep.Spec.Strategy.RollingUpdate != nil {
		t.Errorf("strategy %+v", dep.Spec.Strategy)
	}
}

func TestCredentialKeys(t *testing.T) {
	tests := []struct {
		name         string
		ref          *pgpilotv1.CredentialsSecretRef
		wantUsername string
		wantPassword string
	}{
		{name: "nil reference", ref: nil, wantUsername: "username", wantPassword: "password"},
		{name: "keys left empty", ref: &pgpilotv1.CredentialsSecretRef{Name: "s"}, wantUsername: "username", wantPassword: "password"},
		{
			name:         "only the username key set",
			ref:          &pgpilotv1.CredentialsSecretRef{Name: "s", UsernameKey: "db-user"},
			wantUsername: "db-user", wantPassword: "password",
		},
		{
			name:         "only the password key set",
			ref:          &pgpilotv1.CredentialsSecretRef{Name: "s", PasswordKey: "db-pass"},
			wantUsername: "username", wantPassword: "db-pass",
		},
		{
			name:         "both set",
			ref:          &pgpilotv1.CredentialsSecretRef{Name: "s", UsernameKey: "u", PasswordKey: "p"},
			wantUsername: "u", wantPassword: "p",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotUsername, gotPassword := CredentialKeys(tt.ref)
			if gotUsername != tt.wantUsername || gotPassword != tt.wantPassword {
				t.Errorf("CredentialKeys = %q, %q; want %q, %q", gotUsername, gotPassword, tt.wantUsername, tt.wantPassword)
			}
		})
	}
}

// The keys the controller checks in the Secret must be the keys the pod reads.
func TestBuildDeployment_EnvUsesCredentialKeys(t *testing.T) {
	monitor := newTestMonitor()
	monitor.Spec.Database.CredentialsSecret = &pgpilotv1.CredentialsSecretRef{Name: "pg-creds", PasswordKey: "db-pass"}
	wantUsername, wantPassword := CredentialKeys(monitor.Spec.Database.CredentialsSecret)

	env := BuildDeployment(monitor, MergedMetrics{}, PodTemplateVersions{Config: "h"}).Spec.Template.Spec.Containers[0].Env
	got := map[string]string{}
	for _, e := range env {
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("%s does not come from the Secret", e.Name)
		}
		got[e.Name] = e.ValueFrom.SecretKeyRef.Key
	}
	if got["PGUSER"] != wantUsername || got["PGPASSWORD"] != wantPassword {
		t.Errorf("env keys = %v, want PGUSER=%s PGPASSWORD=%s", got, wantUsername, wantPassword)
	}
}

func TestBuildDeployment_CredentialsVersionAnnotation(t *testing.T) {
	monitor := newTestMonitor()
	// Users cannot set the operator's annotations themselves.
	monitor.Spec.PodMetadata.Annotations = map[string]string{
		AnnotationCredentialsVersion: "from-the-user",
		AnnotationConfigHash:         "from-the-user",
		"example.com/keep":           "yes",
	}

	t.Run("set when the credentials come from a Secret", func(t *testing.T) {
		dep := BuildDeployment(monitor, MergedMetrics{}, PodTemplateVersions{Config: "cfg", Credentials: "v1"})
		annotations := dep.Spec.Template.Annotations
		if annotations[AnnotationCredentialsVersion] != "v1" {
			t.Errorf("%s = %q, want v1", AnnotationCredentialsVersion, annotations[AnnotationCredentialsVersion])
		}
		if annotations[AnnotationConfigHash] != "cfg" {
			t.Errorf("%s = %q, want cfg", AnnotationConfigHash, annotations[AnnotationConfigHash])
		}
		if annotations["example.com/keep"] != "yes" {
			t.Error("user annotations must be kept")
		}
		if _, onDeployment := dep.Annotations[AnnotationCredentialsVersion]; onDeployment {
			t.Error("the version belongs on the pod template, where a change replaces the pod")
		}
	})

	t.Run("a different version changes the pod template", func(t *testing.T) {
		a := BuildDeployment(monitor, MergedMetrics{}, PodTemplateVersions{Config: "cfg", Credentials: "v1"})
		b := BuildDeployment(monitor, MergedMetrics{}, PodTemplateVersions{Config: "cfg", Credentials: "v2"})
		if a.Spec.Template.Annotations[AnnotationCredentialsVersion] == b.Spec.Template.Annotations[AnnotationCredentialsVersion] {
			t.Error("pod templates are identical for different credential versions")
		}
	})

	t.Run("absent for inline credentials", func(t *testing.T) {
		inline := newTestMonitor()
		inline.Spec.Database.CredentialsSecret = nil
		inline.Spec.Database.Username, inline.Spec.Database.Password = "u", "p"
		dep := BuildDeployment(inline, MergedMetrics{}, PodTemplateVersions{Config: "cfg"})
		if _, ok := dep.Spec.Template.Annotations[AnnotationCredentialsVersion]; ok {
			t.Error("no credentials version is expected without a Secret")
		}
	})
}

func sinkArgs(args []string) []string {
	var out []string
	for i, a := range args {
		if a == "--sink" && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

// What pgwatch is told to do and what the pod promises the kubelet have to
// be the same thing: a port and probes exactly when pgwatch is started with
// the Prometheus sink on that port, and a gRPC sink exactly when it is
// enabled and names an endpoint.
func TestBuildDeployment_SinkArgsMatchThePod(t *testing.T) {
	prom := func(enabled bool, port int32) *pgpilotv1.PrometheusSink {
		return &pgpilotv1.PrometheusSink{Enabled: enabled, Port: port}
	}
	cases := map[string]struct {
		sinks pgpilotv1.SinksSpec
		want  []string
	}{
		"Prometheus alone": {pgpilotv1.SinksSpec{Prometheus: prom(true, 0)}, []string{"prometheus://0.0.0.0:9187"}},
		"both": {
			pgpilotv1.SinksSpec{Prometheus: prom(true, 9200), GRPC: &pgpilotv1.GRPCSink{Enabled: true, Endpoint: "receiver:9000"}},
			[]string{"prometheus://0.0.0.0:9200", "grpc://receiver:9000"},
		},
		"gRPC alone": {
			pgpilotv1.SinksSpec{Prometheus: prom(false, 9187), GRPC: &pgpilotv1.GRPCSink{Enabled: true, Endpoint: "receiver:9000"}},
			[]string{"grpc://receiver:9000"},
		},
		"a gRPC sink that names an endpoint and is not enabled": {
			pgpilotv1.SinksSpec{Prometheus: prom(true, 0), GRPC: &pgpilotv1.GRPCSink{Endpoint: "receiver:9000"}},
			[]string{"prometheus://0.0.0.0:9187"},
		},
		"a gRPC sink that is enabled and names no endpoint (stored)": {
			pgpilotv1.SinksSpec{Prometheus: prom(true, 0), GRPC: &pgpilotv1.GRPCSink{Enabled: true}},
			[]string{"prometheus://0.0.0.0:9187"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			monitor := newTestMonitor()
			monitor.Spec.Sinks = tc.sinks
			c := BuildDeployment(monitor, MergedMetrics{Preset: "basic"}, PodTemplateVersions{Config: "h"}).
				Spec.Template.Spec.Containers[0]

			got := sinkArgs(c.Args)
			if !slices.Equal(got, tc.want) {
				t.Errorf("sinks %q, want %q", got, tc.want)
			}
			// The port the kubelet probes is the port pgwatch listens on.
			listens := ""
			if len(c.Ports) == 1 {
				listens = fmt.Sprintf("prometheus://0.0.0.0:%d", c.Ports[0].ContainerPort)
			}
			if slices.Contains(got, listens) != (len(c.Ports) == 1) || (len(c.Ports) == 0) != (c.ReadinessProbe == nil) {
				t.Errorf("ports %+v and readiness probe %+v do not go with the sinks %q", c.Ports, c.ReadinessProbe, got)
			}
		})
	}
}
