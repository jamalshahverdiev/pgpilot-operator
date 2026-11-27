package builder

import (
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

// BuildServiceMonitor creates a prometheus-operator ServiceMonitor that
// scrapes the pgwatch metrics Service. This should only be called when
// the monitoring.coreos.com/v1 CRD is present in the cluster.
// exportedNamespaceLabel is what Prometheus calls a scraped namespace label
// on a target that has one of its own.
const exportedNamespaceLabel = "exported_" + NamespaceTag

func BuildServiceMonitor(monitor *pgpilotv1.PgpilotMonitor) *monitoringv1.ServiceMonitor {
	name := ResourceName(monitor.Name)
	sysLabels := SystemLabels(monitor.Name)

	return &monitoringv1.ServiceMonitor{
		APIVersion: "monitoring.coreos.com/v1", Kind: "ServiceMonitor",
		Name:      name,
		Namespace: monitor.Namespace,
		Labels:    MergeLabels(sysLabels, monitor.Spec.ServiceMetadata.LabelMap()),
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
					Port:          metricsPortName,
					Interval:      monitoringv1.Duration("30s"),
					ScrapeTimeout: monitoringv1.Duration("10s"),
					// honorTimestamps is left at its default, true, on
					// purpose. pgwatch stamps every sample with the time
					// its query ran; with those timestamps rate() over a
					// counter is exact. Stamped with the time of the scrape
					// instead, a counter collected once a minute and
					// scraped twice becomes a staircase, and every rate()
					// swings by the share one collection has in its window.
					// What the default costs is that a metric collected
					// less often than every five minutes has gaps; see
					// docs/observability.md for how to query those.
					//
					// Every series carries the namespace of its monitor
					// as a label of pgwatch's own (NamespaceTag). The
					// target has a namespace label already, so Prometheus
					// keeps the scraped one as exported_namespace: the
					// same value a second time on every series. It is
					// emptied, which removes it, where it says what the
					// target says and only there. A metric with a
					// tag_namespace column has that column in the label
					// instead, the one thing that tells its rows apart.
					MetricRelabelConfigs: []monitoringv1.RelabelConfig{{
						SourceLabels: []monitoringv1.LabelName{exportedNamespaceLabel},
						Regex:        monitor.Namespace,
						TargetLabel:  exportedNamespaceLabel,
						Replacement:  new(""),
						Action:       "replace",
					}},
				},
			},
		},
	}
}
