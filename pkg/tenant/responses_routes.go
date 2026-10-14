package tenant

import (
	"context"
	"errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const responsesRouteBase = "responses-api"

var responsesRouteGVK = schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}

func (r *Reconciler) responsesRouteResources(
	ctx context.Context, owner *unstructured.Unstructured, tenantID, namespace, gatewayName, gatewayNamespace string,
) ([]unstructured.Unstructured, error) {
	if err := r.responsesGatewayExclusive(ctx, owner, gatewayName, gatewayNamespace); err != nil {
		return nil, err
	}
	routeName := ResourceName(responsesRouteBase, tenantID)
	sinkName := ResourceName("responses-api-sink", tenantID)
	rules := make([]any, 0, 2)
	for _, api := range []string{"responses", "conversations"} {
		path := "/v1/" + api
		rules = append(rules, map[string]any{
			"matches":     []any{map[string]any{"path": map[string]any{"type": "PathPrefix", "value": path}}},
			"backendRefs": []any{map[string]any{"name": sinkName, "port": int64(443)}},
			"timeouts":    map[string]any{"request": "300s"},
		})
	}
	hostnames, err := r.responsesHostnames(ctx, namespace)
	if err != nil {
		return nil, err
	}
	spec := map[string]any{
		"parentRefs": []any{map[string]any{"name": gatewayName, "namespace": gatewayNamespace}}, "rules": rules,
	}
	if len(hostnames) > 0 {
		spec["hostnames"] = hostnames
	}
	metadata := map[string]any{"name": routeName, "namespace": namespace}
	route := unstructured.Unstructured{Object: map[string]any{"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute", "metadata": metadata, "spec": spec}}
	sinkObject, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: sinkName, Namespace: namespace},
		Spec:       corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "https", Port: 443, TargetPort: intstr.FromInt(443)}}},
	})
	if err != nil {
		return nil, err
	}
	sink := unstructured.Unstructured{Object: sinkObject}
	return []unstructured.Unstructured{sink, route, responsesRouteFilter(tenantID, namespace, gatewayName, gatewayNamespace, len(rules))}, nil
}

// Standard paths are tenant-wide; sharing a Gateway would make routing ambiguous.
func (r *Reconciler) responsesGatewayExclusive(ctx context.Context, owner *unstructured.Unstructured, gatewayName, gatewayNamespace string) error {
	owners := &unstructured.UnstructuredList{}
	owners.SetGroupVersionKind(AITenantGVK.GroupVersion().WithKind("AITenantList"))
	if err := r.Client.List(ctx, owners); err != nil {
		return fmt.Errorf("check Responses Gateway ownership: %w", err)
	}
	for i := range owners.Items {
		other := &owners.Items[i]
		if other.GetName() == owner.GetName() && other.GetNamespace() == owner.GetNamespace() {
			continue
		}
		name, ns, ok := GatewayRef(other)
		if ok && name == gatewayName && ns == gatewayNamespace {
			return fmt.Errorf("responses requires a dedicated Gateway; also used by AITenant %s/%s", other.GetNamespace(), other.GetName())
		}
	}
	return nil
}

// Follow the existing model-route hostname workaround without inventing a DNS name.
func (r *Reconciler) responsesHostnames(ctx context.Context, namespace string) ([]any, error) {
	routes := &unstructured.UnstructuredList{}
	routes.SetGroupVersionKind(responsesRouteGVK.GroupVersion().WithKind("HTTPRouteList"))
	if err := r.Client.List(ctx, routes, client.InNamespace(namespace), client.HasLabels{"inference.opendatahub.io/external-model"}); err != nil {
		return nil, err
	}
	var hostnames []string
	for i := range routes.Items {
		if routes.Items[i].GetLabels()[LabelManagedBy] != ManagedByAIGatewayController {
			return nil, errors.New("responses model route is not controller-owned")
		}
		values, _, err := unstructured.NestedStringSlice(routes.Items[i].Object, "spec", "hostnames")
		if err != nil {
			return nil, err
		}
		slices.Sort(values)
		if i > 0 && !slices.Equal(hostnames, values) {
			return nil, errors.New("responses model routes have inconsistent hostnames")
		}
		hostnames = values
	}
	result := make([]any, len(hostnames))
	for i, hostname := range hostnames {
		result[i] = hostname
	}
	return result, nil
}

func responsesRouteFilter(tenantID, namespace, gatewayName, gatewayNamespace string, ruleCount int) unstructured.Unstructured {
	patches := make([]any, 0, ruleCount)
	for index := range ruleCount {
		patches = append(patches, map[string]any{
			"applyTo": "HTTP_ROUTE",
			"match": map[string]any{"context": "GATEWAY", "routeConfiguration": map[string]any{"vhost": map[string]any{"route": map[string]any{
				"name": fmt.Sprintf("%s.%s.%d", namespace, ResourceName(responsesRouteBase, tenantID), index),
			}}}},
			"patch": map[string]any{"operation": "MERGE", "value": map[string]any{"typed_per_filter_config": map[string]any{
				"envoy.filters.http.ext_proc.external-model-pre": responsesExtProcOverride("BUFFERED", "SKIP", "NONE"),
				"envoy.filters.http.ext_proc.external-model":     responsesExtProcOverride("BUFFERED", "SEND", "STREAMED"),
				"envoy.filters.http.ext_proc.ipp-pre":            map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExtProcPerRoute", "disabled": true},
				"envoy.filters.http.ext_proc.ipp":                map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExtProcPerRoute", "disabled": true},
			}}},
		})
	}
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "networking.istio.io/v1alpha3", "kind": "EnvoyFilter",
		"metadata": map[string]any{"name": ResourceName("responses-api-filters", tenantID), "namespace": gatewayNamespace},
		"spec": map[string]any{
			"priority": int64(20), "workloadSelector": map[string]any{"labels": map[string]any{"gateway.networking.k8s.io/gateway-name": gatewayName}},
			"configPatches": patches,
		},
	}}
}

func responsesExtProcOverride(requestBody, responseHeaders, responseBody string) map[string]any {
	return map[string]any{
		"@type": "type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExtProcPerRoute",
		"overrides": map[string]any{"processing_mode": map[string]any{
			"request_header_mode": "SEND", "request_body_mode": requestBody,
			"response_header_mode": responseHeaders, "response_body_mode": responseBody,
			"request_trailer_mode": "SKIP", "response_trailer_mode": "SKIP",
		}},
	}
}

func (r *Reconciler) cleanupResponsesRoutes(ctx context.Context, tenantID, namespace, gatewayNamespace string) error {
	for _, target := range []struct {
		gvk             schema.GroupVersionKind
		name, namespace string
	}{
		{responsesRouteGVK, ResourceName(responsesRouteBase, tenantID), namespace},
		{gvkEnvoyFilter, ResourceName("responses-api-filters", tenantID), gatewayNamespace},
		{gvkService, ResourceName("responses-api-sink", tenantID), namespace},
	} {
		if err := r.deleteResourceIfOwned(ctx, target.gvk, target.name, target.namespace); err != nil {
			return err
		}
	}
	return nil
}

// Retire an old stateful workload when a new binding/retention contract is invalid.
func (r *Reconciler) withdrawResponses(ctx context.Context, tenantID, namespace, gatewayNamespace string) error {
	if err := r.cleanupResponsesRoutes(ctx, tenantID, namespace, gatewayNamespace); err != nil {
		return err
	}
	deployment := &unstructured.Unstructured{}
	deployment.SetGroupVersionKind(gvkDeployment)
	name := PayloadProcessingExternalModelDeploymentName(tenantID)
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, deployment); err != nil {
		return client.IgnoreNotFound(err)
	}
	if deployment.GetAnnotations()["opendatahub.io/managed"] == "false" {
		return errors.New("cannot withdraw unmanaged responses workload")
	}
	revision, found, err := unstructured.NestedString(deployment.Object, "spec", "template", "metadata", "annotations", "ai-gateway-controller.opendatahub.io/responses-binding-revision")
	if err != nil {
		return err
	}
	if !found || revision == "" || deployment.GetLabels()[LabelManagedBy] != ManagedByAIGatewayController {
		return nil
	}
	if err := r.Client.Delete(ctx, deployment); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("withdraw responses workload: %w", err)
	}
	return nil
}
