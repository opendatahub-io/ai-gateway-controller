package tenant

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/inference/v1alpha1"
	"github.com/opendatahub-io/ai-gateway-controller/pkg/envelope"
	"github.com/opendatahub-io/ai-gateway-controller/pkg/resolver"
)

const (
	praxisDeploymentName = "praxis"
	praxisServiceName    = "praxis"
	praxisConfigMapName  = "praxis-config"
	praxisServiceAccount = "praxis"
	praxisOverlayName    = "routing-overlay"
	praxisOverlayDataKey = "routing-overlay.json"
	praxisCredentialDir  = "/etc/praxis/credentials" //nolint:gosec // fixed non-secret mount path
)

type praxisCredential struct {
	Name      string
	Namespace string
	Key       string
	Path      string
	// Strategy is the credential_inject wire strategy for this entry
	// (bearer_token|apikey). The Praxis consumer rejects an overlay
	// candidate whose strategy differs from the configured entry, so it
	// must be the same strategy envelope.StrategyFor derives for the
	// provider/model contract that references this Secret.
	Strategy string
}

// defaultCredentialStrategy matches the credential_inject filter's own
// default, keeping entries that no resolved route touches byte-compatible
// with configs rendered before strategy mapping existed.
const defaultCredentialStrategy = "bearer_token"

// PraxisTransportOptions contains explicit transport exceptions for test
// fixtures. Providers use verified TLS by default; a plaintext cluster must
// be named deliberately by the controller's test-only flag.
type PraxisTransportOptions struct {
	PlaintextClusters map[string]struct{}
}

// StandalonePraxisResources renders the tenant-owned Praxis proxy and its
// reference-only configuration. Secret bytes are never read or copied; the
// projected volume is populated by kubelet from the provider namespace.
func StandalonePraxisResources(tenantID, namespace, image, imagePullPolicy string, providers []v1alpha1.ExternalProvider, routes []resolver.Route) ([]unstructured.Unstructured, error) {
	return StandalonePraxisResourcesWithOptions(tenantID, namespace, image, imagePullPolicy, providers, routes, PraxisTransportOptions{})
}

// StandalonePraxisResourcesWithOptions renders the tenant-owned Praxis proxy
// with the explicit transport exceptions supplied by the caller. routes is
// the resolved provider/model contract (resolver.Resolve output): it is what
// the routing overlay will describe, so credential_inject entries must carry
// the strategy those routes resolve to or the consumer rejects the mismatch
// with a 503 on every credentialed request.
func StandalonePraxisResourcesWithOptions(tenantID, namespace, image, imagePullPolicy string, providers []v1alpha1.ExternalProvider, routes []resolver.Route, transport PraxisTransportOptions) ([]unstructured.Unstructured, error) {
	if namespace == "" {
		return nil, errors.New("praxis namespace is required")
	}
	if image == "" {
		return nil, errors.New("praxis image is required")
	}
	if imagePullPolicy == "" {
		imagePullPolicy = "IfNotPresent"
	}
	if imagePullPolicy != "IfNotPresent" && imagePullPolicy != "Never" && imagePullPolicy != "Always" {
		return nil, fmt.Errorf("unsupported Praxis imagePullPolicy %q", imagePullPolicy)
	}
	credentials, err := praxisCredentials(namespace, providers, routes)
	if err != nil {
		return nil, err
	}
	for _, provider := range providers {
		if err := validatePraxisEndpoint(provider.Spec.Endpoint); err != nil {
			return nil, fmt.Errorf("provider %s: %w", provider.Name, err)
		}
	}
	labels := map[string]any{
		"app":                          praxisDeploymentName,
		"app.kubernetes.io/managed-by": "ai-gateway-controller",
		LabelTenantInstance:            ResourceName(praxisDeploymentName, tenantID),
	}
	config, err := praxisConfig(credentials, providers, transport)
	if err != nil {
		return nil, err
	}
	return []unstructured.Unstructured{
		{Object: map[string]any{
			"apiVersion": "v1", "kind": "ServiceAccount",
			"metadata": map[string]any{"name": ResourceName(praxisServiceAccount, tenantID), "namespace": namespace, "labels": labels},
		}},
		{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": ResourceName(praxisConfigMapName, tenantID), "namespace": namespace, "labels": labels},
			"data":     map[string]any{"config.yaml": config},
		}},
		{Object: map[string]any{
			"apiVersion": "v1", "kind": "Service",
			"metadata": map[string]any{"name": ResourceName(praxisServiceName, tenantID), "namespace": namespace, "labels": labels},
			"spec": map[string]any{
				"selector": map[string]any{"app": praxisDeploymentName, LabelTenantInstance: ResourceName(praxisDeploymentName, tenantID)},
				"ports": []any{
					map[string]any{"name": "http", "port": int64(8080), "targetPort": "http"},
					map[string]any{"name": "admin", "port": int64(9901), "targetPort": "admin"},
				},
			},
		}},
		{Object: praxisDeployment(namespace, tenantID, image, imagePullPolicy, labels, credentials)},
	}, nil
}

func validatePraxisEndpoint(endpoint string) error {
	_, err := parsePraxisEndpoint(endpoint)
	return err
}

type praxisEndpoint struct {
	Authority string
	Dial      string
	Host      string
}

func parsePraxisEndpoint(endpoint string) (praxisEndpoint, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return praxisEndpoint{}, errors.New("provider endpoint is required")
	}
	if strings.ContainsAny(endpoint, " /\t\r\n") || strings.Contains(endpoint, "://") {
		return praxisEndpoint{}, fmt.Errorf("provider endpoint %q must be a host or host:port", endpoint)
	}
	parsed, err := url.Parse("https://" + endpoint)
	if err != nil || parsed.User != nil || parsed.Hostname() == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return praxisEndpoint{}, fmt.Errorf("provider endpoint %q must be a host or host:port", endpoint)
	}
	port := parsed.Port()
	if port == "" {
		port = "443"
	}
	return praxisEndpoint{
		Authority: parsed.Host,
		Dial:      net.JoinHostPort(parsed.Hostname(), port),
		Host:      parsed.Hostname(),
	}, nil
}

func praxisCredentials(namespace string, providers []v1alpha1.ExternalProvider, routes []resolver.Route) ([]praxisCredential, error) {
	// Derive each credential's wire strategy from the resolved provider/model
	// contract, not from providers alone: apiFormat lives on the model
	// reference, and envelope.StrategyFor qualifies the provider/API-format
	// pair the overlay candidates will carry. An entry configured with a
	// different strategy than its candidates is a runtime 503 (praxis-ai
	// #1172 cross-check), so unqualified or contradictory contracts must
	// fail closed here instead of rendering a config that cannot serve.
	strategies := map[string]string{}
	for _, route := range routes {
		if route.SecretName == "" {
			continue
		}
		strategy, err := envelope.StrategyFor(route)
		if err != nil {
			return nil, fmt.Errorf("model %s provider %s: %w", route.Model, route.Provider, err)
		}
		if strategy == "" {
			// auth.type "": the candidate carries no credential, so the
			// entry keeps the filter default and nothing can contradict it.
			continue
		}
		key := route.SecretKey
		if key == "" {
			key = "api-key"
		}
		identity := namespace + "/" + route.SecretName + "/" + key
		if prev, ok := strategies[identity]; ok && prev != strategy {
			return nil, fmt.Errorf("credential Secret %s requires conflicting strategies %q and %q across providers; it cannot back both wire formats", route.SecretName, prev, strategy)
		}
		strategies[identity] = strategy
	}

	seen := map[string]bool{}
	credentials := make([]praxisCredential, 0, len(providers))
	for _, provider := range providers {
		if provider.Namespace != "" && provider.Namespace != namespace {
			return nil, fmt.Errorf("provider %s/%s references a cross-namespace Secret", provider.Namespace, provider.Name)
		}
		if provider.Spec.Auth.Type != "" && provider.Spec.Auth.Type != "apikey" {
			return nil, fmt.Errorf("provider %s uses unsupported standalone Praxis credential type %q", provider.Name, provider.Spec.Auth.Type)
		}
		name := provider.Spec.Auth.SecretRef.Name
		if name == "" {
			continue
		}
		key := "api-key"
		identity := namespace + "/" + name + "/" + key
		if seen[identity] {
			continue
		}
		seen[identity] = true
		hash := sha256.Sum256([]byte(identity))
		pathID := name + "-" + hex.EncodeToString(hash[:])[:12] + "/" + key
		strategy := defaultCredentialStrategy
		if s, ok := strategies[identity]; ok {
			strategy = s
		}
		credentials = append(credentials, praxisCredential{Name: name, Namespace: namespace, Key: key, Path: praxisCredentialDir + "/" + pathID, Strategy: strategy})
	}
	for identity, strategy := range strategies {
		if !seen[identity] {
			// A resolved route references a Secret no provider declares as
			// its auth credential; the overlay candidate would then find no
			// configured entry and every request through it fails closed.
			return nil, fmt.Errorf("resolved route requires credential %s (strategy %q) that no provider declares", identity, strategy)
		}
	}
	sort.Slice(credentials, func(i, j int) bool { return credentials[i].Name < credentials[j].Name })
	return credentials, nil
}

func praxisConfig(credentials []praxisCredential, providers []v1alpha1.ExternalProvider, transport PraxisTransportOptions) (string, error) {
	var b strings.Builder
	b.WriteString("listeners:\n")
	b.WriteString("  - name: proxy\n    address: \"0.0.0.0:8080\"\n    filter_chains: [main]\n")
	b.WriteString("filter_chains:\n  - name: main\n    filters:\n")
	b.WriteString("      - filter: intelligent_route\n")
	b.WriteString("        overlay_file: /etc/praxis/routing/routing-overlay.json\n")
	b.WriteString("        model_header: X-Gateway-Model-Name\n")
	b.WriteString("        reload: {enabled: true, debounce_ms: 500}\n")
	if len(credentials) > 0 {
		b.WriteString("      - filter: credential_inject\n        credentials:\n")
		for _, credential := range credentials {
			fmt.Fprintf(&b, "          - name: %s\n            namespace: %s\n            key: %s\n            file: %s\n            strategy: %s\n", credential.Name, credential.Namespace, credential.Key, credential.Path, credential.Strategy)
		}
	}
	b.WriteString("      - filter: load_balancer\n        clusters:\n")
	ordered := append([]v1alpha1.ExternalProvider(nil), providers...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	for _, provider := range ordered {
		if provider.Name == "" || provider.Spec.Endpoint == "" {
			continue
		}
		// The ExternalProvider endpoint is authoritative.  In particular, a
		// provider fixture may live in a separate backend namespace; deriving a
		// Service name from the tenant namespace silently creates an endpoint
		// that Praxis cannot resolve.  Keep the overlay reference-only and add
		// the TLS port when the CRD endpoint omits it.
		endpoint, err := parsePraxisEndpoint(provider.Spec.Endpoint)
		if err != nil {
			return "", fmt.Errorf("provider %s endpoint: %w", provider.Name, err)
		}
		fmt.Fprintf(&b, "          - name: provider-%s\n", provider.Name)
		clusterName := "provider-" + provider.Name
		if _, plaintext := transport.PlaintextClusters[clusterName]; !plaintext {
			// ExternalProvider endpoints use verified TLS unless the caller has
			// explicitly named this fixture cluster as plaintext.
			fmt.Fprintf(&b, "            http:\n              authority: %q\n            tls:\n              sni: %q\n", endpoint.Authority, endpoint.Host)
		}
		fmt.Fprintf(&b, "            endpoints: [%q]\n", endpoint.Dial)
	}
	b.WriteString("admin: {address: \"127.0.0.1:9901\"}\ninsecure_options: {allow_private_endpoints: true}\n")
	return b.String(), nil
}

func praxisDeployment(namespace, tenantID, image, imagePullPolicy string, labels map[string]any, credentials []praxisCredential) map[string]any {
	volumes := []any{
		map[string]any{"name": "config", "configMap": map[string]any{"name": ResourceName(praxisConfigMapName, tenantID)}},
		// The first tenant reconcile can precede the first valid model overlay.
		// Kubelet must allow Praxis to start and report readiness while the
		// routing reconciler publishes the first overlay.
		map[string]any{"name": "routing", "configMap": map[string]any{"name": praxisOverlayName, "optional": true}},
	}
	if len(credentials) > 0 {
		bySecret := map[string][]any{}
		for _, credential := range credentials {
			bySecret[credential.Name] = append(bySecret[credential.Name], map[string]any{
				"key":  credential.Key,
				"path": strings.TrimPrefix(credential.Path, praxisCredentialDir+"/"),
			})
		}
		secretNames := make([]string, 0, len(bySecret))
		for name := range bySecret {
			secretNames = append(secretNames, name)
		}
		sort.Strings(secretNames)
		sources := make([]any, 0, len(secretNames))
		for _, name := range secretNames {
			sources = append(sources, map[string]any{"secret": map[string]any{"name": name, "items": bySecret[name]}})
		}
		volumes = append(volumes, map[string]any{"name": "credentials", "projected": map[string]any{"sources": sources}})
	}
	mounts := []any{
		map[string]any{"name": "config", "mountPath": "/etc/praxis/config", "readOnly": true},
		map[string]any{"name": "routing", "mountPath": "/etc/praxis/routing", "readOnly": true},
	}
	if len(credentials) > 0 {
		mounts = append(mounts, map[string]any{"name": "credentials", "mountPath": praxisCredentialDir, "readOnly": true})
	}
	container := map[string]any{
		"name": "praxis", "image": image, "imagePullPolicy": imagePullPolicy,
		"args":            []any{"-c", "/etc/praxis/config/config.yaml"},
		"ports":           []any{map[string]any{"name": "http", "containerPort": int64(8080)}, map[string]any{"name": "admin", "containerPort": int64(9901)}},
		"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []any{"ALL"}}, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		// Praxis keeps its admin health listener loopback-only; probe the externally bound serving listener.
		"readinessProbe": map[string]any{"tcpSocket": map[string]any{"port": "http"}, "periodSeconds": int64(10)},
		"livenessProbe":  map[string]any{"tcpSocket": map[string]any{"port": "http"}, "periodSeconds": int64(20)},
		"resources":      map[string]any{"requests": map[string]any{"cpu": "50m", "memory": "64Mi"}, "limits": map[string]any{"cpu": "500m", "memory": "512Mi"}},
		"volumeMounts":   mounts,
	}
	return map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": ResourceName(praxisDeploymentName, tenantID), "namespace": namespace, "labels": labels},
		"spec": map[string]any{
			"replicas": int64(1),
			"selector": map[string]any{"matchLabels": map[string]any{"app": praxisDeploymentName, LabelTenantInstance: ResourceName(praxisDeploymentName, tenantID)}},
			"template": map[string]any{"metadata": map[string]any{"labels": labels}, "spec": map[string]any{
				"serviceAccountName":           ResourceName(praxisServiceAccount, tenantID),
				"automountServiceAccountToken": false,
				// Let the platform assign the namespace-compatible UID/GID (notably
				// under OpenShift restricted-v2 SCC). The image remains non-root.
				"securityContext": map[string]any{"runAsNonRoot": true},
				"containers":      []any{container},
				"volumes":         volumes,
			}},
		},
	}
}
