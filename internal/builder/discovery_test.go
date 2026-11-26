package builder

import (
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakediscovery "k8s.io/client-go/discovery/fake"
	clienttesting "k8s.io/client-go/testing"
)

func discoveryWith(resources ...*metav1.APIResourceList) *fakediscovery.FakeDiscovery {
	return &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{Resources: resources}}
}

func TestHasServiceMonitorCRD(t *testing.T) {
	prometheusOperator := &metav1.APIResourceList{
		GroupVersion: "monitoring.coreos.com/v1",
		APIResources: []metav1.APIResource{{Kind: "PodMonitor"}, {Kind: "ServiceMonitor"}},
	}
	withoutServiceMonitor := &metav1.APIResourceList{
		GroupVersion: "monitoring.coreos.com/v1",
		APIResources: []metav1.APIResource{{Kind: "PodMonitor"}},
	}
	otherVersion := &metav1.APIResourceList{
		GroupVersion: "monitoring.coreos.com/v1alpha1",
		APIResources: []metav1.APIResource{{Kind: "ServiceMonitor"}},
	}
	core := &metav1.APIResourceList{GroupVersion: "v1", APIResources: []metav1.APIResource{{Kind: "Service"}}}

	for name, tc := range map[string]struct {
		served []*metav1.APIResourceList
		want   bool
	}{
		"prometheus-operator installed":            {[]*metav1.APIResourceList{core, prometheusOperator}, true},
		"not installed":                            {[]*metav1.APIResourceList{core}, false},
		"the group is served without that kind":    {[]*metav1.APIResourceList{core, withoutServiceMonitor}, false},
		"only another version of the group has it": {[]*metav1.APIResourceList{core, otherVersion}, false},
		"an API server that serves nothing at all": {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := HasServiceMonitorCRD(discoveryWith(tc.served...))
			if err != nil || got != tc.want {
				t.Errorf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}

	// Not knowing is not "no": the operator would run without ServiceMonitors
	// until its next restart, and say nothing about it.
	for name, failure := range map[string]error{
		"the API server is unavailable": apierrors.NewServiceUnavailable("apiserver is shutting down"),
		"the request is not allowed":    apierrors.NewUnauthorized("token expired"),
		"no answer":                     errors.New("dial tcp 10.0.0.1:443: i/o timeout"),
	} {
		t.Run(name, func(t *testing.T) {
			dc := discoveryWith(prometheusOperator)
			dc.PrependReactor("get", "resource", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, failure
			})
			got, err := HasServiceMonitorCRD(dc)
			if got || !errors.Is(err, failure) {
				t.Errorf("got %v, %v; want the failure handed on", got, err)
			}
			if !strings.Contains(err.Error(), "monitoring.coreos.com/v1") {
				t.Errorf("the error does not say what was asked for: %v", err)
			}
		})
	}
}
