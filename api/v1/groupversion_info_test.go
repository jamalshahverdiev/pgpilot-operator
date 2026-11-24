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

package v1

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestGroupVersion(t *testing.T) {
	if GroupVersion.Group != "pgpilot.io" || GroupVersion.Version != "v1" {
		t.Fatalf("GroupVersion = %s, want pgpilot.io/v1", GroupVersion)
	}
}

// Every kind of the API group, and its list type, has to be known to a scheme
// built with AddToScheme; a kind missing here cannot be read or written by the
// manager's client.
func TestAddToScheme_RegistersEveryKind(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	objects := map[string]runtime.Object{
		"PgpilotMonitor":           &PgpilotMonitor{},
		"PgpilotMonitorList":       &PgpilotMonitorList{},
		"PgpilotMetricLibrary":     &PgpilotMetricLibrary{},
		"PgpilotMetricLibraryList": &PgpilotMetricLibraryList{},
	}
	for kind, obj := range objects {
		t.Run(kind, func(t *testing.T) {
			gvks, unversioned, err := scheme.ObjectKinds(obj)
			if err != nil {
				t.Fatalf("ObjectKinds: %v", err)
			}
			if unversioned {
				t.Errorf("%s must be a versioned type", kind)
			}
			if len(gvks) != 1 || gvks[0] != GroupVersion.WithKind(kind) {
				t.Errorf("ObjectKinds = %v, want exactly %s", gvks, GroupVersion.WithKind(kind))
			}

			created, err := scheme.New(GroupVersion.WithKind(kind))
			if err != nil {
				t.Fatalf("scheme.New: %v", err)
			}
			if got, want := reflect.TypeOf(created), reflect.TypeOf(obj); got != want {
				t.Errorf("scheme.New returned %v, want %v", got, want)
			}
		})
	}

	registered := scheme.KnownTypes(GroupVersion)
	for kind := range objects {
		if _, ok := registered[kind]; !ok {
			t.Errorf("kind %s is not registered for %s", kind, GroupVersion)
		}
	}
}

// List/watch calls for the group need the meta/v1 option types in its
// group-version, which metav1.AddToGroupVersion registers.
func TestAddToScheme_RegistersMetaOptionTypes(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	for _, kind := range []string{"ListOptions", "GetOptions", "DeleteOptions", "CreateOptions", "UpdateOptions", "PatchOptions", "WatchEvent"} {
		if !scheme.Recognizes(GroupVersion.WithKind(kind)) {
			t.Errorf("%s is not registered in %s", kind, GroupVersion)
		}
	}
	// Status is what the API server answers errors with; it is registered as
	// an unversioned type.
	if !scheme.Recognizes(metav1.Unversioned.WithKind("Status")) {
		t.Error("meta/v1 Status is not registered")
	}
}

func TestAddToScheme_IsIdempotent(t *testing.T) {
	scheme := runtime.NewScheme()
	for i := range 2 {
		if err := AddToScheme(scheme); err != nil {
			t.Fatalf("AddToScheme call %d: %v", i+1, err)
		}
	}
}
