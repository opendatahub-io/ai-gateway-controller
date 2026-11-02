package tenant

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/opendatahub-io/ai-gateway-controller/pkg/envelope"
)

func renderCredentialRevision(t *testing.T, secret string) (corev1.PodTemplateSpec, string) {
	t.Helper()
	resources := []unstructured.Unstructured{
		{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "payload-processing-external-model-plugins", "namespace": "tenant"},
			"data":     map[string]any{"extproc.yaml": "old"},
		}},
		{Object: map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": "payload-processing-external-model", "namespace": "tenant"},
			"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
				"volumes": []any{map[string]any{
					"name": "plugins-config-volume", "configMap": map[string]any{"name": "payload-processing-external-model-plugins"},
				}},
				"containers": []any{map[string]any{"name": "payload-processing", "volumeMounts": []any{}}},
			}}},
		}},
		externalModelEnvoyFilterFixtureForTenant("", "ingress"),
	}
	candidates := []envelope.Candidate{{
		Cluster: "provider", StableID: envelope.CandidateStableID("model", "provider"),
		Credential: &envelope.Credential{
			Strategy: "bearer_token", SecretRef: envelope.SecretRef{Name: secret, Namespace: "tenant", Key: "api-key"},
		},
	}}
	if err := configureExternalModelExtProc(resources, "", "tenant", candidates, false); err != nil {
		t.Fatal(err)
	}
	template, _, err := unstructured.NestedMap(resources[1].Object, "spec", "template")
	if err != nil {
		t.Fatal(err)
	}
	var pod corev1.PodTemplateSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(template, &pod); err != nil {
		t.Fatal(err)
	}
	config, _, err := unstructured.NestedString(resources[0].Object, "data", "extproc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return pod, config
}

// Kubelet's ConfigMap cache and the Deployment watch can observe updates in
// either order. Resolve the config source of each pod against the other
// revision's cached ConfigMap, then check its real Secret projection paths.
func TestExternalModelStartupConfigCannotSkewFromProjectedCredentials(t *testing.T) {
	oldPod, oldConfig := renderCredentialRevision(t, "credential-a")
	newPod, newConfig := renderCredentialRevision(t, "credential-b")
	for _, tc := range []struct {
		name   string
		pod    corev1.PodTemplateSpec
		cached string
	}{
		{"old pod sees newer ConfigMap", oldPod, newConfig},
		{"new pod sees older cached ConfigMap", newPod, oldConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := ""
			projected := map[string]bool{}
			for _, volume := range tc.pod.Spec.Volumes {
				if volume.Name == "plugins-config-volume" {
					switch {
					case volume.ConfigMap != nil:
						config = tc.cached
					case volume.DownwardAPI != nil:
						items := volume.DownwardAPI.Items
						if len(items) != 1 || items[0].Path != "extproc.yaml" || items[0].FieldRef == nil ||
							items[0].FieldRef.FieldPath != "metadata.annotations['"+extprocConfigSnapshotAnnotation+"']" {
							t.Fatalf("unexpected config snapshot projection: %#v", items)
						}
						config = tc.pod.Annotations[extprocConfigSnapshotAnnotation]
					}
				}
				if volume.Projected != nil {
					for _, source := range volume.Projected.Sources {
						if source.Secret != nil {
							for _, item := range source.Secret.Items {
								projected[extprocCredentialDir+"/"+item.Path] = true
							}
						}
					}
				}
			}
			var parsed struct {
				Chains []struct {
					Filters []struct {
						Name        string `json:"filter"`
						Credentials []struct {
							File string `json:"file"`
						} `json:"credentials"`
					} `json:"filters"`
				} `json:"filter_chains"`
			}
			if err := yaml.Unmarshal([]byte(config), &parsed); err != nil {
				t.Fatal(err)
			}
			checked := 0
			for _, chain := range parsed.Chains {
				for _, filter := range chain.Filters {
					if filter.Name == "credential_inject" {
						for _, credential := range filter.Credentials {
							checked++
							if !projected[credential.File] {
								t.Fatalf("startup configuration references %q, but this pod does not project that credential", credential.File)
							}
						}
					}
				}
			}
			if checked != 1 {
				t.Fatalf("checked %d credential references, want 1", checked)
			}
		})
	}
}

func TestExternalModelConfigSnapshotRejectsOversizedPodAnnotations(t *testing.T) {
	deployment := &unstructured.Unstructured{Object: map[string]any{}}
	err := snapshotExternalModelConfig(deployment, strings.Repeat("x", apivalidation.TotalAnnotationSizeLimitB))
	if err == nil || !strings.Contains(err.Error(), "annotation size limit") {
		t.Fatalf("oversized snapshot error = %v, want annotation-size rejection before apply", err)
	}
}
