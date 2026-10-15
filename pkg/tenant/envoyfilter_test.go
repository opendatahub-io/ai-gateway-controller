package tenant

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/opendatahub-io/ai-gateway-controller/pkg/render"
)

const (
	eppFilterName    = "envoy.filters.http.ext_proc"
	routerFilterName = "envoy.filters.http.router"
)

// istioInferencePoolTypedConfig is Istio's static InferencePool filter
// (pilot/pkg/xds/filters). The praxis-extproc EnvoyFilter replaces Istio's
// filter with this copy, so per-route EPP overrides keep applying.
func istioInferencePoolTypedConfig() map[string]any {
	return map[string]any{
		"@type": "type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor",
		"grpc_service": map[string]any{
			"envoy_grpc": map[string]any{"cluster_name": "dummy"},
			"timeout":    "10s",
		},
		"failure_mode_allow": true,
		"processing_mode": map[string]any{
			"request_header_mode":  "SKIP",
			"response_header_mode": "SKIP",
		},
		"message_timeout": "1000s",
		"metadata_options": map[string]any{
			"forwarding_namespaces": map[string]any{"untyped": []any{"envoy.lb"}},
			"receiving_namespaces":  map[string]any{"untyped": []any{"envoy.lb"}},
		},
	}
}

func TestRenderedPayloadProcessingEnvoyFilterMovesEPPInFrontOfRouter(t *testing.T) {
	requireManifests(t)
	rendered, err := render.Build(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	resources := render.PostRender(rendered, render.Params{
		Namespace:        "gateway-system",
		GatewayName:      "gateway",
		Image:            "extproc:dev",
		MaaSAPIRouteName: ResourceName("maas-api-route", "tenant-a"),
	})
	resources, err = Rename(resources, "tenant-a", "gateway-system")
	if err != nil {
		t.Fatal(err)
	}

	httpFilters := httpFilterPatches(t, resources, PayloadProcessingEnvoyFilterName("tenant-a"))
	if len(httpFilters) < 2 {
		t.Fatalf("got %d HTTP_FILTER patches, want the EPP move as the last two", len(httpFilters))
	}
	remove, insert := httpFilters[len(httpFilters)-2], httpFilters[len(httpFilters)-1]
	for _, want := range []struct {
		patch             map[string]any
		operation, anchor string
	}{
		{remove, "REMOVE", eppFilterName},
		{insert, "INSERT_BEFORE", routerFilterName},
	} {
		if op, _, _ := unstructured.NestedString(want.patch, "patch", "operation"); op != want.operation {
			t.Errorf("EPP move operation = %q, want %q", op, want.operation)
		}
		if anchor, _, _ := unstructured.NestedString(want.patch, "match", "listener", "filterChain", "filter", "subFilter", "name"); anchor != want.anchor {
			t.Errorf("EPP move %s anchor = %q, want %q", want.operation, anchor, want.anchor)
		}
		if _, found, _ := unstructured.NestedFieldNoCopy(want.patch, "match", "routeConfiguration"); found {
			t.Errorf("EPP move %s must not carry a route match", want.operation)
		}
	}

	for i, patch := range httpFilters[:len(httpFilters)-1] {
		if name, _, _ := unstructured.NestedString(patch, "patch", "value", "name"); name == eppFilterName {
			t.Errorf("HTTP_FILTER patch %d inserts %s ahead of the EPP move", i, eppFilterName)
		}
	}
	if name, _, _ := unstructured.NestedString(insert, "patch", "value", "name"); name != eppFilterName {
		t.Errorf("inserted filter = %q, want %q: per-route EPP overrides are keyed on it", name, eppFilterName)
	}
	typedConfig, _, _ := unstructured.NestedFieldNoCopy(insert, "patch", "value", "typed_config")
	if !reflect.DeepEqual(typedConfig, istioInferencePoolTypedConfig()) {
		t.Errorf("inserted typed_config = %v, want Istio's static InferencePool filter %v", typedConfig, istioInferencePoolTypedConfig())
	}
}

func httpFilterPatches(t *testing.T, resources []unstructured.Unstructured, envoyFilterName string) []map[string]any {
	t.Helper()
	for i := range resources {
		if resources[i].GetKind() != "EnvoyFilter" || resources[i].GetName() != envoyFilterName {
			continue
		}
		configPatches, _, err := unstructured.NestedSlice(resources[i].Object, "spec", "configPatches")
		if err != nil {
			t.Fatal(err)
		}
		var patches []map[string]any
		for _, raw := range configPatches {
			if patch, ok := raw.(map[string]any); ok && patch["applyTo"] == "HTTP_FILTER" {
				patches = append(patches, patch)
			}
		}
		return patches
	}
	t.Fatalf("missing EnvoyFilter %s", envoyFilterName)
	return nil
}
