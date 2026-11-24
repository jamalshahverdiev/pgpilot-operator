package builder

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

// BuildService creates the ClusterIP Service that exposes the pgwatch
// Prometheus /metrics endpoint. Prometheus, vmagent, or ServiceMonitor
// scrape this Service.
func BuildService(monitor *pgpilotv1.PgpilotMonitor) *corev1.Service {
	name := ResourceName(monitor.Name)
	sysLabels := SystemLabels(monitor.Name)
	port := prometheusPort(monitor)

	svcLabels := MergeLabels(sysLabels, monitor.Spec.ServiceMetadata.Labels)
	svcAnnotations := MergeAnnotations(nil, monitor.Spec.ServiceMetadata.Annotations)

	svc := &corev1.Service{
		APIVersion: "v1", Kind: "Service",
		Name:        name,
		Namespace:   monitor.Namespace,
		Labels:      svcLabels,
		Annotations: svcAnnotations,
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Selector: map[string]string{
				LabelAppInstance: monitor.Name,
				LabelAppName:     AppName,
			},
			Ports: []corev1.ServicePort{
				{
					Name:       metricsPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       port,
					TargetPort: intstr.FromString(metricsPortName),
				},
			},
		},
	}

	return svc
}
