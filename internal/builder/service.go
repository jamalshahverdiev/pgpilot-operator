package builder

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   monitor.Namespace,
			Labels:      svcLabels,
			Annotations: svcAnnotations,
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Selector: map[string]string{
				LabelAppInstance: monitor.Name,
				LabelAppName:     AppName,
			},
			Ports: []corev1.ServicePort{
				{
					Name:       "metrics",
					Protocol:   corev1.ProtocolTCP,
					Port:       port,
					TargetPort: intstr.FromString("metrics"),
				},
			},
		},
	}

	return svc
}
