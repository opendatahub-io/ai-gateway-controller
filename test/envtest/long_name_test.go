//go:build envtest

package envtest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/inference/v1alpha1"
	"github.com/opendatahub-io/ai-gateway-controller/pkg/tenant"
)

// TestLongNamespaceReconciliation exercises the full controller reconciliation
// loop with a tenant namespace name long enough to push child resource names
// past the 63-character Kubernetes limit, verifying that kmeta.ChildName
// hashing produces valid names end-to-end.
func TestLongNamespaceReconciliation(t *testing.T) {
	c := startControlPlane(t)

	// 40-character namespace: "payload-processing-external-model-" (34 chars)
	// + 40 chars = 74, well past the 63-character Kubernetes name limit.
	const namespace = "long-namespace-name-exceeding-k8s-limits"

	maas := maasFixture{client: c}
	maas.existingPraxisTenant(t, namespace)

	// awaitPraxisInstalled hardcodes simple concatenation for resource
	// names; use tenant naming functions that handle the 63-char hash.
	tenantID := tenant.ID(namespace)
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		var deployment appsv1.Deployment
		require.NoError(collect, c.Get(t.Context(), client.ObjectKey{
			Namespace: gatewayNamespace,
			Name:      tenant.PayloadProcessingDeploymentName(tenantID),
		}, &deployment))
		require.NotEmpty(collect, deployment.Spec.Template.Spec.Containers)
	}, eventuallyTimeout, pollInterval, "praxis must be installed for the long-named tenant")

	provider := createProvider(t, c, namespace)

	model := externalModel("model", namespace,
		withModelName("chat"),
		withProviderModel(provider.Name, "provider-model"),
	)
	require.NoError(t, c.Create(t.Context(), model))

	// The model must reach Ready, proving the controller reconciled all
	// child resources despite some names requiring hash truncation.
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		var actual inferencev1alpha1.ExternalModel
		require.NoError(collect, c.Get(t.Context(), client.ObjectKeyFromObject(model), &actual))
		require.True(collect, apimeta.IsStatusConditionTrue(actual.Status.Conditions, "Ready"),
			"model conditions: %+v", actual.Status.Conditions)
	}, eventuallyTimeout, pollInterval, "model must reach Ready with a long namespace name")

	// Verify that all Deployments, Services, and ServiceAccounts created
	// for this tenant respect the 63-character Kubernetes name limit.
	for _, ns := range []string{namespace, gatewayNamespace} {
		var deployments appsv1.DeploymentList
		require.NoError(t, c.List(t.Context(), &deployments, client.InNamespace(ns)))
		for _, d := range deployments.Items {
			assert.LessOrEqual(t, len(d.Name), 63,
				"Deployment %q in %q exceeds 63 chars", d.Name, ns)
		}

		var services corev1.ServiceList
		require.NoError(t, c.List(t.Context(), &services, client.InNamespace(ns)))
		for _, s := range services.Items {
			assert.LessOrEqual(t, len(s.Name), 63,
				"Service %q in %q exceeds 63 chars", s.Name, ns)
		}

		var serviceAccounts corev1.ServiceAccountList
		require.NoError(t, c.List(t.Context(), &serviceAccounts, client.InNamespace(ns)))
		for _, sa := range serviceAccounts.Items {
			assert.LessOrEqual(t, len(sa.Name), 63,
				"ServiceAccount %q in %q exceeds 63 chars", sa.Name, ns)
		}
	}
}
