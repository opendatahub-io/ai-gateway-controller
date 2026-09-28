package tenant

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"context"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/opendatahub-io/ai-gateway-controller/pkg/envelope"
	"github.com/opendatahub-io/ai-gateway-controller/pkg/resolver"
)

// configureResponsesExtProc replaces the post-auth pipeline for an explicitly
// qualified Responses provider. The existing credential projection is reused.
func configureResponsesExtProc(resources []unstructured.Unstructured, namespace string, candidates []envelope.Candidate, route resolver.Route, backend *ogxBackend) error {
	credentials, err := extprocCredentials(namespace, candidates)
	if err != nil {
		return err
	}
	config, err := responsesConfig(namespace, route, backend, candidates, credentials)
	if err != nil {
		return err
	}
	for i := range resources {
		resource := &resources[i]
		if resource.GetKind() != "ConfigMap" || resource.GetNamespace() != namespace ||
			!strings.HasPrefix(resource.GetName(), PayloadProcessingExternalModelName+"-plugins") {
			continue
		}
		data, _, err := unstructured.NestedStringMap(resource.Object, "data")
		if err != nil {
			return fmt.Errorf("read Responses ExtProc config: %w", err)
		}
		data["extproc.yaml"] = config
		if err := unstructured.SetNestedStringMap(resource.Object, data, "data"); err != nil {
			return fmt.Errorf("write Responses ExtProc config: %w", err)
		}
		break
	}
	for i := range resources {
		resource := &resources[i]
		if resource.GetKind() != "Deployment" || resource.GetNamespace() != namespace ||
			!strings.HasPrefix(resource.GetName(), PayloadProcessingExternalModelName) {
			continue
		}
		hash := sha256.Sum256([]byte(config))
		if err := unstructured.SetNestedField(resource.Object, hex.EncodeToString(hash[:]), "spec", "template", "metadata", "annotations", "ai-gateway-controller.opendatahub.io/extproc-config-sha256"); err != nil {
			return err
		}
		volumes, _, err := unstructured.NestedSlice(resource.Object, "spec", "template", "spec", "volumes")
		if err != nil {
			return err
		}
		volumes = appendOrReplaceNamedVolume(volumes, "responses-db", map[string]any{"name": "responses-db", "emptyDir": map[string]any{}})
		if err := unstructured.SetNestedSlice(resource.Object, volumes, "spec", "template", "spec", "volumes"); err != nil {
			return err
		}
		containers, _, err := unstructured.NestedSlice(resource.Object, "spec", "template", "spec", "containers")
		if err != nil || len(containers) == 0 {
			return fmt.Errorf("Responses ExtProc container missing: %v", err)
		}
		container := containers[0].(map[string]any)
		mounts, _, err := unstructured.NestedSlice(container, "volumeMounts")
		if err != nil {
			return err
		}
		mounts = appendOrReplaceNamedVolume(mounts, "responses-db", map[string]any{"name": "responses-db", "mountPath": "/tmp"})
		container["volumeMounts"] = mounts
		container["workingDir"] = "/tmp"
		containers[0] = container
		if err := unstructured.SetNestedSlice(resource.Object, containers, "spec", "template", "spec", "containers"); err != nil {
			return err
		}
		break
	}
	return nil
}

func responsesConfig(namespace string, route resolver.Route, backend *ogxBackend, candidates []envelope.Candidate, credentials []extprocCredential) (string, error) {
	if backend == nil {
		return "", fmt.Errorf("Responses model %s needs an OGXServer backend", route.Model)
	}
	endpoint, err := url.Parse("https://" + route.Endpoint)
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" {
		return "", fmt.Errorf("invalid Responses provider endpoint %q", route.Endpoint)
	}
	cluster := "provider-" + route.Provider
	seen := map[string]bool{}
	var clusters []string
	for _, candidate := range candidates {
		if !seen[candidate.Cluster] {
			clusters = append(clusters, fmt.Sprintf("%q", candidate.Cluster))
			seen[candidate.Cluster] = true
		}
	}
	sort.Strings(clusters)
	var b strings.Builder
	b.WriteString("server:\n  grpc_address: '0.0.0.0:9004'\n  health_address: '0.0.0.0:50052'\n  metrics_address: '0.0.0.0:9090'\n  tls:\n    mode: self_signed\n\nfilter_chains:\n  - name: post-auth\n    filters:\n")
	b.WriteString("      - filter: trace_context\n      - filter: state_owner\n        mode: trusted_headers\n        tenant:\n          header: x-maas-owner-tenant\n        issuer:\n          static: urn:maas:api-key\n        subject:\n          header: x-maas-username\n")
	b.WriteString("      - filter: headers\n        request_remove: [x-user-brave-key, x-user-ogx-key, x-user-mcp-key, x-mcp-authorized]\n      - filter: identity_header_guard\n        prefix: x-maas-\n        metadata_namespace: maas\n      - filter: project_state_owner_headers\n        tenant_header: x-tenant-id\n        subject_header: x-user-id\n")
	b.WriteString("      - filter: path_rewrite\n        replace:\n          pattern: '^.*(/v1/.*)$'\n          replacement: '$1'\n")
	fmt.Fprintf(&b, "      - filter: headers\n        request_set:\n          - name: x-ogx-route-tenant\n            value: %q\n", namespace)
	fmt.Fprintf(&b, "      - filter: intelligent_route\n        overlay_file: /etc/praxis/routing/routing-overlay.json\n        model_header: X-Gateway-Model-Name\n        provider_hop_clusters: [%s]\n        reload: {enabled: true, debounce_ms: 500}\n", strings.Join(clusters, ", "))
	b.WriteString("      - filter: openai_operation\n      - filter: openai_conversations\n        backend: sqlite\n        database_url: 'sqlite://responses.db?mode=rwc'\n        conversations_table: openai_conversations\n        items_table: openai_conversation_items\n")
	b.WriteString("      - filter: openai_responses_format\n        on_invalid: continue\n        headers:\n          format: x-praxis-ai-format\n          model: x-praxis-ai-model\n          stream: x-praxis-ai-stream\n          mode: x-praxis-responses-mode\n      - filter: openai_responses_validate\n      - filter: openai_tool_parse\n")
	b.WriteString("      - filter: openai_response_store\n        backend: sqlite\n        database_url: 'sqlite://responses.db?mode=rwc'\n        responses_table: openai_responses\n        conversations_table: openai_conversations\n      - filter: openai_responses_rehydrate\n")
	fmt.Fprintf(&b, "      - filter: openai_file_resolve\n        files_api_url: %q\n        allow_pre_security_callout: true\n        outbound_chain:\n          name: files-api-outbound\n          filters:\n            - filter: project_state_owner_headers\n              tenant_header: x-tenant-id\n              subject_header: x-user-id\n        on_missing: reject\n        timeout_ms: 10000\n", backend.url)
	b.WriteString("      - filter: openai_doc_extract\n        allow_pre_security_callout: true\n        on_unsupported: continue\n")
	for _, api := range []string{"files", "vector_stores", "prompts", "embeddings"} {
		fmt.Fprintf(&b, "      - filter: headers\n        conditions:\n          - when:\n              path_prefix: /v1/%s\n        branch_chains:\n          - name: bypass-%s\n            rejoin: terminal\n            chains:\n              - name: ogx-%s\n                filters:\n                  - filter: router\n                    routes:\n                      - path_prefix: /v1/%s\n                        cluster: ogx-api\n                  - filter: load_balancer\n                    clusters:\n                      - name: ogx-api\n                        endpoints: [%q]\n", api, api, api, api, endpointHost(backend.url))
	}
	b.WriteString("      - filter: iterative_request_router\n        conditions:\n          - when:\n              path: /v1/responses\n              methods: [POST]\n        initial_step: inference\n        max_iterations: 8\n        timeout_ms: 360000\n        step_timeout_ms: 300000\n        max_response_bytes: 67108864\n        max_stream_response_bytes: 67108864\n        max_state_bytes: 136314880\n        steps:\n          - name: inference\n            filters:\n")
	fmt.Fprintf(&b, "              - filter: intelligent_route\n                overlay_file: /etc/praxis/routing/routing-overlay.json\n                model_header: X-Gateway-Model-Name\n                provider_hop_clusters: [%q]\n", cluster)
	b.WriteString("              - filter: project_state_owner_headers\n                tenant_header: x-tenant-id\n                subject_header: x-user-id\n              - filter: openai_stream_events\n")
	fmt.Fprintf(&b, "              - filter: openai_file_search_callout\n                vector_store_url: %q\n                outbound_chain:\n                  name: vector-store-outbound\n                  filters:\n                    - filter: project_state_owner_headers\n                      tenant_header: x-tenant-id\n                      subject_header: x-user-id\n                timeout_ms: 10000\n                max_response_bytes: 10485760\n                max_total_response_bytes: 67108864\n                max_state_bytes: 136314880\n                on_failure: closed\n", backend.url)
	b.WriteString("              - filter: openai_agentic_loop\n                max_infer_iters: 7\n              - filter: load_balancer\n                clusters:\n")
	fmt.Fprintf(&b, "                  - name: %q\n                    http:\n                      application_protocol: openai_responses\n                      application_provider: openai\n                    endpoints: [%q]\n                    tls:\n                      sni: %q\n", cluster, endpoint.Host, endpoint.Hostname())
	fmt.Fprintf(&b, "              - filter: headers\n                request_set:\n                  - name: Host\n                    value: %q\n", endpoint.Hostname())
	writeProviderCredentials(&b, credentials)
	b.WriteString("              - filter: openai_responses_proxy\n            on_result:\n              - filter: openai_agentic_loop\n                key: action\n                value: loop\n                next: inference\n              - default: true\n                done: true\n")
	writeOuterCredentials(&b, credentials)
	b.WriteString("\ninsecure_options:\n  allow_unbounded_body: true\n  allow_private_upstreams: true\n")
	return b.String(), nil
}

func writeProviderCredentials(b *strings.Builder, credentials []extprocCredential) {
	if len(credentials) == 0 {
		return
	}
	b.WriteString("              - filter: credential_inject\n                credentials:\n")
	for _, c := range credentials {
		fmt.Fprintf(b, "                  - name: %q\n                    namespace: %q\n                    key: %q\n                    strategy: %q\n                    file: %q\n", c.name, c.namespace, c.key, c.strategy, c.file)
	}
}

func writeOuterCredentials(b *strings.Builder, credentials []extprocCredential) {
	if len(credentials) == 0 {
		return
	}
	b.WriteString("      - filter: credential_inject\n        credentials:\n")
	for _, c := range credentials {
		fmt.Fprintf(b, "          - name: %q\n            namespace: %q\n            key: %q\n            strategy: %q\n            file: %q\n", c.name, c.namespace, c.key, c.strategy, c.file)
	}
}

func ogxAPIRouteName(tenantID string) string {
	return ResourceName("ogx-api", tenantID)
}

const ogxTenantNamespaceLabel = "ai-gateway-controller.opendatahub.io/ogx-tenant-namespace"

// Cross-namespace resources cannot have an ownerReference to the tenant's
// MaasTenantConfig. Label them for cleanup when the backend reference changes.
func markOGXResources(resources []unstructured.Unstructured, tenantNamespace string) {
	for i := range resources {
		labels := resources[i].GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[ogxTenantNamespaceLabel] = tenantNamespace
		resources[i].SetLabels(labels)
	}
}

func (r *Reconciler) cleanupOGXResources(ctx context.Context, tenantID, tenantNamespace, gatewayNamespace, desiredBackendNamespace string) error {
	for _, target := range []struct {
		gvk             schema.GroupVersionKind
		name, namespace string
	}{
		{gvkHTTPRoute, ogxAPIRouteName(tenantID), tenantNamespace},
		{gvkEnvoyFilter, ResourceName("ogx-api-extproc", tenantID), gatewayNamespace},
		{gvkNetworkPolicy, ResourceName("ogx-callouts", tenantID), tenantNamespace},
	} {
		if desiredBackendNamespace == "" {
			if err := r.deleteResourceIfOwned(ctx, target.gvk, target.name, target.namespace); err != nil {
				return err
			}
		}
	}
	for _, target := range []struct {
		gvk  schema.GroupVersionKind
		name string
	}{
		{gvkReferenceGrant, ResourceName("ogx-gateway", tenantID)},
		{gvkNetworkPolicy, ResourceName("ogx-gateway-ingress", tenantID)},
	} {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(target.gvk.GroupVersion().WithKind(target.gvk.Kind + "List"))
		if err := r.Client.List(ctx, list, client.MatchingLabels{ogxTenantNamespaceLabel: tenantNamespace}); err != nil {
			return fmt.Errorf("list OGX %s for cleanup: %w", target.gvk.Kind, err)
		}
		for i := range list.Items {
			obj := &list.Items[i]
			if obj.GetName() != target.name || obj.GetNamespace() == desiredBackendNamespace || !shouldDeletePraxisResource(obj) {
				continue
			}
			if err := client.IgnoreNotFound(r.Client.Delete(ctx, obj)); err != nil {
				return fmt.Errorf("delete OGX %s %s/%s: %w", target.gvk.Kind, obj.GetNamespace(), obj.GetName(), err)
			}
		}
	}
	return nil
}

func (r *Reconciler) ogxAuthEnforced(ctx context.Context, tenantID, tenantNamespace string) (bool, error) {
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicy"})
	key := client.ObjectKey{Namespace: tenantNamespace, Name: ResourceName("ogx-api-auth", tenantID)}
	if err := r.Client.Get(ctx, key, policy); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if policy.GetLabels()["app.kubernetes.io/part-of"] != "ogx-api-auth" {
		return false, nil
	}
	target, _, _ := unstructured.NestedString(policy.Object, "spec", "targetRef", "name")
	observed, _, _ := unstructured.NestedInt64(policy.Object, "status", "observedGeneration")
	if target != ogxAPIRouteName(tenantID) || observed != policy.GetGeneration() || observed == 0 {
		return false, nil
	}
	conditions, _, _ := unstructured.NestedSlice(policy.Object, "status", "conditions")
	ready := map[string]bool{}
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if ok && condition["status"] == "True" {
			if name, ok := condition["type"].(string); ok {
				ready[name] = true
			}
		}
	}
	return ready["Accepted"] && ready["Enforced"], nil
}

func ogxAPIResources(tenantID, tenantNamespace, gatewayName, gatewayNamespace string, backend *ogxBackend, authEnforced bool) []unstructured.Unstructured {
	name := ogxAPIRouteName(tenantID)
	paths := []string{"files", "vector_stores", "prompts", "embeddings", "conversations"}
	rules := make([]any, 0, len(paths)*2)
	for _, api := range paths {
		path := "/v1/" + api
		backendRef := map[string]any{"name": backend.service, "namespace": backend.namespace, "port": backend.port}
		filters := []any{map[string]any{"type": "RequestHeaderModifier", "requestHeaderModifier": map[string]any{"remove": []any{"x-ogx-route-tenant"}}}}
		prefixRule := map[string]any{
			"matches": []any{map[string]any{"path": map[string]any{"type": "PathPrefix", "value": "/" + tenantNamespace + path}}},
			"filters": filters,
		}
		internalRule := map[string]any{
			"matches": []any{map[string]any{"path": map[string]any{"type": "PathPrefix", "value": path},
				"headers": []any{map[string]any{"name": "x-ogx-route-tenant", "type": "Exact", "value": tenantNamespace}}}},
			"filters": filters,
		}
		if authEnforced {
			prefixRule["backendRefs"] = []any{backendRef}
			internalRule["backendRefs"] = []any{backendRef}
		}
		rules = append(rules, prefixRule, internalRule)
	}
	parent := map[string]any{"name": gatewayName}
	if tenantNamespace != gatewayNamespace {
		parent["namespace"] = gatewayNamespace
	}
	route := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute",
		"metadata": map[string]any{"name": name, "namespace": tenantNamespace},
		"spec":     map[string]any{"parentRefs": []any{parent}, "rules": rules},
	}}
	grant := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1beta1", "kind": "ReferenceGrant",
		"metadata": map[string]any{"name": ResourceName("ogx-gateway", tenantID), "namespace": backend.namespace},
		"spec": map[string]any{
			"from": []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "namespace": tenantNamespace}},
			"to":   []any{map[string]any{"group": "", "kind": "Service", "name": backend.service}},
		},
	}}
	filter := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "networking.istio.io/v1alpha3", "kind": "EnvoyFilter",
		"metadata": map[string]any{"name": ResourceName("ogx-api-extproc", tenantID), "namespace": gatewayNamespace},
		"spec": map[string]any{"priority": int64(30), "workloadSelector": map[string]any{"labels": map[string]any{"gateway.networking.k8s.io/gateway-name": gatewayName}},
			"configPatches": ogxAPIEnvoyRoutePatches(tenantNamespace, name, len(rules))},
	}}
	backendPods := map[string]any{"matchLabels": map[string]any{"app": "ogx", "app.kubernetes.io/instance": backend.server}}
	fromNamespace := func(namespace string) map[string]any {
		return map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": namespace}}
	}
	port := []any{map[string]any{"protocol": "TCP", "port": backend.port}}
	egress := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
		"metadata": map[string]any{"name": ResourceName("ogx-callouts", tenantID), "namespace": tenantNamespace},
		"spec": map[string]any{"podSelector": map[string]any{"matchLabels": map[string]any{"app": PayloadProcessingExternalModelName, LabelTenantInstance: PayloadProcessingExternalModelDeploymentName(tenantID)}},
			"policyTypes": []any{"Egress"}, "egress": []any{map[string]any{"to": []any{map[string]any{"namespaceSelector": fromNamespace(backend.namespace), "podSelector": backendPods}}, "ports": port}}},
	}}
	ingress := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
		"metadata": map[string]any{"name": ResourceName("ogx-gateway-ingress", tenantID), "namespace": backend.namespace},
		"spec": map[string]any{"podSelector": backendPods, "policyTypes": []any{"Ingress"}, "ingress": []any{map[string]any{
			"from": []any{
				map[string]any{"namespaceSelector": fromNamespace(tenantNamespace), "podSelector": map[string]any{"matchLabels": map[string]any{"app": PayloadProcessingExternalModelName, LabelTenantInstance: PayloadProcessingExternalModelDeploymentName(tenantID)}}},
				map[string]any{"namespaceSelector": fromNamespace(gatewayNamespace), "podSelector": map[string]any{"matchLabels": map[string]any{"gateway.networking.k8s.io/gateway-name": gatewayName}}},
			}, "ports": port,
		}}},
	}}
	resources := []unstructured.Unstructured{route, grant, filter, egress, ingress}
	markOGXResources(resources, tenantNamespace)
	return resources
}

func ogxAPIEnvoyRoutePatches(namespace, name string, count int) []any {
	patches := make([]any, 0, count)
	mode := map[string]any{"request_header_mode": "SEND", "request_body_mode": "BUFFERED", "request_trailer_mode": "SKIP", "response_header_mode": "SEND", "response_body_mode": "NONE", "response_trailer_mode": "SKIP"}
	preMode := map[string]any{"request_header_mode": "SEND", "request_body_mode": "BUFFERED", "request_trailer_mode": "SKIP", "response_header_mode": "SKIP", "response_body_mode": "NONE", "response_trailer_mode": "SKIP"}
	for i := 0; i < count; i++ {
		patches = append(patches, map[string]any{
			"applyTo": "HTTP_ROUTE", "match": map[string]any{"context": "GATEWAY", "routeConfiguration": map[string]any{"vhost": map[string]any{"route": map[string]any{"name": fmt.Sprintf("%s.%s.%d", namespace, name, i)}}}},
			"patch": map[string]any{"operation": "MERGE", "value": map[string]any{"typed_per_filter_config": map[string]any{
				"envoy.filters.http.ext_proc.external-model":     map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExtProcPerRoute", "overrides": map[string]any{"processing_mode": mode}},
				"envoy.filters.http.ext_proc.external-model-pre": map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExtProcPerRoute", "overrides": map[string]any{"processing_mode": preMode}},
				"envoy.filters.http.ext_proc.ipp":                map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExtProcPerRoute", "disabled": true},
				"envoy.filters.http.ext_proc.ipp-pre":            map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExtProcPerRoute", "disabled": true},
			}}},
		})
	}
	return patches
}
