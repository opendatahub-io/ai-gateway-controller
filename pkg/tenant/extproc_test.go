package tenant

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/opendatahub-io/ai-gateway-controller/pkg/envelope"
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
		{
			Cluster:  "provider-provider-b",
			StableID: envelope.CandidateStableID("model-b", "provider-b"),
			Credential: &envelope.Credential{
				Strategy:  "bearer_token",
				SecretRef: envelope.SecretRef{Name: "b-secret", Namespace: "tenant-a", Key: "api-key"},
			},
		},
		{
			Cluster:  "provider-provider-a",
			StableID: envelope.CandidateStableID("model-a", "provider-a"),
			Credential: &envelope.Credential{
				Strategy:  "bearer_token",
				SecretRef: envelope.SecretRef{Name: "a-secret", Namespace: "tenant-a", Key: "api-key"},
			},
		},
	}
	if err := configureExternalModelExtProc(resources, "tenant-a", "", candidates, nil); err != nil {
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
	for _, want := range []string{"filter: intelligent_route", "filter: credential_inject", "provider_hop_clusters: [\"provider-provider-a\", \"provider-provider-b\"]", "strategy: bearer_token", "a-secret", "b-secret"} {
		if !strings.Contains(config, want) {
			t.Fatalf("post-auth config missing %q:\n%s", want, config)
		}
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
	if err := configureExternalModelExtProc(resources, "tenant-a", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	data, _, err := unstructured.NestedStringMap(resources[0].Object, "data")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(data["extproc.yaml"], "credential_inject") {
		t.Fatalf("credential_inject must be omitted without credential references:\n%s", data["extproc.yaml"])
	}
	hash, found, err := unstructured.NestedString(resources[1].Object, "spec", "template", "metadata", "annotations", "ai-gateway-controller.opendatahub.io/extproc-config-sha256")
	if err != nil || !found || hash == "" {
		t.Fatal("config hash is empty")
	}
}

func TestExternalModelConfigWiresMeteringWhenConfigured(t *testing.T) {
	credentials := []extprocCredential{}
	config, err := extprocPostAuthConfig(nil, credentials, "http://metering.svc:8080", []string{"anthropic", "openai"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"filter: identity_header_guard",
		"filter: external_metering",
		"metering_url: http://metering.svc:8080",
		"source: maas-gateway",
		"identity_header_prefix: x-maas-",
		"identity_metadata_namespace: identity",
		"prefix: x-maas-",
		"metadata_namespace: identity",
		"filter: token_count",
		"provider: anthropic",
		"provider: openai",
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("metered post-auth config missing %q:\n%s", want, config)
		}
	}
	// identity_header_guard must capture/strip identity before routing forwards
	// the request upstream, and token_count must sit after external_metering (so
	// response-path extraction precedes the metering read) and before routing.
	guard := strings.Index(config, "identity_header_guard")
	meter := strings.Index(config, "external_metering")
	token := strings.Index(config, "token_count")
	route := strings.Index(config, "intelligent_route")
	if guard >= meter || meter >= token || token >= route {
		t.Fatalf("filter order must be guard<meter<token_count<route (guard=%d meter=%d token=%d route=%d):\n%s", guard, meter, token, route, config)
	}
}

func TestExternalModelConfigOmitsTokenCountWhenMeteringUnset(t *testing.T) {
	config, err := extprocPostAuthConfig(nil, nil, "", []string{"openai"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(config, "token_count") {
		t.Fatalf("token_count must be omitted when metering is off:\n%s", config)
	}
}

func TestExternalModelConfigOmitsMeteringWhenUnset(t *testing.T) {
	config, err := extprocPostAuthConfig(nil, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(config, "external_metering") || strings.Contains(config, "identity_header_guard") {
		t.Fatalf("metering filters must be omitted when no metering URL is configured:\n%s", config)
	}
}

func TestConfigureBaseExtProcMeteringInjectsIntoSharedChain(t *testing.T) {
	baseConfig := "server:\n  grpc_address: \"0.0.0.0:9004\"\n\nfilter_chains:\n  - name: ipp\n    filters:\n      - filter: request_id\n\ninsecure_options:\n  allow_unbounded_body: true\n"
	resources := []unstructured.Unstructured{
		{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": PayloadProcessingPluginsConfigMapName, "namespace": "openshift-ingress"},
			"data":     map[string]any{"extproc.yaml": baseConfig, "pre-extproc.yaml": "pre"},
		}},
		{Object: map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": PayloadProcessingName, "namespace": "openshift-ingress"},
			"spec":     map[string]any{"template": map[string]any{"spec": map[string]any{}}},
		}},
	}
	if err := configureBaseExtProcMetering(resources, "", "http://metering.svc:8080"); err != nil {
		t.Fatal(err)
	}
	data, _, err := unstructured.NestedStringMap(resources[0].Object, "data")
	if err != nil {
		t.Fatal(err)
	}
	config := data["extproc.yaml"]
	for _, want := range []string{"external_metering", "identity_header_guard", "request_id", "http://metering.svc:8080", "maas-gateway", "token_count", "provider: openai"} {
		if !strings.Contains(config, want) {
			t.Fatalf("shared chain missing %q:\n%s", want, config)
		}
	}
	// pre-extproc.yaml (the BBR chain) must be left untouched.
	if data["pre-extproc.yaml"] != "pre" {
		t.Fatalf("pre-extproc.yaml must not be modified, got %q", data["pre-extproc.yaml"])
	}
	// The guard must be prepended ahead of the pre-existing request_id filter,
	// and token_count must follow external_metering so the response-path read
	// sees the extracted counts.
	if strings.Index(config, "identity_header_guard") > strings.Index(config, "request_id") {
		t.Fatalf("metering must precede request_id in the shared chain:\n%s", config)
	}
	if meter, token := strings.Index(config, "external_metering"), strings.Index(config, "token_count"); meter >= token {
		t.Fatalf("token_count must follow external_metering in the shared chain (meter=%d token=%d):\n%s", meter, token, config)
	}
	hash, found, err := unstructured.NestedString(resources[1].Object, "spec", "template", "metadata", "annotations", "ai-gateway-controller.opendatahub.io/extproc-config-sha256")
	if err != nil || !found || hash == "" {
		t.Fatalf("shared ExtProc config hash missing: found=%v err=%v", found, err)
	}
}

func TestConfigureBaseExtProcMeteringNoopWhenUnset(t *testing.T) {
	baseConfig := "filter_chains:\n  - name: ipp\n    filters:\n      - filter: request_id\n"
	resources := []unstructured.Unstructured{
		{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": PayloadProcessingPluginsConfigMapName, "namespace": "openshift-ingress"},
			"data":     map[string]any{"extproc.yaml": baseConfig},
		}},
		{Object: map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": PayloadProcessingName, "namespace": "openshift-ingress"},
			"spec":     map[string]any{"template": map[string]any{"spec": map[string]any{}}},
		}},
	}
	if err := configureBaseExtProcMetering(resources, "", ""); err != nil {
		t.Fatal(err)
	}
	data, _, _ := unstructured.NestedStringMap(resources[0].Object, "data")
	if data["extproc.yaml"] != baseConfig {
		t.Fatalf("config must be unchanged when metering is off, got:\n%s", data["extproc.yaml"])
	}
	if _, found, _ := unstructured.NestedString(resources[1].Object, "spec", "template", "metadata", "annotations", "ai-gateway-controller.opendatahub.io/extproc-config-sha256"); found {
		t.Fatal("no config hash should be stamped when metering is off")
	}
}

func TestExtprocCredentialsRejectCrossNamespaceProvider(t *testing.T) {
	candidates := []envelope.Candidate{{
		StableID: envelope.CandidateStableID("model", "provider"),
		Credential: &envelope.Credential{
			Strategy:  "bearer_token",
			SecretRef: envelope.SecretRef{Name: "secret", Namespace: "other", Key: "api-key"},
		},
	}}
	if _, err := extprocCredentials("tenant-a", candidates); err == nil || !strings.Contains(err.Error(), "cross-namespace") {
		t.Fatalf("expected cross-namespace rejection, got %v", err)
	}
}

func TestExtprocCredentialsRejectsUnsupportedAuth(t *testing.T) {
	candidates := []envelope.Candidate{{
		StableID: envelope.CandidateStableID("model", "provider"),
		Credential: &envelope.Credential{
			Strategy:  "oauth2",
			SecretRef: envelope.SecretRef{Name: "secret", Namespace: "tenant-a", Key: "api-key"},
		},
	}}
	if _, err := extprocCredentials("tenant-a", candidates); err == nil || !strings.Contains(err.Error(), "unsupported ExtProc credential strategy") {
		t.Fatalf("expected unsupported-auth rejection, got %v", err)
	}
}
