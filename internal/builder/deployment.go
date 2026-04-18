package builder

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	pgpilotv1 "github.com/jamalshahverdiyev/pgpilot-operator/api/v1"
)

const (
	configVolumeName = "pgwatch-config"
	configMountPath  = "/etc/pgwatch"
	containerName    = "pgwatch"
)

// BuildDeployment creates the Deployment that runs the pgwatch collector.
func BuildDeployment(
	monitor *pgpilotv1.PgpilotMonitor,
	merged MergedMetrics,
	configHash string,
) *appsv1.Deployment {
	name := ResourceName(monitor.Name)
	sysLabels := SystemLabels(monitor.Name)
	podLabels := MergeLabels(sysLabels, monitor.Spec.PodMetadata.Labels)

	podAnnotations := map[string]string{
		AnnotationConfigHash: configHash,
	}
	podAnnotations = MergeAnnotations(podAnnotations, monitor.Spec.PodMetadata.Annotations)

	args := buildArgs(monitor, merged)
	envVars := buildEnvVars(monitor)
	promPort := prometheusPort(monitor)

	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: monitor.Namespace,
			Labels:    sysLabels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					LabelAppInstance: monitor.Name,
					LabelAppName:    AppName,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  containerName,
							Image: containerImage(monitor),
							Args:  args,
							Env:   envVars,
							Ports: []corev1.ContainerPort{
								{
									Name:          "metrics",
									ContainerPort: promPort,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							Resources: monitor.Spec.Resources,
							LivenessProbe:  tcpProbe(promPort, 15, 20),
							ReadinessProbe: tcpProbe(promPort, 5, 10),
							SecurityContext: &corev1.SecurityContext{
								RunAsNonRoot:             ptr.To(true),
								RunAsUser:                ptr.To[int64](65534),
								ReadOnlyRootFilesystem:   ptr.To(true),
								AllowPrivilegeEscalation: ptr.To(false),
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
									Name:      "tmp",
									MountPath: "/tmp",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: configVolumeName,
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: name + "-config",
									},
								},
							},
						},
						{
							Name: "tmp",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true),
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
	}

	if merged.Preset != "" || len(merged.Definitions) > 0 {
		args = append(args, "--metrics", configMountPath+"/"+MetricsKey)
	}

	sinks := monitor.Spec.Sinks

	if sinks.Prometheus != nil && sinks.Prometheus.Enabled {
		port := sinks.Prometheus.Port
		if port == 0 {
			port = 9187
		}
		args = append(args, "--sink", fmt.Sprintf("prometheus://0.0.0.0:%d", port))
	}

	if sinks.GRPC != nil && sinks.GRPC.Enabled && sinks.GRPC.Endpoint != "" {
		sink := "grpc://" + sinks.GRPC.Endpoint
		args = append(args, "--sink", sink)
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
	usernameKey := secret.UsernameKey
	if usernameKey == "" {
		usernameKey = "username"
	}
	passwordKey := secret.PasswordKey
	if passwordKey == "" {
		passwordKey = "password"
	}
	return []corev1.EnvVar{
		{
			Name: "PGUSER",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secret.Name},
					Key:                  usernameKey,
				},
			},
		},
		{
			Name: "PGPASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secret.Name},
					Key:                  passwordKey,
				},
			},
		},
	}
}

func containerImage(monitor *pgpilotv1.PgpilotMonitor) string {
	repo := monitor.Spec.Image.Repository
	if repo == "" {
		repo = "cybertecpostgresql/pgwatch"
	}
	tag := monitor.Spec.Image.Tag
	if tag == "" {
		tag = "5.1.0"
	}
	return repo + ":" + tag
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
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{
				Port: intstr.FromInt32(port),
			},
		},
		InitialDelaySeconds: initialDelay,
		PeriodSeconds:       period,
		TimeoutSeconds:      5,
		SuccessThreshold:    1,
		FailureThreshold:    3,
	}
}
