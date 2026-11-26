package builder

import (
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/discovery"
)

// serviceMonitorGroupVersion is the API the prometheus-operator serves its
// ServiceMonitor under.
const serviceMonitorGroupVersion = "monitoring.coreos.com/v1"

// HasServiceMonitorCRD reports whether the cluster serves the ServiceMonitor
// of the prometheus-operator. It is asked once, when the operator starts, to
// decide whether ServiceMonitors are generated and watched.
//
// Only "the API server does not know that group" means no. Any other failure
// is returned: taking it for a no would leave every monitor without its
// ServiceMonitor until the next restart, with nothing to say why.
func HasServiceMonitorCRD(dc discovery.DiscoveryInterface) (bool, error) {
	resources, err := dc.ServerResourcesForGroupVersion(serviceMonitorGroupVersion)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("ask the API server for %s: %w", serviceMonitorGroupVersion, err)
	}
	for _, r := range resources.APIResources {
		if r.Kind == "ServiceMonitor" {
			return true, nil
		}
	}
	return false, nil
}
