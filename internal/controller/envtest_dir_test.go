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

package controller

import (
	"os"
	"path/filepath"
	"testing"
)

// After a Kubernetes library upgrade bin/k8s holds the binaries of the old
// and the new version. Picking the first directory ran the suite against the
// old API server.
func TestNewestEnvTestBinaryDir(t *testing.T) {
	tests := []struct {
		name string
		dirs []string
		file string // a plain file to place next to the directories
		want string
	}{
		{name: "no directory at all", want: ""},
		{name: "single version", dirs: []string{"1.37.0-linux-amd64"}, want: "1.37.0-linux-amd64"},
		{
			name: "newest of several",
			dirs: []string{"1.35.0-linux-amd64", "1.37.0-linux-amd64", "1.36.2-linux-amd64"},
			want: "1.37.0-linux-amd64",
		},
		{
			name: "compared as numbers, not as text",
			dirs: []string{"1.9.0-linux-amd64", "1.10.0-linux-amd64"},
			want: "1.10.0-linux-amd64",
		},
		{
			name: "patch level decides",
			dirs: []string{"1.37.0-linux-amd64", "1.37.2-linux-amd64", "1.37.1-linux-amd64"},
			want: "1.37.2-linux-amd64",
		},
		{
			name: "directories that are not versions are ignored",
			dirs: []string{"tmp", "1.36.0-linux-amd64", "latest-linux-amd64"},
			want: "1.36.0-linux-amd64",
		},
		{name: "only unrelated directories", dirs: []string{"tmp", "cache"}, want: ""},
		{
			name: "plain files are ignored",
			dirs: []string{"1.36.0-linux-amd64"},
			file: "1.99.0-linux-amd64",
			want: "1.36.0-linux-amd64",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			for _, d := range tt.dirs {
				if err := os.Mkdir(filepath.Join(base, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tt.file != "" {
				if err := os.WriteFile(filepath.Join(base, tt.file), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}

			want := ""
			if tt.want != "" {
				want = filepath.Join(base, tt.want)
			}
			if got := newestEnvTestBinaryDir(base); got != want {
				t.Errorf("newestEnvTestBinaryDir = %q, want %q", got, want)
			}
		})
	}

	t.Run("missing base directory", func(t *testing.T) {
		if got := newestEnvTestBinaryDir(filepath.Join(t.TempDir(), "does-not-exist")); got != "" {
			t.Errorf("newestEnvTestBinaryDir = %q, want empty", got)
		}
	})
}

// `make test` picks the Kubernetes version of go.mod and exports it; a
// directory lying around under bin/k8s must not override that choice.
func TestEnvtestBinaryDir(t *testing.T) {
	base := t.TempDir()
	for _, d := range []string{"1.35.0-linux-amd64", "1.37.0-linux-amd64"} {
		if err := os.Mkdir(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	empty := t.TempDir()

	tests := []struct {
		name          string
		assetsFromEnv string
		basePath      string
		want          string
	}{
		{name: "variable set: leave the choice to envtest", assetsFromEnv: "/opt/envtest/1.28.3", basePath: base, want: ""},
		{name: "variable unset: newest local version", basePath: base, want: filepath.Join(base, "1.37.0-linux-amd64")},
		{name: "variable unset and nothing local", basePath: empty, want: ""},
		{name: "variable set and nothing local", assetsFromEnv: "/opt/envtest/1.28.3", basePath: empty, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := envtestBinaryDir(tt.assetsFromEnv, tt.basePath); got != tt.want {
				t.Errorf("envtestBinaryDir(%q, ...) = %q, want %q", tt.assetsFromEnv, got, tt.want)
			}
		})
	}
}
