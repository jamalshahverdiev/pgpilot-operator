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
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	pgpilotv1 "github.com/jamalshahverdiev/pgpilot-operator/api/v1"
)

var yamlDocSeparator = regexp.MustCompile(`(?m)^---\s*$`)

// pgpilotObjects returns every pgpilot.io object found in a YAML text.
func pgpilotObjects(text, source string) []*unstructured.Unstructured {
	GinkgoHelper()
	var out []*unstructured.Unstructured
	for _, doc := range yamlDocSeparator.Split(text, -1) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		obj := map[string]any{}
		Expect(yaml.Unmarshal([]byte(doc), &obj)).To(Succeed(), "parse a document of %s", source)
		u := &unstructured.Unstructured{Object: obj}
		if u.GroupVersionKind().Group != pgpilotv1.GroupVersion.Group {
			continue
		}
		out = append(out, u)
	}
	return out
}

// yamlBlocks returns the fenced yaml code blocks of a Markdown text.
func yamlBlocks(markdown string) []string {
	matches := regexp.MustCompile("(?s)```ya?ml\n(.*?)```").FindAllStringSubmatch(markdown, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

// The manifests we hand to users have to pass the validation we ship: a
// sample that the API server rejects is the first thing a new user meets.
var _ = Describe("Shipped manifests", func() {
	root := filepath.Join("..", "..")
	var ns string

	BeforeEach(func() {
		created := &corev1.Namespace{}
		created.GenerateName = "test-samples-"
		Expect(k8sClient.Create(context.TODO(), created)).To(Succeed())
		ns = created.Name
	})

	apply := func(objs []*unstructured.Unstructured, source string) {
		GinkgoHelper()
		for _, obj := range objs {
			obj.SetNamespace(ns)
			Expect(k8sClient.Create(context.TODO(), obj)).To(Succeed(),
				"%s %q from %s must be accepted by the API server", obj.GetKind(), obj.GetName(), source)
		}
	}

	It("accepts every sample under config/samples", func() {
		files, err := filepath.Glob(filepath.Join(root, "config", "samples", "pgpilot_v1_*.yaml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(files).NotTo(BeEmpty(), "no samples found")

		total := 0
		for _, f := range files {
			data, err := os.ReadFile(f)
			Expect(err).NotTo(HaveOccurred())
			objs := pgpilotObjects(string(data), f)
			Expect(objs).NotTo(BeEmpty(), "%s contains no pgpilot.io object", f)
			apply(objs, f)
			total += len(objs)
		}
		Expect(total).To(BeNumerically(">=", 3), "expected the monitor, inline-credentials and library samples")
	})

	// Complete manifests in the documentation are copied by users as they
	// are. Fragments (a block without apiVersion/kind) are not objects and
	// are skipped by pgpilotObjects.
	DescribeTable("accepts the complete manifests shown in the documentation",
		func(doc string) {
			data, err := os.ReadFile(filepath.Join(root, doc))
			Expect(err).NotTo(HaveOccurred())

			total := 0
			for i, block := range yamlBlocks(string(data)) {
				objs := pgpilotObjects(block, doc)
				// Different blocks of one page reuse names to show an
				// object evolving; only the first of each is created.
				var fresh []*unstructured.Unstructured
				for _, obj := range objs {
					existing := obj.DeepCopy()
					existing.SetNamespace(ns)
					if err := k8sClient.Get(context.TODO(),
						client.ObjectKeyFromObject(existing), existing); err == nil {
						// Already created from an earlier block: validate
						// this version as an update instead.
						obj.SetNamespace(ns)
						obj.SetResourceVersion(existing.GetResourceVersion())
						Expect(k8sClient.Update(context.TODO(), obj)).To(Succeed(),
							"block %d of %s: %s %q must be accepted as an update", i, doc, obj.GetKind(), obj.GetName())
						total++
						continue
					}
					fresh = append(fresh, obj)
				}
				apply(fresh, doc)
				total += len(fresh)
			}
			Expect(total).To(BeNumerically(">", 0), "%s shows no complete pgpilot.io manifest", doc)
		},
		Entry("README", "README.md"),
		Entry("quickstart", filepath.Join("docs", "quickstart.md")),
	)
})
