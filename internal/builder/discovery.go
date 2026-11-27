package builder

import (
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/discovery"
)

// serviceMonitorGroupVersion is the API the prometheus-operator serves its
// ServiceMonitor under.
const serviceMonitorGroupVersion = "monitoring.coreos.com/v1"

// openShiftSecurityGroup is the API group of OpenShift's security context
// constraints. A cluster that serves it enforces them, and that is what
// matters here: OpenShift, OKD and MicroShift do, and so would a cluster on
// which someone installed the group for another reason, where the operator
// has to be told the user of the collector instead.
const openShiftSecurityGroup = "security.openshift.io"

// IsOpenShift reports whether the cluster serves the API group of security
// context constraints. It is asked once, when the operator starts, to decide
// who names the user the collector runs as.
//
// A failure is returned, not taken for a no: on OpenShift that would give
// every collector a user its namespace does not allow.
func IsOpenShift(dc discovery.DiscoveryInterface) (bool, error) {
	groups, err := dc.ServerGroups()
	if err != nil {
		return false, fmt.Errorf("ask the API server for its API groups: %w", err)
	}
	for _, group := range groups.Groups {
		if group.Name == openShiftSecurityGroup {
			return true, nil
		}
	}
	return false, nil
}

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
