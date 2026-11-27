package builder

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

const (
	configVolumeName = "pgwatch-config"
	configMountPath  = "/etc/pgwatch"
	containerName    = "pgwatch"

	// metricsPortName names the Prometheus sink port on the container. The
	// Service and the ServiceMonitor refer to the port by this name.
	metricsPortName = "metrics"

	// tmpVolumeName is the emptyDir mounted at /tmp, the one writable place
	// of the container.
	tmpVolumeName = "tmp"

	// AnnotationSafeToEvictLocalVolumes tells the cluster autoscaler which
	// emptyDir volumes of a pod hold nothing worth keeping. Without it a pod
	// with a disk-backed emptyDir keeps its node from being scaled down.
	AnnotationSafeToEvictLocalVolumes = "cluster-autoscaler.kubernetes.io/safe-to-evict-local-volumes"

	// minReadySecondsWithoutProbes is how long a collector without probes
	// has to run before its pod counts as available.
	//
	// It stands in for a readiness probe, and so has to be longer than
	// pgwatch takes to give up on a gRPC sink it cannot use. A receiver that
	// refuses the connection, or a name that does not resolve, ends pgwatch
	// within a second. One that does not answer at all (a NetworkPolicy or a
	// firewall that drops the packets, a port where something else listens)
	// is waited for: 20 seconds for the connection, and up to 30 before that
	// for a name server that does not answer. A pod that counted as
	// available earlier would be reported as running, and would take the
	// place of the working pod of the previous spec, seconds before it
	// exits.
	minReadySecondsWithoutProbes = 60

	// webDisableArg turns off pgwatch's built-in web UI and REST API, which
	// otherwise listen on 0.0.0.0:8080 in every collector pod without
	// authentication. Its /test-connect endpoint opens a connection to a
	// caller-supplied address using the PGUSER/PGPASSWORD of the pod, so any
	// workload able to reach the pod could read the monitored database's
	// credentials. Nothing in the operator needs that server: probes and
	// scraping use the Prometheus sink port, where there is one.
	//
	// The value must stay attached with "=": pgwatch declares the flag with an
	// optional value, so a separate "all" argument would not be consumed.
	webDisableArg = "--web-disable=all"
)

// PodTemplateVersions are markers written as annotations on the pod template.
// The collector takes its credentials from the environment, which is fixed
// when it starts, and although it re-reads its configuration files every two
// minutes, a mounted ConfigMap takes about as long again to reach the pod. A
// change in either therefore replaces the pod, which applies it at a known
// moment; a changed annotation is what makes the Deployment do that.
type PodTemplateVersions struct {
	// Config is the hash of the rendered ConfigMap.
	Config string
	// Credentials identifies the state of the referenced credentials Secret.
	// Empty for inline credentials, which are part of the pod template
	// themselves.
	Credentials string
}

// BuildDeployment creates the Deployment that runs the pgwatch collector.
func BuildDeployment(
	monitor *pgpilotv1.PgpilotMonitor,
	merged MergedMetrics,
	versions PodTemplateVersions,
) *appsv1.Deployment {
	name := ResourceName(monitor.Name)
	sysLabels := SystemLabels(monitor.Name)
	podLabels := MergeLabels(sysLabels, monitor.Spec.PodMetadata.LabelMap())

	podAnnotations := map[string]string{
		AnnotationConfigHash: versions.Config,
	}
	if versions.Credentials != "" {
		podAnnotations[AnnotationCredentialsVersion] = versions.Credentials
	}
	// What the operator sets by default gives way to the user's annotations,
	// and those to the markers above.
	podAnnotations = MergeAnnotations(podAnnotations, MergeAnnotations(
		monitor.Spec.PodMetadata.Annotations,
		map[string]string{AnnotationSafeToEvictLocalVolumes: tmpVolumeName},
	))

	container := corev1.Container{
		Name:  containerName,
		Image: containerImage(monitor),
		// Left empty when the monitor names none: the API server then
		// decides by the tag, as it does for any pod.
		ImagePullPolicy: monitor.Spec.Image.PullPolicy,
		Args:            buildArgs(monitor, merged),
		Env:             buildEnvVars(monitor),
		Resources:       monitor.Spec.Resources,
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             new(true),
			RunAsUser:                ptr.To[int64](65534),
			ReadOnlyRootFilesystem:   new(true),
			AllowPrivilegeEscalation: new(false),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{
				Name:      configVolumeName,
				MountPath: configMountPath,
				ReadOnly:  true,
			},
			{
				Name:      tmpVolumeName,
				MountPath: "/tmp",
			},
		},
	}
	var minReadySeconds int32
	if PrometheusSinkEnabled(monitor) {
		port := prometheusPort(monitor)
		container.Ports = []corev1.ContainerPort{
			{Name: metricsPortName, ContainerPort: port, Protocol: corev1.ProtocolTCP},
		}
		container.LivenessProbe = tcpProbe(port, 15, 20)
		container.ReadinessProbe = tcpProbe(port, 5, 10)
	} else {
		// Without the Prometheus sink the collector listens nowhere: its
		// web server is turned off, and the gRPC sink only connects out.
		// A probe of the Prometheus port would never pass, and the pod
		// would be restarted every minute without ever becoming ready.
		// The kubelet restarts a collector that exits all the same; that it
		// has stayed up for a while is what stands in for readiness.
		minReadySeconds = minReadySecondsWithoutProbes
	}

	dep := &appsv1.Deployment{
		APIVersion: "apps/v1", Kind: "Deployment",
		Name:      name,
		Namespace: monitor.Namespace,
		Labels:    sysLabels,
		Spec: appsv1.DeploymentSpec{
			Replicas:        ptr.To[int32](1),
			MinReadySeconds: minReadySeconds,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					LabelAppInstance: monitor.Name,
					LabelAppName:     AppName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{container},
					// The collector never talks to the Kubernetes API.
					AutomountServiceAccountToken: new(false),
					Volumes: []corev1.Volume{
						{
							Name: configVolumeName,
							ConfigMap: &corev1.ConfigMapVolumeSource{
								Name: name + "-config",
							},
						},
						{
							Name:     tmpVolumeName,
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						},
					},
					// With the container's settings above, this is what the
					// "restricted" Pod Security Standard asks for.
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   new(true),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
				},
			},
		},
	}

	return dep
}

func buildArgs(monitor *pgpilotv1.PgpilotMonitor, merged MergedMetrics) []string {
	args := []string{
		"--sources", configMountPath + "/" + SourcesKey,
		webDisableArg,
	}

	if merged.Preset != "" || len(merged.Definitions) > 0 {
		args = append(args, "--metrics", configMountPath+"/"+MetricsKey)
	}

	if PrometheusSinkEnabled(monitor) {
		args = append(args, "--sink", fmt.Sprintf("prometheus://0.0.0.0:%d", prometheusPort(monitor)))
	}

	if GRPCSinkEnabled(monitor) {
		args = append(args, "--sink", "grpc://"+monitor.Spec.Sinks.GRPC.Endpoint)
	}

	return args
}

func buildEnvVars(monitor *pgpilotv1.PgpilotMonitor) []corev1.EnvVar {
	db := monitor.Spec.Database

	// Inline credentials take effect only when both username and password
	// are set (CRD validation enforces the exactly-one rule).
	if db.Username != "" && db.Password != "" {
		return []corev1.EnvVar{
			{Name: "PGUSER", Value: db.Username},
			{Name: "PGPASSWORD", Value: db.Password},
		}
	}

	// Secret-based credentials.
	secret := db.CredentialsSecret
	usernameKey, passwordKey := CredentialKeys(secret)
	return []corev1.EnvVar{
		{
			Name: "PGUSER",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					Name: secret.Name,
					Key:  usernameKey,
				},
			},
		},
		{
			Name: "PGPASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					Name: secret.Name,
					Key:  passwordKey,
				},
			},
		},
	}
}

// CredentialKeys returns the keys of the credentials Secret that hold the
// username and the password. A key left empty in the reference means the
// default name.
func CredentialKeys(ref *pgpilotv1.CredentialsSecretRef) (usernameKey, passwordKey string) {
	usernameKey, passwordKey = "username", "password"
	if ref == nil {
		return usernameKey, passwordKey
	}
	if ref.UsernameKey != "" {
		usernameKey = ref.UsernameKey
	}
	if ref.PasswordKey != "" {
		passwordKey = ref.PasswordKey
	}
	return usernameKey, passwordKey
}

// containerImage is the collector image of a monitor. Whatever the monitor
// leaves unset follows the operator, so that an operator upgrade moves the
// collector to the pgwatch release the new operator was built for.
func containerImage(monitor *pgpilotv1.PgpilotMonitor) string {
	repo := monitor.Spec.Image.Repository
	if repo == "" {
		repo = DefaultImageRepository
	}
	tag := monitor.Spec.Image.Tag
	if tag == "" {
		tag = PgwatchVersion
	}
	return repo + ":" + tag
}

// PrometheusSinkEnabled reports whether the collector serves /metrics. Only
// then is there something for a Service and a ServiceMonitor to point at.
func PrometheusSinkEnabled(monitor *pgpilotv1.PgpilotMonitor) bool {
	return monitor.Spec.Sinks.Prometheus != nil && monitor.Spec.Sinks.Prometheus.Enabled
}

// GRPCSinkEnabled reports whether the collector sends what it collects to a
// gRPC receiver. A sink that is enabled and names no endpoint is none.
func GRPCSinkEnabled(monitor *pgpilotv1.PgpilotMonitor) bool {
	grpc := monitor.Spec.Sinks.GRPC
	return grpc != nil && grpc.Enabled && grpc.Endpoint != ""
}

// HasSink reports whether the collector has anywhere to put what it
// collects. Without a sink pgwatch exits as soon as it starts.
func HasSink(monitor *pgpilotv1.PgpilotMonitor) bool {
	return PrometheusSinkEnabled(monitor) || GRPCSinkEnabled(monitor)
}

func prometheusPort(monitor *pgpilotv1.PgpilotMonitor) int32 {
	if monitor.Spec.Sinks.Prometheus != nil && monitor.Spec.Sinks.Prometheus.Port > 0 {
		return monitor.Spec.Sinks.Prometheus.Port
	}
	return 9187
}

// tcpProbe returns a TCP socket probe. We deliberately do NOT use HTTP
// probes against pgwatch: every path on its HTTP server (including
// /readiness and /liveness) returns the full Prometheus /metrics payload
// (~75 KB), and kubelet's HTTP probe closes the connection after reading
// the status line — causing pgwatch to spam "broken pipe" errors.
// A TCP probe just verifies the port is open, which is sufficient: the
// process is alive and accepting connections.
func tcpProbe(port int32, initialDelay, period int32) *corev1.Probe {
	return &corev1.Probe{
		TCPSocket: &corev1.TCPSocketAction{
			Port: intstr.FromInt32(port),
		},
		InitialDelaySeconds: initialDelay,
		PeriodSeconds:       period,
		TimeoutSeconds:      5,
		SuccessThreshold:    1,
		FailureThreshold:    3,
	}
}
