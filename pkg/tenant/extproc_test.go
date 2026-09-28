package tenant

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/opendatahub-io/ai-gateway-controller/pkg/envelope"
	"github.com/opendatahub-io/ai-gateway-controller/pkg/resolver"
)

func TestConfigureExternalModelExtProcProjectsReferencesAndTrustedHandoff(t *testing.T) {
	resources := []unstructured.Unstructured{
		{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "payload-processing-external-model-plugins-redteam", "namespace": "tenant-a"},
			"data": map[string]any{"extproc.yaml": "old", "pre-extproc.yaml": "pre"},
		}},
		{Object: map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "payload-processing-external-model-redteam", "namespace": "tenant-a"},
			"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"volumes":    []any{map[string]any{"name": "routing-overlay-volume"}},
				"containers": []any{map[string]any{"name": "payload-processing", "volumeMounts": []any{map[string]any{"name": "routing-overlay-volume"}}}},
			}}},
		}},
	}
	candidates := []envelope.Candidate{
		{Cluster: "provider-provider-b", StableID: "provider-provider-b", Credential: &envelope.Credential{Strategy: "bearer_token", SecretRef: envelope.SecretRef{Name: "b-secret", Namespace: "tenant-a", Key: "api-key"}}},
		{Cluster: "provider-provider-a", StableID: "provider-provider-a", Credential: &envelope.Credential{Strategy: "bearer_token", SecretRef: envelope.SecretRef{Name: "a-secret", Namespace: "tenant-a", Key: "api-key"}}},
	}
	if err := configureExternalModelExtProc(resources, "tenant-a", candidates,
		"https://files.example.test/v1/files?tenant=redteam&scope=file",
		"https://vectors.example.test/v1/vector_stores"); err != nil {
		t.Fatal(err)
	}
	data, found, err := unstructured.NestedStringMap(resources[0].Object, "data")
	if err != nil || !found {
		t.Fatalf("ConfigMap data missing: found=%v err=%v", found, err)
	}
	config, found := data["extproc.yaml"]
	if !found {
		t.Fatal("post-auth extproc.yaml is missing")
	}
	for _, want := range []string{
		"filter: intelligent_route",
		"name: bypass-irr",
		"path_prefix: \"/tenant-a/v1/files\"\n                        cluster: \"files-api\"",
		"path_prefix: \"/tenant-a/v1/vector_stores\"\n                        cluster: \"vector-stores-backend\"",
		"pattern: \"^/tenant-a(/.*)$\"",
		"pattern: \"^.*(/v1/.*)$\"\n          replacement: \"$1\"",
		"name: \"files-api\"\n                        endpoints: [\"files.example.test\"]",
		"name: \"vector-stores-backend\"\n                        endpoints: [\"vectors.example.test\"]",
		"filter: header_copy\n        mappings:\n          - from: x-maas-username\n            to: x-user-id\n          - from: x-maas-subscription\n            to: x-tenant-id",
		"filter: openai_file_resolve",
		"allow_pre_security_callout: true",
		"files_api_url: \"https://files.example.test/v1/files?tenant=redteam&scope=file\"",
		"filter: openai_file_search_callout",
		"vector_store_url: \"https://vectors.example.test/v1/vector_stores\"",
		"filter: credential_inject",
		"provider_hop_clusters: [\"provider-provider-a\", \"provider-provider-b\"]",
		"strategy: bearer_token",
		"a-secret",
		"b-secret",
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("post-auth config missing %q:\n%s", want, config)
		}
	}
	if strings.Index(config, "name: bypass-irr") > strings.Index(config, "filter: intelligent_route") {
		t.Fatalf("bypass branch must run before intelligent_route:\n%s", config)
	}
	if strings.Index(config, "pattern: \"^.*(/v1/.*)$\"") > strings.Index(config, "filter: intelligent_route") {
		t.Fatalf("general path rewrite must run before intelligent_route:\n%s", config)
	}
	if strings.Contains(config, "secret-value") {
		t.Fatal("secret value appeared in post-auth configuration")
	}
	volumes, _, err := unstructured.NestedSlice(resources[1].Object, "spec", "template", "spec", "volumes")
	if err != nil {
		t.Fatal(err)
	}
	if len(volumes) != 2 {
		t.Fatalf("volumes = %d, want routing plus projected credentials", len(volumes))
	}
	annotations, _, err := unstructured.NestedStringMap(resources[1].Object, "spec", "template", "metadata", "annotations")
	if err != nil || annotations["ai-gateway-controller.opendatahub.io/extproc-config-sha256"] == "" {
		t.Fatalf("post-auth config hash missing: %#v (err=%v)", annotations, err)
	}
}

func TestResponsesConfigUsesOwnedBackendAndComposesStoreBeforeIRR(t *testing.T) {
	backend := &ogxBackend{namespace: "applications", service: "ogx-service", port: 8321, url: "http://ogx-service.applications.svc.cluster.local:8321"}
	route := resolver.Route{Model: "model", Provider: "openai", ProviderType: "openai", Endpoint: "api.openai.com:443", APIFormat: "openai-responses"}
	config, err := responsesConfig("tenant-a", route, backend, []envelope.Candidate{{Cluster: "provider-openai"}}, []extprocCredential{{name: "openai-key", namespace: "tenant-a", key: "api-key", strategy: "bearer_token", file: "/etc/praxis/credentials/openai-key/api-key"}})
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := yaml.UnmarshalStrict([]byte(config), &parsed); err != nil {
		t.Fatalf("invalid generated YAML: %v\n%s", err, config)
	}
	for _, expected := range []string{"filter: state_owner", "filter: openai_conversations", "filter: openai_response_store", "filter: openai_file_resolve", "filter: openai_file_search_callout", "filter: openai_stream_events", "filter: openai_agentic_loop", "filter: iterative_request_router", "openai_responses_proxy", backend.url, "api.openai.com", "provider-openai"} {
		if !strings.Contains(config, expected) {
			t.Fatalf("missing %q from generated config", expected)
		}
	}
	if strings.Index(config, "filter: openai_response_store") > strings.Index(config, "filter: iterative_request_router") {
		t.Fatal("store must run on response after IRR composition")
	}
	for _, api := range []string{"files", "vector_stores", "prompts", "embeddings"} {
		if !strings.Contains(config, "path_prefix: /v1/"+api) || !strings.Contains(config, "name: bypass-"+api) {
			t.Fatalf("OGX %s requests must bypass IRR", api)
		}
	}
	if strings.Contains(config, "path_prefix: /v1/chat/completions") {
		t.Fatal("Chat Completions must continue through the existing model path")
	}
	if strings.Contains(config, "BRAVE_API_KEY") || strings.Contains(config, "x-user-brave-key\n") {
		t.Fatal("do not enable shared or caller-supplied Brave credentials")
	}
}

func TestResponsesConfigLoadsInExtProc(t *testing.T) {
	bin := os.Getenv("PRAXIS_EXTPROC_BIN")
	if bin == "" {
		t.Skip("set PRAXIS_EXTPROC_BIN to validate against a responses-full ExtProc binary")
	}
	dir := t.TempDir()
	overlay := filepath.Join(dir, "routing-overlay.json")
	if err := os.WriteFile(overlay, []byte(`{"local_site":"site-a","candidates":[{"kind":"inference_model","name":"model","site":"site-a","cluster":"provider-openai","fresh":true}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	backend := &ogxBackend{namespace: "applications", service: "ogx-service", port: 8321, url: "http://ogx-service.applications.svc.cluster.local:8321"}
	route := resolver.Route{Model: "model", Provider: "openai", ProviderType: "openai", Endpoint: "api.openai.com:443", APIFormat: "openai-responses"}
	config, err := responsesConfig("tenant-a", route, backend, []envelope.Candidate{{Cluster: "provider-openai"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	config = strings.ReplaceAll(config, "/etc/praxis/routing/routing-overlay.json", overlay)
	path := filepath.Join(dir, "extproc.yaml")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, "--validate", "-c", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ExtProc rejected generated config: %v\n%s", err, out)
	}
}

func TestExternalModelConfigOmitsCredentialFilterWithoutReferences(t *testing.T) {
	resources := []unstructured.Unstructured{
		{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "payload-processing-external-model-plugins", "namespace": "tenant-a"},
			"data": map[string]any{"pre-extproc.yaml": "pre", "extproc.yaml": "old"},
		}},
		{Object: map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "payload-processing-external-model", "namespace": "tenant-a"},
			"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"volumes": []any{}, "containers": []any{map[string]any{"name": "payload-processing", "volumeMounts": []any{}}},
			}}},
		}},
	}
	if err := configureExternalModelExtProc(resources, "tenant-a", nil, "", ""); err != nil {
		t.Fatal(err)
	}
	data, _, err := unstructured.NestedStringMap(resources[0].Object, "data")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(data["extproc.yaml"], "credential_inject") {
		t.Fatalf("credential_inject must be omitted without credential references:\n%s", data["extproc.yaml"])
	}
	if strings.Contains(data["extproc.yaml"], "openai_file_resolve") || strings.Contains(data["extproc.yaml"], "openai_file_search_callout") {
		t.Fatalf("agentic file filters must be omitted without URLs:\n%s", data["extproc.yaml"])
	}
	hash, found, err := unstructured.NestedString(resources[1].Object, "spec", "template", "metadata", "annotations", "ai-gateway-controller.opendatahub.io/extproc-config-sha256")
	if err != nil || !found || hash == "" {
		t.Fatal("config hash is empty")
	}
}

func TestExtprocPostAuthConfigIncludesAgenticFiltersIndependently(t *testing.T) {
	tests := []struct {
		name            string
		filesURL        string
		vectorStoresURL string
		want            string
		omit            string
	}{
		{
			name:     "files only",
			filesURL: "https://files.example.test/v1/files",
			want:     "files_api_url: \"https://files.example.test/v1/files\"",
			omit:     "bypass-irr-vector-stores-backend",
		},
		{
			name:            "vector stores only",
			vectorStoresURL: "https://vectors.example.test/v1/vector_stores",
			want:            "vector_store_url: \"https://vectors.example.test/v1/vector_stores\"",
			omit:            "bypass-irr-files-api",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := extprocPostAuthConfig("tenant-a", nil, nil, tt.filesURL, tt.vectorStoresURL)
			if !strings.Contains(config, tt.want) || strings.Contains(config, tt.omit) {
				t.Fatalf("unexpected generated config:\n%s", config)
			}
			if !strings.Contains(config, "name: bypass-irr-") {
				t.Fatalf("matching bypass branch is missing:\n%s", config)
			}
		})
	}
}

func TestExtprocCredentialsRejectCrossNamespaceProvider(t *testing.T) {
	candidates := []envelope.Candidate{{StableID: "provider-provider", Credential: &envelope.Credential{Strategy: "bearer_token", SecretRef: envelope.SecretRef{Name: "secret", Namespace: "other", Key: "api-key"}}}}
	if _, err := extprocCredentials("tenant-a", candidates); err == nil || !strings.Contains(err.Error(), "cross-namespace") {
		t.Fatalf("expected cross-namespace rejection, got %v", err)
	}
}

func TestExtprocCredentialsRejectsUnsupportedAuth(t *testing.T) {
	candidates := []envelope.Candidate{{StableID: "provider-provider", Credential: &envelope.Credential{Strategy: "oauth2", SecretRef: envelope.SecretRef{Name: "secret", Namespace: "tenant-a", Key: "api-key"}}}}
	if _, err := extprocCredentials("tenant-a", candidates); err == nil || !strings.Contains(err.Error(), "unsupported ExtProc credential strategy") {
		t.Fatalf("expected unsupported-auth rejection, got %v", err)
	}
}
