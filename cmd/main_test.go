/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakediscovery "k8s.io/client-go/discovery/fake"
	clienttesting "k8s.io/client-go/testing"
)

func clusterServing(groupVersions ...string) *fakediscovery.FakeDiscovery {
	resources := make([]*metav1.APIResourceList, 0, len(groupVersions))
	for _, gv := range groupVersions {
		resources = append(resources, &metav1.APIResourceList{GroupVersion: gv})
	}
	return &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{Resources: resources}}
}

// What the command line says has to arrive in the settings the controllers
// are started with. Each flag is set to something other than its default
// here, so that one that is read and then left out shows.
func TestControllerOptionsFor(t *testing.T) {
	kubernetes := clusterServing("v1", "apps/v1")
	openShift := clusterServing("v1", "apps/v1", "security.openshift.io/v1")

	options, err := controllerOptionsFor(commandLine{
		credentialsRecheck:    7 * time.Minute,
		migrateStoredImageTag: false,
		collectorRunAsUser:    "1001",
	}, true, kubernetes)
	if err != nil {
		t.Fatal(err)
	}
	if !options.ServiceMonitorEnabled || options.CredentialsRecheck != 7*time.Minute || !options.KeepStoredImageTag {
		t.Errorf("options %+v", options)
	}
	if uid := options.CollectorRunAsUser.RunAsUserIn("team"); uid == nil || *uid != 1001 {
		t.Errorf("--collector-run-as-user=1001 gives the collectors the user %v", uid)
	}

	// The defaults of the flags.
	defaults := commandLine{migrateStoredImageTag: true, collectorRunAsUser: "auto"}
	options, err = controllerOptionsFor(defaults, false, kubernetes)
	if err != nil {
		t.Fatal(err)
	}
	if options.ServiceMonitorEnabled || options.CredentialsRecheck != 0 || options.KeepStoredImageTag {
		t.Errorf("options %+v", options)
	}
	if uid := options.CollectorRunAsUser.RunAsUserIn("team"); uid == nil || *uid != 65534 {
		t.Errorf("by default the collectors run as %v", uid)
	}

	// The cluster is asked, and its answer used.
	options, err = controllerOptionsFor(commandLine{collectorRunAsUser: "auto"}, false, openShift)
	if err != nil {
		t.Fatal(err)
	}
	if uid := options.CollectorRunAsUser.RunAsUserIn("team"); uid != nil {
		t.Errorf("on OpenShift the collectors are given the user %d", *uid)
	}
	options, err = controllerOptionsFor(commandLine{collectorRunAsUser: "none"}, false, kubernetes)
	if err != nil || options.CollectorRunAsUser.RunAsUserIn("team") != nil {
		t.Errorf("--collector-run-as-user=none: %+v, %v", options, err)
	}

	// What cannot be run with is refused before anything is started.
	for name, flags := range map[string]commandLine{
		"a user that is none":            {collectorRunAsUser: "root"},
		"root":                           {collectorRunAsUser: "0"},
		"a recheck interval of a second": {credentialsRecheck: time.Second, collectorRunAsUser: "auto"},
	} {
		if _, err := controllerOptionsFor(flags, false, kubernetes); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := controllerOptionsFor(commandLine{collectorRunAsUser: "0"}, false, kubernetes); err == nil ||
		!strings.Contains(err.Error(), "which user the collectors run as") {
		t.Errorf("got %v", err)
	}
}
