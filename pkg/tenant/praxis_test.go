package tenant

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/inference/v1alpha1"
)

func TestStandalonePraxisResourcesProjectAndDeduplicateCredentials(t *testing.T) {
	providers := []v1alpha1.ExternalProvider{
		{ObjectMeta: metav1.ObjectMeta{Name: "provider-a", Namespace: "tenant-a"}, Spec: v1alpha1.ExternalProviderSpec{Endpoint: "a.example.com", Auth: v1alpha1.AuthConfig{SecretRef: v1alpha1.NameReference{Name: "shared"}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "provider-b", Namespace: "tenant-a"}, Spec: v1alpha1.ExternalProviderSpec{Endpoint: "b.example.com", Auth: v1alpha1.AuthConfig{SecretRef: v1alpha1.NameReference{Name: "shared"}}}},
	}
	resources, err := StandalonePraxisResources("tenant-a", "tenant-a", "praxis:test", "Never", providers)
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 4 {
		t.Fatalf("resource count = %d, want 4", len(resources))
	}
	deployment := findResource(resources, "Deployment", "praxis-tenant-a")
	volumes, found, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "volumes")
	if err != nil || !found {
		t.Fatalf("Praxis volumes missing: found=%v err=%v", found, err)
	}
	if len(volumes) != 3 {
		t.Fatalf("volume count = %d, want config/routing/credentials", len(volumes))
	}
	container, found, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	if err != nil || !found || len(container) != 1 {
		t.Fatalf("Praxis container missing: found=%v err=%v", found, err)
	}
	containerMap, ok := container[0].(map[string]any)
	if !ok {
		t.Fatalf("Praxis container has unexpected type %T", container[0])
	}
	if containerMap["imagePullPolicy"] != "Never" {
		t.Fatalf("image pull policy = %v, want Never", containerMap["imagePullPolicy"])
	}
	automount, found, err := unstructured.NestedBool(deployment.Object, "spec", "template", "spec", "automountServiceAccountToken")
	if err != nil || !found || automount {
		t.Fatalf("service-account token automount = %v, found=%v err=%v", automount, found, err)
	}
	projected, ok := volumes[2].(map[string]any)["projected"].(map[string]any)
	if !ok {
		t.Fatal("projected credentials volume missing or has unexpected type")
	}
	sources, found, err := unstructured.NestedSlice(projected, "sources")
	if err != nil || !found {
		t.Fatalf("projected Secret sources missing: found=%v err=%v", found, err)
	}
	if len(sources) != 1 {
		t.Fatalf("projected Secret source count = %d, want one deduplicated source", len(sources))
	}
	configMap := findResource(resources, "ConfigMap", "praxis-config-tenant-a")
	data, found, err := unstructured.NestedStringMap(configMap.Object, "data")
	if err != nil || !found {
		t.Fatalf("Praxis config data missing: found=%v err=%v", found, err)
	}
	config := data["config.yaml"]
	if strings.Contains(config, "secret-value") || !strings.Contains(config, "/etc/praxis/credentials/shared-") {
		t.Fatalf("config contains unexpected credential material or path: %s", config)
	}
}

func TestStandalonePraxisResourcesRejectCrossNamespaceProvider(t *testing.T) {
	providers := []v1alpha1.ExternalProvider{{ObjectMeta: metav1.ObjectMeta{Name: "provider", Namespace: "other"}}}
	if _, err := StandalonePraxisResources("tenant-a", "tenant-a", "praxis:test", "Never", providers); err == nil {
		t.Fatal("expected cross-namespace provider rejection")
	}
}

func findResource(resources []unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	for i := range resources {
		if resources[i].GetKind() == kind && resources[i].GetName() == name {
			return &resources[i]
		}
	}
	panic("resource not found")
}
