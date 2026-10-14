//go:build envtest

package envtest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/opendatahub-io/ai-gateway-controller/pkg/tenant"
)

func TestResponsesControlPlane(t *testing.T) {
	c := startControlPlane(t)
	// The pinned MaaS fixture predates Responses. This permits the consumed field
	// in this local control plane; MaaS owns production admission/defaulting tests.
	var crd apiextensionsv1.CustomResourceDefinition
	schemaObject := &unstructured.Unstructured{}
	schemaObject.SetAPIVersion("apiextensions.k8s.io/v1")
	schemaObject.SetKind("CustomResourceDefinition")
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: "aitenants.maas.opendatahub.io"}, schemaObject))
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(schemaObject.Object, &crd))
	for i := range crd.Spec.Versions {
		schema := crd.Spec.Versions[i].Schema.OpenAPIV3Schema
		spec := schema.Properties["spec"]
		spec.Properties["responses"] = apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: ptr.To(true)}
		schema.Properties["spec"] = spec
	}
	var err error
	schemaObject.Object, err = runtime.DefaultUnstructuredConverter.ToUnstructured(&crd)
	require.NoError(t, err)
	require.NoError(t, c.Update(t.Context(), schemaObject))
	const namespace = "responses-test"
	(maasFixture{client: c}).existingPraxisTenant(t, namespace)
	awaitPraxisInstalled(t, c, namespace)
	assertExternalModelIsolation(t, c, namespace)
	require.NoError(t, c.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "responses-infra"}}))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "responses-db-config", Namespace: "responses-infra"},
		Data:       map[string][]byte{"DB_CONNECTION_URL": []byte("postgresql://responses@postgres.responses-infra.svc/responses"), "tls.crt": []byte("fixture-only-cert"), "tls.key": []byte("fixture-only-key")},
	}
	require.NoError(t, c.Create(t.Context(), secret))
	ca := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "responses-db-ca", Namespace: "responses-infra"}, Data: map[string][]byte{"ca.crt": []byte("fixture-only-ca")}}
	require.NoError(t, c.Create(t.Context(), ca))
	setResponsesSpec(t, c, namespace, map[string]any{"enabled": true})
	routeKey := client.ObjectKey{Namespace: namespace, Name: "responses-api-" + namespace}
	deploymentKey := client.ObjectKey{Namespace: namespace, Name: tenant.PayloadProcessingExternalModelDeploymentName(namespace)}
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		route := &unstructured.Unstructured{}
		route.SetAPIVersion("gateway.networking.k8s.io/v1")
		route.SetKind("HTTPRoute")
		require.NoError(collect, c.Get(t.Context(), routeKey, route))
		var deployment appsv1.Deployment
		require.NoError(collect, c.Get(t.Context(), deploymentKey, &deployment))
		require.Len(collect, deployment.Spec.Template.Spec.InitContainers, 1)
		require.NotNil(collect, deployment.Spec.Template.Spec.Containers[0].ReadinessProbe.GRPC)
		require.Contains(collect, deployment.Spec.Template.Annotations, "ai-gateway-controller.opendatahub.io/responses-binding-revision")
		var projection corev1.Secret
		require.NoError(collect, c.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: "responses-runtime-" + namespace}, &projection))
		require.Equal(collect, ca.Data["ca.crt"], projection.Data["ca.crt"])
		owner := tenant.NewAITenant()
		require.NoError(collect, c.Get(t.Context(), client.ObjectKey{Namespace: maasNamespace, Name: namespace}, owner))
		var config corev1.ConfigMap
		require.NoError(collect, c.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: tenant.PayloadProcessingExternalModelPluginsConfigMapForTenant(namespace)}, &config))
		require.Contains(collect, config.Data["extproc.yaml"], "tenant: {static: \""+string(owner.GetUID())+"\"}")
		for _, api := range []string{"responses", "conversations"} {
			require.Contains(collect, config.Data["extproc.yaml"], "filter: openai_conversations\n        conditions: [{when: {path_prefix: /v1/"+api+"}}]")
		}
	}, eventuallyTimeout, pollInterval)
	assertExternalModelIsolation(t, c, namespace)
	// Metadata watch must roll the init-container's key snapshot on Secret changes.
	var before appsv1.Deployment
	require.NoError(t, c.Get(t.Context(), deploymentKey, &before))
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(secret), secret))
	secret.Data["tls.key"] = []byte("rotated-fixture-only-key")
	require.NoError(t, c.Update(t.Context(), secret))
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		var deployment appsv1.Deployment
		require.NoError(collect, c.Get(t.Context(), deploymentKey, &deployment))
		const revision = "ai-gateway-controller.opendatahub.io/responses-binding-revision"
		require.NotEqual(collect, before.Spec.Template.Annotations[revision], deployment.Spec.Template.Annotations[revision])
		var projection corev1.Secret
		require.NoError(collect, c.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: "responses-runtime-" + namespace}, &projection))
		require.Equal(collect, secret.Data["tls.key"], projection.Data["tls.key"])
	}, eventuallyTimeout, pollInterval)
	setResponsesSpec(t, c, namespace, map[string]any{"enabled": true, "retention": map[string]any{"maxAge": "1h"}})
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		route := &unstructured.Unstructured{}
		route.SetAPIVersion("gateway.networking.k8s.io/v1")
		route.SetKind("HTTPRoute")
		require.True(collect, apierrors.IsNotFound(c.Get(t.Context(), routeKey, route)), "unsupported retention must withdraw API routes")
		require.True(collect, apierrors.IsNotFound(c.Get(t.Context(), deploymentKey, &appsv1.Deployment{})), "old stateful readiness must not survive invalid retention")
	}, eventuallyTimeout, pollInterval)
	setResponsesSpec(t, c, namespace, map[string]any{"enabled": false})
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		require.True(collect, apierrors.IsNotFound(c.Get(t.Context(), deploymentKey, &appsv1.Deployment{})), "no models or Responses remain")
	}, eventuallyTimeout, pollInterval)
	assertExternalModelIsolation(t, c, namespace)
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(secret), &corev1.Secret{}), "Retain keeps database credentials")
}

func setResponsesSpec(t *testing.T, c client.Client, namespace string, spec map[string]any) {
	t.Helper()
	owner := tenant.NewAITenant()
	key := client.ObjectKey{Namespace: maasNamespace, Name: namespace}
	require.NoError(t, c.Get(t.Context(), key, owner))
	require.NoError(t, unstructured.SetNestedMap(owner.Object, spec, "spec", "responses"))
	require.NoError(t, c.Update(t.Context(), owner))
	conditions, _, err := unstructured.NestedSlice(owner.Object, "status", "conditions")
	require.NoError(t, err)
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		require.True(t, ok)
		condition["observedGeneration"] = owner.GetGeneration()
	}
	require.NoError(t, unstructured.SetNestedSlice(owner.Object, conditions, "status", "conditions"))
	require.NoError(t, c.Status().Update(t.Context(), owner))
}
