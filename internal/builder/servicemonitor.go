package builder

import (
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

// BuildServiceMonitor creates a prometheus-operator ServiceMonitor that
// scrapes the pgwatch metrics Service. This should only be called when
// the monitoring.coreos.com/v1 CRD is present in the cluster.
func BuildServiceMonitor(monitor *pgpilotv1.PgpilotMonitor) *monitoringv1.ServiceMonitor {
	name := ResourceName(monitor.Name)
	sysLabels := SystemLabels(monitor.Name)

	return &monitoringv1.ServiceMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: "monitoring.coreos.com/v1", Kind: "ServiceMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: monitor.Namespace,
			Labels:    MergeLabels(sysLabels, monitor.Spec.ServiceMetadata.Labels),
		},
		Spec: monitoringv1.ServiceMonitorSpec{
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					LabelAppInstance: monitor.Name,
					LabelAppName:     AppName,
				},
			},
			NamespaceSelector: monitoringv1.NamespaceSelector{
				MatchNames: []string{monitor.Namespace},
			},
			Endpoints: []monitoringv1.Endpoint{
				{
					Port:          "metrics",
					Interval:      monitoringv1.Duration("30s"),
					ScrapeTimeout: monitoringv1.Duration("10s"),
				},
			},
		},
	}
}
