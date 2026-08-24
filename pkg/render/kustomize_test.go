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

package render

import (
	"testing"
)

func TestNormalizeJSONTypesConvertsIntToInt64(t *testing.T) {
	obj := map[string]any{
		"replicas": 3,
		"nested": map[string]any{
			"port": 9004,
		},
		"list": []any{1, 2, map[string]any{"n": 5}},
	}

	normalizeJSONTypes(obj)

	if _, ok := obj["replicas"].(int64); !ok {
		t.Fatalf("replicas = %T, want int64", obj["replicas"])
	}

	nested, ok := obj["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested = %T, want map[string]any", obj["nested"])
	}
	if _, ok := nested["port"].(int64); !ok {
		t.Fatalf("nested.port = %T, want int64", nested["port"])
	}

	list, ok := obj["list"].([]any)
	if !ok {
		t.Fatalf("list = %T, want []any", obj["list"])
	}
	if _, ok := list[0].(int64); !ok {
		t.Fatalf("list[0] = %T, want int64", list[0])
	}
	listItem, ok := list[2].(map[string]any)
	if !ok {
		t.Fatalf("list[2] = %T, want map[string]any", list[2])
	}
	if _, ok := listItem["n"].(int64); !ok {
		t.Fatalf("list[2].n = %T, want int64", listItem["n"])
	}
}

// TestRenderKustomizeBuildsVendoredOverlay is a smoke test against the real
// vendored manifest tree (hack/scripts/get-manifests.sh). It is skipped, not
// failed, when the manifests have not been fetched yet (e.g. a fresh clone
// before "make get-manifests").
func TestRenderKustomizeBuildsVendoredOverlay(t *testing.T) {
	const manifestPath = "../../config/manifests/praxis-extproc/overlays/odh"

	resources, err := Build(manifestPath)
	if err != nil {
		t.Skipf("vendored manifests not present at %s; run hack/scripts/get-manifests.sh first: %v", manifestPath, err)
	}

	wantKinds := map[string]int{
		"ServiceAccount":     1,
		"ClusterRole":        1,
		"ClusterRoleBinding": 1,
		"ConfigMap":          1,
		"Service":            2,
		"Deployment":         2,
		"DestinationRule":    2,
		"EnvoyFilter":        1,
		"NetworkPolicy":      1,
	}
	gotKinds := map[string]int{}
	for _, r := range resources {
		gotKinds[r.GetKind()]++
	}
	for kind, want := range wantKinds {
		if gotKinds[kind] != want {
			t.Errorf("kind %s: got %d resources, want %d (full count map: %v)", kind, gotKinds[kind], want, gotKinds)
		}
	}
}
