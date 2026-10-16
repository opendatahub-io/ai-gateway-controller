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
	if err := configureExternalModelExtProc(resources, "tenant-a", candidates); err != nil {
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
	if err := configureExternalModelExtProc(resources, "tenant-a", nil); err != nil {
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

func TestSharedExtProcConfigDefaultOff(t *testing.T) {
	config := sharedExtProcConfig(false)
	for _, want := range []string{"filter: request_id", "name: ipp", "allow_unbounded_body: true"} {
		if !strings.Contains(config, want) {
			t.Fatalf("default shared config missing %q:\n%s", want, config)
		}
	}
	for _, forbidden := range []string{
		"anthropic_messages_to_chat_completions",
		"path_rewrite",
		"/v1/messages",
	} {
		if strings.Contains(config, forbidden) {
			t.Fatalf("default shared config must not contain %q:\n%s", forbidden, config)
		}
	}
}

func TestSharedExtProcConfigAPITranslation(t *testing.T) {
	config := sharedExtProcConfig(true)
	for _, want := range []string{
		"filter: request_id",
		"filter: anthropic_messages_to_chat_completions",
		"filter: anthropic_messages_to_chat_completions_stream",
		"filter: path_rewrite",
		"path: \"/v1/messages\"",
		"pattern: \"^/v1/messages$\"",
		"replacement: \"/v1/chat/completions\"",
		"content-type: \"text/event-stream\"",
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("apiTranslation shared config missing %q:\n%s", want, config)
		}
	}
}

func TestConfigureSharedExtProcRewritesConfigAndHashesDeployment(t *testing.T) {
	tenantID := "redteam"
	gatewayNS := "openshift-ingress"
	resources := []unstructured.Unstructured{
		{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{
				"name": PayloadProcessingPluginsConfigMapForTenant(tenantID), "namespace": gatewayNS,
			},
			"data": map[string]any{
				"pre-extproc.yaml": "pre",
				"extproc.yaml":     "old-shared",
			},
		}},
		{Object: map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{
				"name": PayloadProcessingDeploymentName(tenantID), "namespace": gatewayNS,
			},
			"spec": map[string]any{"template": map[string]any{
				"metadata": map[string]any{"annotations": map[string]any{}},
				"spec":     map[string]any{"containers": []any{map[string]any{"name": "payload-processing"}}},
			}},
		}},
		// ExternalModel CM must stay untouched.
		{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{
				"name": PayloadProcessingExternalModelPluginsConfigMapForTenant(tenantID), "namespace": "tenant-a",
			},
			"data": map[string]any{"extproc.yaml": "external-old"},
		}},
	}
	if err := configureSharedExtProc(resources, tenantID, gatewayNS, true); err != nil {
		t.Fatal(err)
	}
	data, _, err := unstructured.NestedStringMap(resources[0].Object, "data")
	if err != nil {
		t.Fatal(err)
	}
	if data["pre-extproc.yaml"] != "pre" {
		t.Fatalf("pre-extproc.yaml was rewritten: %q", data["pre-extproc.yaml"])
	}
	if !strings.Contains(data["extproc.yaml"], "anthropic_messages_to_chat_completions") {
		t.Fatalf("shared extproc.yaml missing Anthropic filters:\n%s", data["extproc.yaml"])
	}
	hash, found, err := unstructured.NestedString(resources[1].Object, "spec", "template", "metadata", "annotations", extprocConfigHashAnnotation)
	if err != nil || !found || hash == "" {
		t.Fatalf("shared config hash missing: found=%v err=%v hash=%q", found, err, hash)
	}
	external, _, err := unstructured.NestedStringMap(resources[2].Object, "data")
	if err != nil {
		t.Fatal(err)
	}
	if external["extproc.yaml"] != "external-old" {
		t.Fatalf("ExternalModel ConfigMap was rewritten: %q", external["extproc.yaml"])
	}
}
