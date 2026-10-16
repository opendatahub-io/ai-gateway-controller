/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/inference/v1alpha1"
	"github.com/opendatahub-io/ai-gateway-controller/pkg/envelope"
	"github.com/opendatahub-io/ai-gateway-controller/pkg/publisher"
	"github.com/opendatahub-io/ai-gateway-controller/pkg/resolver"
	"github.com/opendatahub-io/ai-gateway-controller/pkg/tenant"
)

const (
	withdrawalDrainAnnotation       = "inference.opendatahub.io/provider-withdrawal-drain"
	withdrawalRequestTimeoutSeconds = 300
	withdrawalDrainSafetyMarginSecs = 10
	extprocConfigHashAnnotation     = "ai-gateway-controller.opendatahub.io/extproc-config-sha256"
	externalModelExtProcContainer   = "payload-processing"
)

var extprocServingRevisionPattern = regexp.MustCompile(`accepted_revision.*?="([0-9a-f]{64})".*?serving_revision.*?="([0-9a-f]{64})"`)
var terminalEscapePattern = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

var (
	// Each generated provider route has a 300 second request timeout. Keep the
	// withdrawn backend available for that full interval after the last old
	// ExtProc endpoint disappears, plus a margin for reconciliation and clock
	// skew, so an already selected upstream request can finish before pruning.
	withdrawalDrainDuration   = time.Duration(withdrawalRequestTimeoutSeconds+withdrawalDrainSafetyMarginSecs) * time.Second
	withdrawalRequeueInterval = 5 * time.Second
)

type withdrawalDrainState struct {
	Revision  string    `json:"revision"`
	StartedAt time.Time `json:"startedAt"`
}

type pendingWithdrawalError struct {
	After time.Duration
}

func (e *pendingWithdrawalError) Error() string {
	return "ExternalModel provider withdrawal is waiting for serving endpoints and in-flight requests to drain"
}

// readPublishedEnvelope reads and verifies the last published routing state
// before any resource mutation. The previous candidates are the durable input
// needed to reconstruct a withdrawn route after a controller restart.
func (r *Reconciler) readPublishedEnvelope(ctx context.Context, namespace string) (*envelope.Envelope, error) {
	reader := r.APIReader
	if reader == nil {
		return nil, errors.New("APIReader is required to stage provider withdrawal")
	}
	var cm corev1.ConfigMap
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: r.configMapName()}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read published routing overlay %s/%s: %w", namespace, r.configMapName(), err)
	}
	if cm.Labels["app.kubernetes.io/managed-by"] != "ai-gateway-controller" {
		return nil, fmt.Errorf("routing overlay %s/%s is not owned by ai-gateway-controller", namespace, cm.Name)
	}
	raw := cm.Data[publisher.DefaultDataKey]
	if raw == "" {
		return nil, fmt.Errorf("%w: %s/%s has no %q key", publisher.ErrNoEnvelope, namespace, cm.Name, publisher.DefaultDataKey)
	}
	var env envelope.Envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return nil, fmt.Errorf("%w: %s/%s: %w", publisher.ErrEnvelopeTampered, namespace, cm.Name, err)
	}
	digest, err := envelope.ComputeDigestFromWire([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %s/%s: %w", publisher.ErrEnvelopeTampered, namespace, cm.Name, err)
	}
	if digest != env.Revision.Value {
		return nil, fmt.Errorf("%w: %s/%s declares %s, content hashes to %s", publisher.ErrEnvelopeTampered, namespace, cm.Name, env.Revision.Value, digest)
	}
	return &env, nil
}

// transportRoutesForPublication adds candidates from the previously
// published overlay and any already applied HTTPRoute to the desired routes.
// Additions therefore reach Envoy before publication, while withdrawals keep
// their old provider rule until the serving workload has drained.
func (r *Reconciler) transportRoutesForPublication(ctx context.Context, namespace string, desired []resolver.Route, previous *envelope.Envelope) ([]resolver.Route, bool, error) {
	byID := make(map[string]resolver.Route, len(desired))
	for _, route := range desired {
		byID[envelope.CandidateStableID(route.Model, route.Provider)] = route
	}
	union := append([]resolver.Route(nil), desired...)

	transportRoutes, err := r.readAppliedModelRoutes(ctx, namespace)
	if err != nil {
		return nil, false, err
	}
	for id, route := range transportRoutes {
		if _, current := byID[id]; current {
			continue
		}
		byID[id] = route
		union = append(union, route)
	}

	if previous != nil {
		for _, candidate := range previous.Overlay.Candidates {
			if candidate.Kind != "inference_model" {
				continue
			}
			if candidate.StableID == "" {
				return nil, false, fmt.Errorf("cannot stage inference_model candidate without stable_id in cluster %q", candidate.Cluster)
			}
			if _, current := byID[candidate.StableID]; current {
				continue
			}
			modelName, providerName, ok := splitCandidateStableID(candidate.StableID)
			if !ok || candidate.Cluster != providerServicePrefix+providerName {
				return nil, false, fmt.Errorf("cannot stage stale candidate %q with cluster %q", candidate.StableID, candidate.Cluster)
			}
			route, err := r.routeForWithdrawnCandidate(ctx, namespace, modelName, providerName, candidate.Name, transportRoutes)
			if err != nil {
				return nil, false, err
			}
			byID[candidate.StableID] = route
			union = append(union, route)
		}
	}

	currentIDs := make(map[string]bool, len(desired))
	for _, route := range desired {
		currentIDs[envelope.CandidateStableID(route.Model, route.Provider)] = true
	}
	stale := false
	for id := range byID {
		if !currentIDs[id] {
			stale = true
			break
		}
	}
	return union, stale, nil
}

func splitCandidateStableID(stableID string) (model, provider string, ok bool) {
	index := strings.LastIndex(stableID, "/provider-")
	if index <= 0 || index+len("/provider-") >= len(stableID) {
		return "", "", false
	}
	return stableID[:index], stableID[index+len("/provider-"):], true
}

func (r *Reconciler) routeForWithdrawnCandidate(ctx context.Context, namespace, modelName, providerName, clientName string, applied map[string]resolver.Route) (resolver.Route, error) {
	stableID := envelope.CandidateStableID(modelName, providerName)
	if route, ok := applied[stableID]; ok {
		if clientName != "" {
			route.ClientName = clientName
		}
		return route, nil
	}
	var model v1alpha1.ExternalModel
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: modelName}, &model); err != nil && !apierrors.IsNotFound(err) {
		return resolver.Route{}, fmt.Errorf("read ExternalModel %s/%s for withdrawn route: %w", namespace, modelName, err)
	}
	var provider v1alpha1.ExternalProvider
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: providerName}, &provider); err != nil {
		return resolver.Route{}, fmt.Errorf("read ExternalProvider %s/%s for withdrawn route: %w", namespace, providerName, err)
	}
	if clientName == "" {
		clientName = model.Spec.ModelName
		if clientName == "" {
			clientName = modelName
		}
	}
	return resolver.Route{
		Model: modelName, ClientName: clientName, Namespace: namespace,
		Provider: providerName, ProviderType: provider.Spec.Provider,
		Cluster: providerServicePrefix + providerName, Endpoint: provider.Spec.Endpoint,
		TLSCACertificates: strings.TrimSpace(provider.Spec.Config["tls.caCertificates"]),
	}, nil
}

// readAppliedModelRoutes parses only controller-owned model HTTPRoutes in the
// tenant namespace. These rules may outlive the published overlay while a
// staged withdrawal is draining.
func (r *Reconciler) readAppliedModelRoutes(ctx context.Context, namespace string) (map[string]resolver.Route, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRouteList"})
	if err := r.List(ctx, list, client.InNamespace(namespace), client.MatchingLabels{
		"app.kubernetes.io/managed-by": "ai-gateway-controller",
	}); err != nil {
		return nil, fmt.Errorf("list applied ExternalModel routes in %s: %w", namespace, err)
	}
	result := map[string]resolver.Route{}
	for i := range list.Items {
		route := &list.Items[i]
		modelName := route.GetLabels()["inference.opendatahub.io/external-model"]
		if modelName == "" {
			continue
		}
		rules, _, err := unstructured.NestedSlice(route.Object, "spec", "rules")
		if err != nil {
			return nil, fmt.Errorf("read rules from HTTPRoute %s/%s: %w", namespace, route.GetName(), err)
		}
		clientName := routeClientName(rules, namespace, modelName)
		for _, rawRule := range rules {
			rule, ok := rawRule.(map[string]any)
			if !ok {
				continue
			}
			stableID := providerStableIDFromRule(rule)
			refs, _, _ := unstructured.NestedSlice(rule, "backendRefs")
			if stableID == "" || len(refs) == 0 {
				continue
			}
			backend, ok := refs[0].(map[string]any)
			if !ok {
				continue
			}
			serviceName, _ := backend["name"].(string)
			if !strings.HasPrefix(serviceName, providerServicePrefix) || strings.HasPrefix(serviceName, providerSelectionSinkPrefix) {
				continue
			}
			candidateModel, providerName, ok := splitCandidateStableID(stableID)
			if !ok || candidateModel != modelName || serviceName != providerServicePrefix+providerName {
				continue
			}
			endpoint, err := providerEndpointFromRule(rule, backend)
			if err != nil {
				return nil, fmt.Errorf("read provider endpoint from HTTPRoute %s/%s: %w", namespace, route.GetName(), err)
			}
			if endpoint == "" {
				continue
			}
			result[stableID] = resolver.Route{
				Model: modelName, ClientName: clientName, Namespace: namespace,
				Provider: providerName, Cluster: providerServicePrefix + providerName,
				Endpoint: endpoint,
			}
		}
	}
	return result, nil
}

func (r *Reconciler) providerTransportStillInUse(ctx context.Context, namespace, providerName string) (bool, error) {
	env, err := r.readPublishedEnvelope(ctx, namespace)
	if err != nil {
		return false, err
	}
	cluster := providerServicePrefix + providerName
	if env != nil {
		for _, candidate := range env.Overlay.Candidates {
			if candidate.Cluster == cluster {
				return true, nil
			}
		}
	}
	routes, err := r.readAppliedModelRoutes(ctx, namespace)
	if err != nil {
		return false, err
	}
	for _, route := range routes {
		if route.Provider == providerName {
			return true, nil
		}
	}
	return false, nil
}

func providerStableIDFromRule(rule map[string]any) string {
	matches, _, _ := unstructured.NestedSlice(rule, "matches")
	for _, rawMatch := range matches {
		match, ok := rawMatch.(map[string]any)
		if !ok {
			continue
		}
		headers, _, _ := unstructured.NestedSlice(match, "headers")
		for _, rawHeader := range headers {
			header, ok := rawHeader.(map[string]any)
			if !ok {
				continue
			}
			name, _ := header["name"].(string)
			value, _ := header["value"].(string)
			if strings.EqualFold(name, selectedProviderHeader) {
				return value
			}
		}
	}
	return ""
}

func providerEndpointFromRule(rule, backend map[string]any) (string, error) {
	filters, _, _ := unstructured.NestedSlice(rule, "filters")
	host := ""
	for _, rawFilter := range filters {
		filter, ok := rawFilter.(map[string]any)
		if !ok {
			continue
		}
		if hostname, found, _ := unstructured.NestedString(filter, "urlRewrite", "hostname"); found && hostname != "" {
			host = hostname
			break
		}
	}
	if host == "" {
		return "", nil
	}
	port, found, err := unstructured.NestedInt64(backend, "port")
	if err != nil {
		return "", fmt.Errorf("read backendRef port: %w", err)
	}
	if !found {
		port = 443
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("backendRef port %d is outside the valid range", port)
	}
	if port == 443 {
		return host, nil
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), strconv.FormatInt(port, 10)), nil
}

func routeClientName(rules []any, namespace, model string) string {
	prefix := "/" + namespace + "/"
	for _, rawRule := range rules {
		rule, ok := rawRule.(map[string]any)
		if !ok {
			continue
		}
		matches, _, _ := unstructured.NestedSlice(rule, "matches")
		for _, rawMatch := range matches {
			match, ok := rawMatch.(map[string]any)
			if !ok {
				continue
			}
			path, found, _ := unstructured.NestedString(match, "path", "value")
			if found && strings.HasPrefix(path, prefix) {
				return strings.TrimPrefix(path, prefix)
			}
		}
	}
	return model
}

func (r *Reconciler) externalModelRolloutSafe(ctx context.Context, namespace, tenantID, expectedConfigHash string) (bool, error) {
	if r.readPodLogs == nil {
		return false, errors.New("pod log reader is required to verify ExternalModel serving overlay revisions")
	}
	var overlayRevision string
	env, err := r.readPublishedEnvelope(ctx, namespace)
	if err != nil {
		return false, err
	}
	if env == nil || env.Revision.Value == "" {
		return false, nil
	}
	overlayRevision = env.Revision.Value
	deployment, replicas, safe, err := r.externalModelDeploymentSafe(ctx, namespace, tenantID, expectedConfigHash)
	if err != nil || !safe {
		return safe, err
	}
	currentPods, safe, err := r.externalModelPodsSafe(ctx, namespace, deployment, replicas, expectedConfigHash, overlayRevision)
	if err != nil || !safe {
		return safe, err
	}
	return r.externalModelEndpointsSafe(ctx, namespace, tenantID, currentPods)
}

func (r *Reconciler) finishProviderWithdrawal(ctx context.Context, namespace, tenantID, gatewayName, gatewayNamespace string,
	models []*v1alpha1.ExternalModel, owners map[string]*v1alpha1.ExternalModel, desired []resolver.Route, revision string, pending bool) (time.Duration, error) {
	if !pending {
		return 0, r.clearWithdrawalDrainState(ctx, namespace)
	}
	runtimeHash, err := runtimeConfigHash(ctx, r, models, namespace)
	if err != nil {
		return 0, err
	}
	safe, err := r.externalModelRolloutSafe(ctx, namespace, tenantID, runtimeHash)
	if err != nil {
		return 0, err
	}
	remaining, err := r.withdrawalDrainRemaining(ctx, namespace, revision, safe, time.Now())
	if err != nil || remaining > 0 {
		return remaining, err
	}
	if err := r.applyTransport(ctx, desired, namespace, tenantID, gatewayName, gatewayNamespace, owners); err != nil {
		return 0, err
	}
	if err := r.cleanupExternalModelRouteFilters(ctx, namespace, tenantID, gatewayName, gatewayNamespace); err != nil {
		return 0, err
	}
	if err := r.cleanupModelTransport(ctx, namespace, desired); err != nil {
		return 0, err
	}
	return 0, r.clearWithdrawalDrainState(ctx, namespace)
}

func (r *Reconciler) externalModelDeploymentSafe(ctx context.Context, namespace, tenantID, expectedConfigHash string) (*appsv1.Deployment, int32, bool, error) {
	reader := r.APIReader
	if reader == nil {
		return nil, 0, false, errors.New("APIReader is required to verify ExternalModel serving endpoints")
	}
	deploymentName := tenant.PayloadProcessingExternalModelDeploymentName(tenantID)
	deployment := &appsv1.Deployment{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: deploymentName}, deployment); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, 0, false, nil
		}
		return nil, 0, false, fmt.Errorf("read ExternalModel ExtProc Deployment %s/%s: %w", namespace, deploymentName, err)
	}
	if deployment.Labels["app.kubernetes.io/managed-by"] != "ai-gateway-controller" || deployment.Labels["app"] != tenant.PayloadProcessingExternalModelName {
		return nil, 0, false, nil
	}
	templateHash := deployment.Spec.Template.Annotations[extprocConfigHashAnnotation]
	if expectedConfigHash == "" || templateHash != expectedConfigHash {
		return nil, 0, false, nil
	}
	var configMap corev1.ConfigMap
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: tenant.PayloadProcessingExternalModelPluginsConfigMapForTenant(tenantID)}, &configMap); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, 0, false, nil
		}
		return nil, 0, false, fmt.Errorf("read ExternalModel ExtProc config: %w", err)
	}
	if configMap.Labels["app.kubernetes.io/managed-by"] != "ai-gateway-controller" {
		return nil, 0, false, nil
	}
	extprocConfig := configMap.Data["extproc.yaml"]
	if extprocConfig == "" {
		return nil, 0, false, nil
	}
	configDigest := sha256.Sum256([]byte(extprocConfig))
	if hex.EncodeToString(configDigest[:]) != expectedConfigHash {
		return nil, 0, false, nil
	}
	if deployment.Status.ObservedGeneration != deployment.Generation {
		return nil, 0, false, nil
	}
	replicas := int32(1)
	if deployment.Spec.Replicas != nil {
		replicas = *deployment.Spec.Replicas
	}
	if replicas <= 0 || deployment.Status.Replicas != replicas || deployment.Status.UpdatedReplicas != replicas ||
		deployment.Status.ReadyReplicas != replicas || deployment.Status.AvailableReplicas != replicas || deployment.Status.UnavailableReplicas != 0 {
		return nil, 0, false, nil
	}
	if deployment.Status.TerminatingReplicas != nil && *deployment.Status.TerminatingReplicas != 0 {
		return nil, 0, false, nil
	}
	if deployment.Spec.Selector == nil || len(deployment.Spec.Selector.MatchLabels) == 0 {
		return nil, 0, false, nil
	}
	return deployment, replicas, true, nil
}

func (r *Reconciler) externalModelPodsSafe(ctx context.Context, namespace string, deployment *appsv1.Deployment, replicas int32, expectedConfigHash, expectedOverlayRevision string) (map[types.UID]bool, bool, error) {
	reader := r.APIReader
	selector := client.MatchingLabels(deployment.Spec.Selector.MatchLabels)
	var replicaSets appsv1.ReplicaSetList
	if err := reader.List(ctx, &replicaSets, client.InNamespace(namespace), selector); err != nil {
		return nil, false, fmt.Errorf("list ExternalModel ExtProc ReplicaSets: %w", err)
	}
	ownedReplicaSets := make([]*appsv1.ReplicaSet, 0, len(replicaSets.Items))
	for i := range replicaSets.Items {
		rs := &replicaSets.Items[i]
		if metav1.GetControllerOf(rs) != nil && metav1.GetControllerOf(rs).UID == deployment.UID {
			ownedReplicaSets = append(ownedReplicaSets, rs)
		}
	}
	var currentRS *appsv1.ReplicaSet
	var currentRevision int64 = -1
	for _, rs := range ownedReplicaSets {
		revision, _ := strconv.ParseInt(rs.Annotations["deployment.kubernetes.io/revision"], 10, 64)
		if revision > currentRevision {
			currentRevision, currentRS = revision, rs
		}
	}
	if currentRS == nil || currentRS.Spec.Template.Annotations[extprocConfigHashAnnotation] != expectedConfigHash ||
		currentRS.Status.Replicas != replicas || currentRS.Status.ReadyReplicas != replicas || currentRS.Status.AvailableReplicas != replicas {
		return nil, false, nil
	}
	for _, rs := range ownedReplicaSets {
		if rs.UID == currentRS.UID {
			continue
		}
		if rs.Status.Replicas != 0 || rs.Status.ReadyReplicas != 0 || rs.Status.AvailableReplicas != 0 {
			return nil, false, nil
		}
	}

	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(namespace), selector); err != nil {
		return nil, false, fmt.Errorf("list ExternalModel ExtProc pods: %w", err)
	}
	currentPods := map[types.UID]bool{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		owner := metav1.GetControllerOf(pod)
		if owner == nil {
			return nil, false, nil
		}
		belongsToDeployment := false
		for _, rs := range ownedReplicaSets {
			if owner.UID == rs.UID {
				belongsToDeployment = true
				if rs.UID != currentRS.UID || !pod.DeletionTimestamp.IsZero() || !podReady(pod) {
					return nil, false, nil
				}
				servesExpectedRevision, err := r.podServesOverlayRevision(ctx, pod, expectedOverlayRevision)
				if err != nil {
					return nil, false, err
				}
				if !servesExpectedRevision {
					return nil, false, nil
				}
				currentPods[pod.UID] = true
				break
			}
		}
		if !belongsToDeployment {
			return nil, false, nil
		}
	}
	if len(currentPods) != int(replicas) {
		return nil, false, nil
	}
	return currentPods, true, nil
}

func (r *Reconciler) podServesOverlayRevision(ctx context.Context, pod *corev1.Pod, expectedRevision string) (bool, error) {
	logs, err := r.readPodLogs(ctx, pod.Namespace, pod.Name, externalModelExtProcContainer)
	if err != nil {
		return false, fmt.Errorf("read ExternalModel ExtProc logs for pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	lines := strings.Split(logs, "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		line := terminalEscapePattern.ReplaceAllString(lines[index], "")
		if !strings.Contains(line, "accepted_revision") {
			continue
		}
		match := extprocServingRevisionPattern.FindStringSubmatch(line)
		return len(match) == 3 && match[1] == expectedRevision && match[2] == expectedRevision, nil
	}
	return false, nil
}

func (r *Reconciler) externalModelEndpointsSafe(ctx context.Context, namespace, tenantID string, currentPods map[types.UID]bool) (bool, error) {
	reader := r.APIReader
	serviceName := tenant.PayloadProcessingExternalModelServiceName(tenantID)
	var slices discv1.EndpointSliceList
	if err := reader.List(ctx, &slices, client.InNamespace(namespace), client.MatchingLabels{"kubernetes.io/service-name": serviceName}); err != nil {
		return false, fmt.Errorf("list ExternalModel ExtProc EndpointSlices: %w", err)
	}
	if len(slices.Items) == 0 {
		return false, nil
	}
	servingPods := map[types.UID]bool{}
	for i := range slices.Items {
		for _, endpoint := range slices.Items[i].Endpoints {
			readyOrUnknown := endpoint.Conditions.Ready == nil || *endpoint.Conditions.Ready
			serving := endpoint.Conditions.Serving != nil && *endpoint.Conditions.Serving
			if !readyOrUnknown && !serving {
				continue
			}
			if endpoint.TargetRef == nil || endpoint.TargetRef.Kind != "Pod" || endpoint.TargetRef.UID == "" || !currentPods[endpoint.TargetRef.UID] {
				return false, nil
			}
			servingPods[endpoint.TargetRef.UID] = true
		}
	}
	if len(servingPods) != len(currentPods) {
		return false, nil
	}
	return true, nil
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (r *Reconciler) withdrawalDrainRemaining(ctx context.Context, namespace, revision string, safe bool, now time.Time) (time.Duration, error) {
	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: namespace, Name: r.configMapName()}
	if err := r.APIReader.Get(ctx, key, &cm); err != nil {
		return 0, fmt.Errorf("read withdrawal drain state %s: %w", key, err)
	}
	if cm.Labels["app.kubernetes.io/managed-by"] != "ai-gateway-controller" {
		return 0, fmt.Errorf("withdrawal drain state ConfigMap %s is not controller-owned", key)
	}
	var state withdrawalDrainState
	stateExists := false
	if raw := cm.Annotations[withdrawalDrainAnnotation]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &state); err == nil && state.Revision != "" && !state.StartedAt.IsZero() {
			stateExists = true
		}
	}
	if !safe {
		if stateExists {
			if err := r.patchWithdrawalDrainState(ctx, &cm, ""); err != nil {
				return 0, err
			}
		}
		return withdrawalRequeueInterval, nil
	}
	if !stateExists || state.Revision != revision {
		state = withdrawalDrainState{Revision: revision, StartedAt: now.UTC()}
		encoded, err := json.Marshal(state)
		if err != nil {
			return 0, fmt.Errorf("encode withdrawal drain state: %w", err)
		}
		if err := r.patchWithdrawalDrainState(ctx, &cm, string(encoded)); err != nil {
			return 0, err
		}
		return withdrawalDrainDuration, nil
	}
	remaining := withdrawalDrainDuration - now.Sub(state.StartedAt)
	if remaining > 0 {
		return remaining, nil
	}
	return 0, nil
}

func (r *Reconciler) clearWithdrawalDrainState(ctx context.Context, namespace string) error {
	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: namespace, Name: r.configMapName()}
	if err := r.APIReader.Get(ctx, key, &cm); err != nil {
		return client.IgnoreNotFound(err)
	}
	if cm.Annotations[withdrawalDrainAnnotation] == "" {
		return nil
	}
	if cm.Labels["app.kubernetes.io/managed-by"] != "ai-gateway-controller" {
		return fmt.Errorf("withdrawal drain state ConfigMap %s is not controller-owned", key)
	}
	return r.patchWithdrawalDrainState(ctx, &cm, "")
}

func (r *Reconciler) patchWithdrawalDrainState(ctx context.Context, cm *corev1.ConfigMap, value string) error {
	base := cm.DeepCopy()
	if cm.Annotations == nil {
		cm.Annotations = map[string]string{}
	}
	if value == "" {
		delete(cm.Annotations, withdrawalDrainAnnotation)
	} else {
		cm.Annotations[withdrawalDrainAnnotation] = value
	}
	if err := r.Patch(ctx, cm, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("persist withdrawal drain state on %s/%s: %w", cm.Namespace, cm.Name, err)
	}
	return nil
}

func runtimeConfigHash(ctx context.Context, r *Reconciler, models []*v1alpha1.ExternalModel, namespace string) (string, error) {
	var providerList v1alpha1.ExternalProviderList
	if err := r.List(ctx, &providerList, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("list ExternalProviders for ExtProc runtime config: %w", err)
	}
	providers := make([]*v1alpha1.ExternalProvider, 0, len(providerList.Items))
	for i := range providerList.Items {
		if providerList.Items[i].DeletionTimestamp.IsZero() {
			providers = append(providers, &providerList.Items[i])
		}
	}
	routes, err := resolver.ResolveAll(models, providers)
	if err != nil {
		return "", fmt.Errorf("resolve current ExternalModel ExtProc runtime routes: %w", err)
	}
	candidates, err := tenant.ExternalModelRuntimeCandidates(routes)
	if err != nil {
		return "", err
	}
	hash, err := tenant.ExternalModelExtProcConfigHash(namespace, candidates)
	if err != nil {
		return "", fmt.Errorf("compute current ExternalModel ExtProc config hash: %w", err)
	}
	return hash, nil
}
