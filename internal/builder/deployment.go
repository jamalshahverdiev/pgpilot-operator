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

	// webDisableArg turns off pgwatch's built-in web UI and REST API, which
	// otherwise listen on 0.0.0.0:8080 in every collector pod without
	// authentication. Its /test-connect endpoint opens a connection to a
	// caller-supplied address using the PGUSER/PGPASSWORD of the pod, so any
	// workload able to reach the pod could read the monitored database's
	// credentials. Nothing in the operator needs that server: probes and
	// scraping use the Prometheus sink port.
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
	podAnnotations = MergeAnnotations(podAnnotations, monitor.Spec.PodMetadata.Annotations)

	args := buildArgs(monitor, merged)
	envVars := buildEnvVars(monitor)
	promPort := prometheusPort(monitor)

	dep := &appsv1.Deployment{
		APIVersion: "apps/v1", Kind: "Deployment",
		Name:      name,
		Namespace: monitor.Namespace,
		Labels:    sysLabels,
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
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
					Containers: []corev1.Container{
						{
							Name:  containerName,
							Image: containerImage(monitor),
							Args:  args,
							Env:   envVars,
							Ports: []corev1.ContainerPort{
								{
									Name:          metricsPortName,
									ContainerPort: promPort,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							Resources:      monitor.Spec.Resources,
							LivenessProbe:  tcpProbe(promPort, 15, 20),
							ReadinessProbe: tcpProbe(promPort, 5, 10),
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
									Name:      "tmp",
									MountPath: "/tmp",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: configVolumeName,
							ConfigMap: &corev1.ConfigMapVolumeSource{
								Name: name + "-config",
							},
						},
						{
							Name:     "tmp",
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						},
					},
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: new(true),
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
