/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package tenant

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/opendatahub-io/ai-gateway-controller/pkg/envelope"
)

const extprocCredentialDir = "/etc/praxis/credentials" //nolint:gosec // fixed non-secret mount path

// MaaS-aligned metering defaults. Authorino injects the verified x-maas-*
// identity headers after the pre-auth (BBR) chain, so metering lives in the
// post-auth chain where those headers — and the response path usage/billing
// needs — are visible. See docs/praxis-dataplane-evolution.md and
// https://github.com/opendatahub-io/ai-gateway-controller/issues/99.
const (
	// meteringCloudEventSource is the CloudEvents `source` on emitted metering
	// events (praxis-extproc#117).
	meteringCloudEventSource = "maas-gateway"
	// meteringIdentityHeaderPrefix is the prefix of the Authorino-injected
	// identity headers (x-maas-username/group/subscription/model) that metering
	// captures (praxis-extproc#116).
	meteringIdentityHeaderPrefix = "x-maas-"
	// meteringIdentityMetadataNamespace is the filter_metadata namespace
	// identity_header_guard writes the captured headers to and external_metering
	// reads them back from.
	meteringIdentityMetadataNamespace = "identity"
	// meteringTimeoutSeconds bounds each metering HTTP call so a slow metering
	// service cannot stall inference requests.
	meteringTimeoutSeconds = 5
)

type extprocCredential struct {
	name      string
	namespace string
	key       string
	strategy  string
	file      string
}

// configureExternalModelExtProc completes the dedicated ExternalModel ExtProc
// render with tenant-specific routing and credential references. The upstream
// MaaS/KServe post-auth workload is intentionally not modified. Secret values
// are never read: kubelet projects the referenced Secret into this workload and
// credential_inject reads the mounted file at request time.
func configureExternalModelExtProc(resources []unstructured.Unstructured, namespace, meteringURL string, candidates []envelope.Candidate, providerKinds []string) error {
	credentials, err := extprocCredentials(namespace, candidates)
	if err != nil {
		return err
	}
	config, err := extprocPostAuthConfig(candidates, credentials, meteringURL, providerKinds)
	if err != nil {
		return err
	}

	var configMap *unstructured.Unstructured
	var deployment *unstructured.Unstructured
	for i := range resources {
		resource := &resources[i]
		if configMap == nil && resource.GetNamespace() == namespace && resource.GetKind() == "ConfigMap" && strings.HasPrefix(resource.GetName(), PayloadProcessingExternalModelName+"-plugins") {
			configMap = resource
		}
		if deployment == nil && resource.GetNamespace() == namespace && resource.GetKind() == "Deployment" && strings.HasPrefix(resource.GetName(), PayloadProcessingExternalModelName) {
			deployment = resource
		}
	}
	if configMap == nil || deployment == nil {
		return errors.New("post-auth ExtProc resources are missing the plugin ConfigMap or Deployment")
	}
	data, found, err := unstructured.NestedStringMap(configMap.Object, "data")
	if err != nil {
		return fmt.Errorf("read post-auth ExtProc ConfigMap data: %w", err)
	}
	if !found {
		return errors.New("post-auth ExtProc ConfigMap data is missing")
	}
	data["extproc.yaml"] = config
	if err := unstructured.SetNestedStringMap(configMap.Object, data, "data"); err != nil {
		return fmt.Errorf("write post-auth ExtProc ConfigMap data: %w", err)
	}
	configHash := sha256.Sum256([]byte(config))
	if err := unstructured.SetNestedField(deployment.Object, hex.EncodeToString(configHash[:]), "spec", "template", "metadata", "annotations", "ai-gateway-controller.opendatahub.io/extproc-config-sha256"); err != nil {
		return fmt.Errorf("write post-auth ExtProc config hash: %w", err)
	}

	if len(credentials) == 0 {
		return nil
	}
	volumes, _, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "volumes")
	if err != nil {
		return fmt.Errorf("read post-auth ExtProc volumes: %w", err)
	}
	projectedSources := make([]any, 0, len(credentials))
	projectedFiles := map[string]bool{}
	for _, credential := range credentials {
		if projectedFiles[credential.file] {
			continue
		}
		projectedFiles[credential.file] = true
		projectedSources = append(projectedSources, map[string]any{
			"secret": map[string]any{
				"name":  credential.name,
				"items": []any{map[string]any{"key": credential.key, "path": strings.TrimPrefix(credential.file, extprocCredentialDir+"/")}},
			},
		})
	}
	volumes = appendOrReplaceNamedVolume(volumes, "provider-credentials", map[string]any{
		"name":      "provider-credentials",
		"projected": map[string]any{"sources": projectedSources},
	})
	if err := unstructured.SetNestedSlice(deployment.Object, volumes, "spec", "template", "spec", "volumes"); err != nil {
		return fmt.Errorf("write post-auth ExtProc volumes: %w", err)
	}
	containers, found, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	if err != nil || !found || len(containers) == 0 {
		if err == nil {
			err = errors.New("containers are missing")
		}
		return fmt.Errorf("read post-auth ExtProc containers: %w", err)
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		return errors.New("post-auth ExtProc first container is malformed")
	}
	mounts, _, err := unstructured.NestedSlice(container, "volumeMounts")
	if err != nil {
		return fmt.Errorf("read post-auth ExtProc volume mounts: %w", err)
	}
	mounts = appendOrReplaceNamedVolume(mounts, "provider-credentials", map[string]any{
		"name": "provider-credentials", "mountPath": extprocCredentialDir, "readOnly": true,
	})
	if err := unstructured.SetNestedSlice(container, mounts, "volumeMounts"); err != nil {
		return fmt.Errorf("write post-auth ExtProc volume mounts: %w", err)
	}
	containers[0] = container
	if err := unstructured.SetNestedSlice(deployment.Object, containers, "spec", "template", "spec", "containers"); err != nil {
		return fmt.Errorf("write post-auth ExtProc containers: %w", err)
	}
	return nil
}

// extprocCredentials returns deterministic, reference-only file mappings.
func extprocCredentials(namespace string, candidates []envelope.Candidate) ([]extprocCredential, error) {
	seen := map[string]bool{}
	credentials := make([]extprocCredential, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Credential == nil {
			continue
		}
		ref := candidate.Credential.SecretRef
		if ref.Namespace != namespace {
			return nil, fmt.Errorf("candidate %s references cross-namespace Secret %s/%s", candidate.StableID, ref.Namespace, ref.Name)
		}
		if candidate.Credential.Strategy != "bearer_token" && candidate.Credential.Strategy != "apikey" {
			return nil, fmt.Errorf("candidate %s uses unsupported ExtProc credential strategy %q", candidate.StableID, candidate.Credential.Strategy)
		}
		if ref.Name == "" || ref.Key == "" {
			return nil, fmt.Errorf("candidate %s has an incomplete credential reference", candidate.StableID)
		}
		identity := ref.Namespace + "/" + ref.Name + "/" + ref.Key + "/" + candidate.Credential.Strategy
		if seen[identity] {
			continue
		}
		seen[identity] = true
		hash := sha256.Sum256([]byte(ref.Namespace + "/" + ref.Name + "/" + ref.Key))
		path := fmt.Sprintf("%s/%s-%s/%s", extprocCredentialDir, ref.Name, hex.EncodeToString(hash[:])[:12], ref.Key)
		credentials = append(credentials, extprocCredential{name: ref.Name, namespace: ref.Namespace, key: ref.Key, strategy: candidate.Credential.Strategy, file: path})
	}
	sort.Slice(credentials, func(i, j int) bool {
		return credentials[i].name+credentials[i].key+credentials[i].strategy < credentials[j].name+credentials[j].key+credentials[j].strategy
	})
	return credentials, nil
}

func extprocPostAuthConfig(candidates []envelope.Candidate, credentials []extprocCredential, meteringURL string, providerKinds []string) (string, error) {
	clusterSet := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		clusterSet[candidate.Cluster] = true
	}
	clusters := make([]string, 0, len(clusterSet))
	for cluster := range clusterSet {
		clusters = append(clusters, cluster)
	}
	sort.Strings(clusters)
	metering, err := meteringFiltersYAML(meteringURL)
	if err != nil {
		return "", err
	}
	// token_count feeds external_metering the per-provider token.* metadata it
	// needs; it is only wired when metering is on. providerKinds are the distinct
	// Praxis token_count ProviderKinds for the chain, already resolved (from the
	// same envelope authority as the routing strategy), deduped, and sorted by
	// runtimeCandidates — one filter per kind so each response format is parsed.
	var tokenCount string
	if meteringURL != "" {
		if tokenCount, err = filterEntriesYAML(tokenCountFilterEntries(providerKinds)); err != nil {
			return "", err
		}
	}
	var b strings.Builder
	b.WriteString("server:\n  grpc_address: \"0.0.0.0:9004\"\n  health_address: \"0.0.0.0:50052\"\n")
	b.WriteString("  metrics_address: \"0.0.0.0:9090\"\n  tls:\n    mode: self_signed\n\n")
	// Metering runs first so identity_header_guard captures and strips the
	// x-maas-* headers before intelligent_route forwards the request upstream.
	// token_count sits after external_metering (response hooks run in reverse,
	// so extraction happens before metering reads the counts) and before
	// intelligent_route.
	b.WriteString("filter_chains:\n  - name: post-auth\n    filters:\n")
	b.WriteString(metering)
	b.WriteString(tokenCount)
	b.WriteString("      - filter: intelligent_route\n")
	b.WriteString("        overlay_file: /etc/praxis/routing/routing-overlay.json\n")
	b.WriteString("        model_header: X-Gateway-Model-Name\n        provider_hop_clusters: [")
	for i, cluster := range clusters {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", cluster)
	}
	b.WriteString("]\n        reload: {enabled: true, debounce_ms: 500}\n")
	if len(credentials) > 0 {
		b.WriteString("      - filter: credential_inject\n        credentials:\n")
		for _, credential := range credentials {
			fmt.Fprintf(&b, "          - name: %s\n            namespace: %s\n", credential.name, credential.namespace)
			fmt.Fprintf(&b, "            key: %s\n            strategy: %s\n            file: %s\n", credential.key, credential.strategy, credential.file)
		}
	}
	b.WriteString("\ninsecure_options:\n  allow_unbounded_body: true\n")
	return b.String(), nil
}

// meteringFilterEntries returns the identity_header_guard and external_metering
// filters, in chain order, that wire MaaS usage/billing metering into a
// post-auth Praxis chain. identity_header_guard must precede external_metering:
// it captures the Authorino-injected x-maas-* identity headers into
// filter_metadata (the namespace external_metering reads) and strips them from
// the upstream request. external_metering additionally needs the token.* usage
// metadata written by a companion token_count filter (see tokenCountFilterEntries);
// callers append those after these entries. Returns nil when no metering endpoint
// is configured, which disables metering entirely (opt-in).
func meteringFilterEntries(meteringURL string) []any {
	if meteringURL == "" {
		return nil
	}
	return []any{
		map[string]any{
			"filter":             "identity_header_guard",
			"prefix":             meteringIdentityHeaderPrefix,
			"metadata_namespace": meteringIdentityMetadataNamespace,
		},
		map[string]any{
			"filter":       "external_metering",
			"metering_url": meteringURL,
			// The metering service is an in-cluster Service (private address);
			// Praxis refuses non-public endpoints unless this is set.
			"allow_private_endpoint":      true,
			"timeout_seconds":             meteringTimeoutSeconds,
			"source":                      meteringCloudEventSource,
			"fail_open":                   true,
			"identity_header_prefix":      meteringIdentityHeaderPrefix,
			"identity_metadata_namespace": meteringIdentityMetadataNamespace,
		},
	}
}

// meteringFiltersYAML renders the metering filters as YAML list items indented
// to sit under a filter chain's "filters:" key (six-space indent). Returns the
// empty string when metering is not configured.
func meteringFiltersYAML(meteringURL string) (string, error) {
	return filterEntriesYAML(meteringFilterEntries(meteringURL))
}

// filterEntriesYAML renders filter entries as YAML list items indented to sit
// under a filter chain's "filters:" key (six-space indent). Returns the empty
// string for an empty slice.
func filterEntriesYAML(entries []any) (string, error) {
	if len(entries) == 0 {
		return "", nil
	}
	raw, err := yaml.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("marshal ExtProc filters: %w", err)
	}
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		b.WriteString("      ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String(), nil
}

// baseChainTokenCountProvider is the token_count ProviderKind for the shared
// KServe post-auth chain, which serves OpenAI-compatible (vLLM) inference.
const baseChainTokenCountProvider = "openai"

// tokenCountFilterEntries returns one token_count filter per distinct provider,
// in the given order. external_metering has no independent usage parser: it
// reads the token.* filter_metadata that token_count writes, so without a
// token_count whose provider matches the response format metering books zero
// tokens. token_count must be declared AFTER external_metering — response hooks
// run in reverse chain order, so extraction then executes before metering reads.
func tokenCountFilterEntries(providerKinds []string) []any {
	entries := make([]any, 0, len(providerKinds))
	for _, kind := range providerKinds {
		entries = append(entries, map[string]any{
			"filter":   "token_count",
			"provider": kind,
		})
	}
	return entries
}

// configureBaseExtProcMetering injects the metering filters into the shared
// payload-processing post-auth chain that serves ordinary KServe/MaaS traffic,
// and stamps a config hash so the Deployment rolls when the chain changes. It is
// a no-op when metering is not configured. The per-tenant ExternalModel chain is
// metered separately in extprocPostAuthConfig.
func configureBaseExtProcMetering(resources []unstructured.Unstructured, tenantID, meteringURL string) error {
	if meteringURL == "" {
		return nil
	}
	configMapName := PayloadProcessingPluginsConfigMapForTenant(tenantID)
	deploymentName := PayloadProcessingDeploymentName(tenantID)
	var configMap, deployment *unstructured.Unstructured
	for i := range resources {
		resource := &resources[i]
		if configMap == nil && resource.GetKind() == "ConfigMap" && resource.GetName() == configMapName {
			configMap = resource
		}
		if deployment == nil && resource.GetKind() == "Deployment" && resource.GetName() == deploymentName {
			deployment = resource
		}
	}
	if configMap == nil || deployment == nil {
		return errors.New("shared post-auth ExtProc resources are missing the plugin ConfigMap or Deployment")
	}
	data, found, err := unstructured.NestedStringMap(configMap.Object, "data")
	if err != nil {
		return fmt.Errorf("read shared ExtProc ConfigMap data: %w", err)
	}
	if !found {
		return errors.New("shared ExtProc ConfigMap data is missing")
	}
	config, found := data["extproc.yaml"]
	if !found {
		return errors.New("shared ExtProc ConfigMap is missing extproc.yaml")
	}
	metered, err := injectMeteringFilters(config, meteringURL)
	if err != nil {
		return fmt.Errorf("inject metering into shared post-auth chain: %w", err)
	}
	data["extproc.yaml"] = metered
	if err := unstructured.SetNestedStringMap(configMap.Object, data, "data"); err != nil {
		return fmt.Errorf("write shared ExtProc ConfigMap data: %w", err)
	}
	configHash := sha256.Sum256([]byte(metered))
	if err := unstructured.SetNestedField(deployment.Object, hex.EncodeToString(configHash[:]), "spec", "template", "metadata", "annotations", "ai-gateway-controller.opendatahub.io/extproc-config-sha256"); err != nil {
		return fmt.Errorf("write shared ExtProc config hash: %w", err)
	}
	return nil
}

// injectMeteringFilters parses a Praxis ExtProc config document and prepends the
// metering filters to its first filter chain. Prepending keeps identity_header_guard
// ahead of any filter that reads identity from request headers. token_count is
// appended after external_metering (still ahead of the pre-existing filters) so
// external_metering has the token.* metadata to report; the shared chain serves
// OpenAI-compatible KServe traffic, so one token_count with the openai provider
// covers it.
func injectMeteringFilters(config, meteringURL string) (string, error) {
	entries := meteringFilterEntries(meteringURL)
	if len(entries) == 0 {
		return config, nil
	}
	entries = append(entries, tokenCountFilterEntries([]string{baseChainTokenCountProvider})...)
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(config), &doc); err != nil {
		return "", fmt.Errorf("parse ExtProc config: %w", err)
	}
	chains, ok := doc["filter_chains"].([]any)
	if !ok || len(chains) == 0 {
		return "", errors.New("ExtProc config has no filter_chains")
	}
	chain, ok := chains[0].(map[string]any)
	if !ok {
		return "", errors.New("ExtProc first filter chain is malformed")
	}
	filters, _ := chain["filters"].([]any)
	chain["filters"] = append(append([]any{}, entries...), filters...)
	chains[0] = chain
	doc["filter_chains"] = chains
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal ExtProc config: %w", err)
	}
	return string(out), nil
}

func appendOrReplaceNamedVolume(items []any, name string, value map[string]any) []any {
	for i, item := range items {
		m, ok := item.(map[string]any)
		if ok && m["name"] == name {
			items[i] = value
			return items
		}
	}
	return append(items, value)
}
