package builder

import (
	"k8s.io/client-go/discovery"
)

// HasServiceMonitorCRD checks whether the monitoring.coreos.com/v1 API
// (prometheus-operator ServiceMonitor) is registered in the cluster.
// This is called once at operator startup to decide whether to generate
// ServiceMonitor resources during reconciliation.
func HasServiceMonitorCRD(dc discovery.DiscoveryInterface) bool {
	resources, err := dc.ServerResourcesForGroupVersion("monitoring.coreos.com/v1")
	if err != nil {
		return false
	}
	for _, r := range resources.APIResources {
		if r.Kind == "ServiceMonitor" {
			return true
		}
	}
	return false
}
