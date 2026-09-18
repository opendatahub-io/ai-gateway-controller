package tenant

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/inference/v1alpha1"
	"github.com/opendatahub-io/ai-gateway-controller/pkg/resolver"
)

func TestStandalonePraxisResourcesProjectAndDeduplicateCredentials(t *testing.T) {
	providers := []v1alpha1.ExternalProvider{
		{ObjectMeta: metav1.ObjectMeta{Name: "provider-a", Namespace: "tenant-a"}, Spec: v1alpha1.ExternalProviderSpec{Endpoint: "a.example.com", Auth: v1alpha1.AuthConfig{SecretRef: v1alpha1.NameReference{Name: "shared"}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "provider-b", Namespace: "tenant-a"}, Spec: v1alpha1.ExternalProviderSpec{Endpoint: "b.example.com", Auth: v1alpha1.AuthConfig{SecretRef: v1alpha1.NameReference{Name: "shared"}}}},
	}
	resources, err := StandalonePraxisResources("tenant-a", "tenant-a", "praxis:test", "Never", providers, nil)
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
	routingVolume, ok := volumes[1].(map[string]any)
	if !ok {
		t.Fatal("routing volume has unexpected type")
	}
	routingConfigMap, ok := routingVolume["configMap"].(map[string]any)
	if !ok || routingConfigMap["optional"] != true {
		t.Fatalf("routing ConfigMap must be optional during bootstrap: %#v", routingVolume["configMap"])
	}
	podSecurity, found, err := unstructured.NestedMap(deployment.Object, "spec", "template", "spec", "securityContext")
	if err != nil || !found {
		t.Fatalf("pod security context missing: found=%v err=%v", found, err)
	}
	for _, field := range []string{"runAsUser", "runAsGroup", "fsGroup"} {
		if _, exists := podSecurity[field]; exists {
			t.Errorf("pod security context must not pin %s for OpenShift SCC portability", field)
		}
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
	if !strings.Contains(config, "endpoints: [\"a.example.com:443\"]") ||
		!strings.Contains(config, "endpoints: [\"b.example.com:443\"]") {
		t.Fatalf("Praxis config did not preserve declared provider endpoints: %s", config)
	}
}

func TestStandalonePraxisResourcesRejectCrossNamespaceProvider(t *testing.T) {
	providers := []v1alpha1.ExternalProvider{{ObjectMeta: metav1.ObjectMeta{Name: "provider", Namespace: "other"}}}
	if _, err := StandalonePraxisResources("tenant-a", "tenant-a", "praxis:test", "Never", providers, nil); err == nil {
		t.Fatal("expected cross-namespace provider rejection")
	}
}

func TestStandalonePraxisResourcesUsesExternalProviderEndpoint(t *testing.T) {
	providers := []v1alpha1.ExternalProvider{{
		ObjectMeta: metav1.ObjectMeta{Name: "provider-b", Namespace: "tenant-a"},
		Spec:       v1alpha1.ExternalProviderSpec{Endpoint: "provider-b.backend.svc.cluster.local", Auth: v1alpha1.AuthConfig{SecretRef: v1alpha1.NameReference{Name: "credentials"}}},
	}}
	resources, err := StandalonePraxisResources("tenant-a", "tenant-a", "praxis:test", "Never", providers, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := findResource(resources, "ConfigMap", "praxis-config-tenant-a")
	data, _, err := unstructured.NestedStringMap(config.Object, "data")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(data["config.yaml"], `provider-b.backend.svc.cluster.local:443`) {
		t.Fatalf("config did not use declared external endpoint: %s", data["config.yaml"])
	}
	if strings.Contains(data["config.yaml"], "provider-b.tenant-a.svc.cluster.local") {
		t.Fatalf("config synthesized a tenant-local endpoint: %s", data["config.yaml"])
	}
}

func TestStandalonePraxisResourcesConfiguresTLSAndAuthorityForExternalEndpoint(t *testing.T) {
	providers := []v1alpha1.ExternalProvider{{
		ObjectMeta: metav1.ObjectMeta{Name: "openai", Namespace: "tenant-a"},
		Spec:       v1alpha1.ExternalProviderSpec{Provider: "openai", Endpoint: "api.openai.com", Auth: v1alpha1.AuthConfig{SecretRef: v1alpha1.NameReference{Name: "credentials"}}},
	}}
	resources, err := StandalonePraxisResources("tenant-a", "tenant-a", "praxis:test", "Never", providers, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := findResource(resources, "ConfigMap", "praxis-config-tenant-a")
	data, _, err := unstructured.NestedStringMap(config.Object, "data")
	if err != nil {
		t.Fatal(err)
	}
	want := "            http:\n              authority: \"api.openai.com\"\n            tls:\n              sni: \"api.openai.com\"\n            endpoints: [\"api.openai.com:443\"]"
	if !strings.Contains(data["config.yaml"], want) {
		t.Fatalf("external provider config missing verified TLS and authority:\n%s", data["config.yaml"])
	}
}

func TestStandalonePraxisResourcesSeparatesExplicitPortFromSNI(t *testing.T) {
	providers := []v1alpha1.ExternalProvider{{
		ObjectMeta: metav1.ObjectMeta{Name: "provider", Namespace: "tenant-a"},
		Spec:       v1alpha1.ExternalProviderSpec{Endpoint: "provider.example.com:8443"},
	}}
	resources, err := StandalonePraxisResources("tenant-a", "tenant-a", "praxis:test", "Never", providers, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := findResource(resources, "ConfigMap", "praxis-config-tenant-a")
	data, _, err := unstructured.NestedStringMap(config.Object, "data")
	if err != nil {
		t.Fatal(err)
	}
	want := "            http:\n              authority: \"provider.example.com:8443\"\n            tls:\n              sni: \"provider.example.com\"\n            endpoints: [\"provider.example.com:8443\"]"
	if !strings.Contains(data["config.yaml"], want) {
		t.Fatalf("explicit provider port leaked into TLS SNI:\n%s", data["config.yaml"])
	}
}

func TestStandalonePraxisResourcesUsesExplicitPlaintextFixtureException(t *testing.T) {
	providers := []v1alpha1.ExternalProvider{{
		ObjectMeta: metav1.ObjectMeta{Name: "provider-a", Namespace: "tenant-a"},
		Spec:       v1alpha1.ExternalProviderSpec{Endpoint: "provider-a.maas-system.svc.cluster.local"},
	}}
	resources, err := StandalonePraxisResourcesWithOptions("tenant-a", "tenant-a", "praxis:test", "Never", providers, nil, PraxisTransportOptions{PlaintextClusters: map[string]struct{}{"provider-provider-a": {}}})
	if err != nil {
		t.Fatal(err)
	}
	config := findResource(resources, "ConfigMap", "praxis-config-tenant-a")
	data, _, err := unstructured.NestedStringMap(config.Object, "data")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(data["config.yaml"], "tls:") || strings.Contains(data["config.yaml"], "authority:") {
		t.Fatalf("in-cluster fixture unexpectedly configured TLS or authority:\n%s", data["config.yaml"])
	}
}

func TestStandalonePraxisResourcesUsesTLSByDefaultForServiceDNS(t *testing.T) {
	providers := []v1alpha1.ExternalProvider{{
		ObjectMeta: metav1.ObjectMeta{Name: "provider-a", Namespace: "tenant-a"},
		Spec:       v1alpha1.ExternalProviderSpec{Endpoint: "provider-a.maas-system.svc.cluster.local"},
	}}
	resources, err := StandalonePraxisResources("tenant-a", "tenant-a", "praxis:test", "Never", providers, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := findResource(resources, "ConfigMap", "praxis-config-tenant-a")
	data, _, err := unstructured.NestedStringMap(config.Object, "data")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(data["config.yaml"], "sni: \"provider-a.maas-system.svc.cluster.local\"") {
		t.Fatalf("service DNS endpoint did not default to verified TLS:\n%s", data["config.yaml"])
	}
}

func TestStandalonePraxisResourcesRejectsMalformedEndpoint(t *testing.T) {
	providers := []v1alpha1.ExternalProvider{{ObjectMeta: metav1.ObjectMeta{Name: "provider", Namespace: "tenant-a"}, Spec: v1alpha1.ExternalProviderSpec{Endpoint: "https://provider.example"}}}
	if _, err := StandalonePraxisResources("tenant-a", "tenant-a", "praxis:test", "Never", providers, nil); err == nil {
		t.Fatal("expected malformed endpoint rejection")
	}
}

func praxisTestProvider(name, namespace, providerType, secret string) v1alpha1.ExternalProvider {
	return v1alpha1.ExternalProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.ExternalProviderSpec{
			Provider: providerType,
			Endpoint: name + ".example.com",
			Auth:     v1alpha1.AuthConfig{Type: "apikey", SecretRef: v1alpha1.NameReference{Name: secret}},
		},
	}
}

func praxisTestRoute(provider, providerType, apiFormat, authType, secret string) resolver.Route {
	return resolver.Route{
		Model:        "model-" + provider,
		Provider:     provider,
		Namespace:    "tenant-a",
		ProviderType: providerType,
		APIFormat:    apiFormat,
		AuthType:     authType,
		SecretName:   secret,
		SecretKey:    "api-key",
		Cluster:      "provider-" + provider,
	}
}

// praxisTestEntry returns the rendered credential_inject entry for one
// Secret so assertions bind the strategy to the right credential instead of
// to whatever appears last in the config.
func praxisTestEntry(t *testing.T, config, secretName string) string {
	t.Helper()
	header := "          - name: " + secretName + "\n"
	start := strings.Index(config, header)
	if start < 0 {
		t.Fatalf("credential entry for %s missing from config:\n%s", secretName, config)
	}
	rest := config[start+len(header):]
	end := len(rest)
	if next := strings.Index(rest, "\n          - "); next >= 0 && next < end {
		end = next
	}
	if next := strings.Index(rest, "\n      - filter:"); next >= 0 && next < end {
		end = next
	}
	return header + rest[:end]
}

func TestStandalonePraxisResourcesRendersQualifiedCredentialStrategies(t *testing.T) {
	providers := []v1alpha1.ExternalProvider{
		praxisTestProvider("openai", "tenant-a", "openai", "openai-creds"),
		praxisTestProvider("anthropic", "tenant-a", "anthropic", "anthropic-creds"),
	}
	routes := []resolver.Route{
		praxisTestRoute("openai", "openai", "openai-chat", "apikey", "openai-creds"),
		praxisTestRoute("anthropic", "anthropic", "messages", "apikey", "anthropic-creds"),
	}
	resources, err := StandalonePraxisResources("tenant-a", "tenant-a", "praxis:test", "Never", providers, routes)
	if err != nil {
		t.Fatal(err)
	}
	configMap := findResource(resources, "ConfigMap", "praxis-config-tenant-a")
	data, _, err := unstructured.NestedStringMap(configMap.Object, "data")
	if err != nil {
		t.Fatal(err)
	}
	config := data["config.yaml"]
	if entry := praxisTestEntry(t, config, "openai-creds"); !strings.Contains(entry, "strategy: bearer_token") {
		t.Fatalf("qualified OpenAI Chat credential did not render bearer_token:\n%s", entry)
	}
	if entry := praxisTestEntry(t, config, "anthropic-creds"); !strings.Contains(entry, "strategy: apikey") {
		t.Fatalf("qualified Anthropic Messages credential did not render apikey:\n%s", entry)
	}
}

func TestStandalonePraxisResourcesFailsClosedOnUnqualifiedContract(t *testing.T) {
	providers := []v1alpha1.ExternalProvider{praxisTestProvider("anthropic", "tenant-a", "anthropic", "anthropic-creds")}
	for name, route := range map[string]resolver.Route{
		"unqualified provider/API-format pair": praxisTestRoute("anthropic", "anthropic", "openai-chat", "apikey", "anthropic-creds"),
		"unmapped auth type":                   praxisTestRoute("anthropic", "anthropic", "messages", "sigv4", "anthropic-creds"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := StandalonePraxisResources("tenant-a", "tenant-a", "praxis:test", "Never", providers, []resolver.Route{route}); err == nil {
				t.Fatalf("expected render rejection for %s", name)
			}
		})
	}
}

func TestStandalonePraxisResourcesRejectsConflictingStrategiesOnSharedSecret(t *testing.T) {
	providers := []v1alpha1.ExternalProvider{
		praxisTestProvider("openai", "tenant-a", "openai", "shared"),
		praxisTestProvider("anthropic", "tenant-a", "anthropic", "shared"),
	}
	routes := []resolver.Route{
		praxisTestRoute("openai", "openai", "openai-chat", "apikey", "shared"),
		praxisTestRoute("anthropic", "anthropic", "messages", "apikey", "shared"),
	}
	if _, err := StandalonePraxisResources("tenant-a", "tenant-a", "praxis:test", "Never", providers, routes); err == nil {
		t.Fatal("expected rejection of one Secret backing two wire formats")
	}
}

func TestStandalonePraxisResourcesDefaultsStrategyWithoutQualifiedRoutes(t *testing.T) {
	providers := []v1alpha1.ExternalProvider{praxisTestProvider("anthropic", "tenant-a", "anthropic", "anthropic-creds")}
	// No models yet (nil routes), and a provider whose candidate carries no
	// credential (auth.type ""), must both keep the filter default rather
	// than fail: nothing on the wire can contradict it.
	routes := []resolver.Route{praxisTestRoute("anthropic", "anthropic", "messages", "", "anthropic-creds")}
	for name, rr := range map[string][]resolver.Route{"no routes": nil, "uncredentialed route": routes} {
		t.Run(name, func(t *testing.T) {
			resources, err := StandalonePraxisResources("tenant-a", "tenant-a", "praxis:test", "Never", providers, rr)
			if err != nil {
				t.Fatal(err)
			}
			configMap := findResource(resources, "ConfigMap", "praxis-config-tenant-a")
			data, _, err := unstructured.NestedStringMap(configMap.Object, "data")
			if err != nil {
				t.Fatal(err)
			}
			if entry := praxisTestEntry(t, data["config.yaml"], "anthropic-creds"); !strings.Contains(entry, "strategy: bearer_token") {
				t.Fatalf("credential without a qualified route lost the filter default:\n%s", entry)
			}
		})
	}
}

func TestStandalonePraxisResourcesRejectsCredentialUnknownToProviders(t *testing.T) {
	providers := []v1alpha1.ExternalProvider{praxisTestProvider("anthropic", "tenant-a", "anthropic", "anthropic-creds")}
	routes := []resolver.Route{praxisTestRoute("anthropic", "anthropic", "messages", "apikey", "phantom-creds")}
	if _, err := StandalonePraxisResources("tenant-a", "tenant-a", "praxis:test", "Never", providers, routes); err == nil {
		t.Fatal("expected rejection of a route credential no provider declares")
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
