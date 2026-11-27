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

// PodTemplateVersions say which configuration and which credentials a pod
// template is for. The collector takes its credentials from the environment,
// which is fixed when it starts, and its configuration from a ConfigMap that
// is made for that configuration alone and named after it. A change in
// either therefore changes the template and replaces the pod, which applies
// it at a known moment, and the pod it replaces keeps what it started with
// until it is gone.
type PodTemplateVersions struct {
	// Config is the hash of the rendered ConfigMap. It is an annotation
	// of the template, and part of the name of the ConfigMap it mounts.
	Config string
	// Credentials identifies the state of the Secret the pod takes its
	// credentials from: the one the monitor references, or the one generated
	// from its inline credentials.
	Credentials string
}

// defaultServiceAccount is the account a pod runs as when it names none.
const defaultServiceAccount = "default"

// DefaultRunAsUser is the user the pgwatch container runs as unless the
// operator is told otherwise: "nobody". The pgwatch image names no user of
// its own, which means root, and the pod must not run as root.
const DefaultRunAsUser int64 = 65534

// PodOption changes how BuildDeployment renders the pod for the cluster the
// operator runs in, as opposed to what a monitor asks for.
type PodOption func(*podSettings)

type podSettings struct {
	runAsUser *int64
}

// RunAsUser sets the user of the pgwatch container; nil leaves it to the
// platform. OpenShift assigns one from the range of the namespace, and its
// restricted security context constraints refuse a pod that asks for a user
// outside that range, which 65534 is.
func RunAsUser(uid *int64) PodOption {
	return func(s *podSettings) { s.runAsUser = uid }
}

// BuildDeployment creates the Deployment that runs the pgwatch collector.
func BuildDeployment(
	monitor *pgpilotv1.PgpilotMonitor,
	merged MergedMetrics,
	versions PodTemplateVersions,
	options ...PodOption,
) *appsv1.Deployment {
	settings := podSettings{runAsUser: new(DefaultRunAsUser)}
	for _, option := range options {
		option(&settings)
	}
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
		Env:             append(buildEnvVars(monitor), extraEnvVars(monitor)...),
		Resources:       monitor.Spec.Resources,
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             new(true),
			RunAsUser:                settings.runAsUser,
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
	tlsVolumes, tlsMounts := tlsVolumes(monitor)
	container.VolumeMounts = append(container.VolumeMounts, tlsMounts...)

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
			Replicas: ptr.To[int32](1),
			// No ReplicaSets of earlier revisions are kept. Nothing rolls
			// back to one: the pod template is the operator's to set, and a
			// rollback would be undone at the next reconcile. What they do
			// keep is whatever an earlier template held, which up to v1.0.2
			// was the inline password of the monitor.
			RevisionHistoryLimit: ptr.To[int32](0),
			MinReadySeconds:      minReadySeconds,
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
					Volumes: append([]corev1.Volume{
						{
							Name: configVolumeName,
							ConfigMap: &corev1.ConfigMapVolumeSource{
								Name: ConfigMapName(monitor.Name, versions.Config),
							},
						},
						{
							Name:     tmpVolumeName,
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						},
					}, tlsVolumes...),
					// With the container's settings above, this is what the
					// "restricted" Pod Security Standard asks for.
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   new(true),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					// Always named, "default" included. The API server keeps
					// the deprecated field serviceAccount in step with this
					// one, and fills this one from it when it is left out:
					// an account that was named once would stay for good
					// after it was taken out of the monitor.
					ServiceAccountName: orDefault(monitor.Spec.ServiceAccountName, defaultServiceAccount),
					ImagePullSecrets:   imagePullSecrets(monitor),
				},
			},
		},
	}
	if scheduling := monitor.Spec.Scheduling; scheduling != nil {
		pod := &dep.Spec.Template.Spec
		pod.NodeSelector = nodeSelector(scheduling)
		pod.Tolerations = scheduling.Tolerations
		pod.Affinity = scheduling.Affinity
		pod.TopologySpreadConstraints = scheduling.TopologySpreadConstraints
		pod.PriorityClassName = scheduling.PriorityClassName
	}

	return dep
}

func buildArgs(monitor *pgpilotv1.PgpilotMonitor, merged MergedMetrics) []string {
	args := []string{
		"--sources", configMountPath + "/" + SourcesKey,
		webDisableArg,
	}

	// Without the flag the collector uses the registry inside its image.
	if NeedsMetricsFile(merged) {
		args = append(args, "--metrics", configMountPath+"/"+MetricsKey)
	}

	if PrometheusSinkEnabled(monitor) {
		args = append(args, "--sink", fmt.Sprintf("prometheus://0.0.0.0:%d", prometheusPort(monitor)))
	}

	if GRPCSinkEnabled(monitor) {
		args = append(args, "--sink", grpcSinkAddress(monitor))
	}

	return args
}

// buildEnvVars points PGUSER and PGPASSWORD at a Secret: the one the monitor
// references, or the one the operator generates from inline credentials (see
// BuildCredentialsSecret). The values themselves are never in the pod
// template.
func buildEnvVars(monitor *pgpilotv1.PgpilotMonitor) []corev1.EnvVar {
	var name, usernameKey, passwordKey string
	switch secret := monitor.Spec.Database.CredentialsSecret; {
	// Inline credentials first, as the controller reads them.
	case HasInlineCredentials(monitor):
		name, usernameKey, passwordKey = CredentialsSecretName(monitor.Name), generatedUsernameKey, generatedPasswordKey
	case secret != nil:
		name = secret.Name
		usernameKey, passwordKey = CredentialKeys(secret)
	default:
		// No credentials at all: the controller reports that and builds
		// nothing.
		return nil
	}
	fromSecret := func(key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{Name: name, Key: key}}
	}
	return []corev1.EnvVar{
		{Name: "PGUSER", ValueFrom: fromSecret(usernameKey)},
		{Name: "PGPASSWORD", ValueFrom: fromSecret(passwordKey)},
	}
}

// extraEnvVars are the variables of spec.env, after those of the operator:
// a value may then refer to one of them as $(NAME).
func extraEnvVars(monitor *pgpilotv1.PgpilotMonitor) []corev1.EnvVar {
	if len(monitor.Spec.Env) == 0 {
		return nil
	}
	out := make([]corev1.EnvVar, 0, len(monitor.Spec.Env))
	for _, env := range monitor.Spec.Env {
		out = append(out, corev1.EnvVar{Name: env.Name, Value: env.Value, ValueFrom: env.ValueFrom})
	}
	return out
}

func imagePullSecrets(monitor *pgpilotv1.PgpilotMonitor) []corev1.LocalObjectReference {
	if len(monitor.Spec.Image.PullSecrets) == 0 {
		return nil
	}
	out := make([]corev1.LocalObjectReference, 0, len(monitor.Spec.Image.PullSecrets))
	for _, secret := range monitor.Spec.Image.PullSecrets {
		out = append(out, corev1.LocalObjectReference{Name: secret.Name})
	}
	return out
}

func nodeSelector(scheduling *pgpilotv1.SchedulingSpec) map[string]string {
	if len(scheduling.NodeSelector) == 0 {
		return nil
	}
	out := make(map[string]string, len(scheduling.NodeSelector))
	for key, value := range scheduling.NodeSelector {
		out[key] = string(value)
	}
	return out
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
