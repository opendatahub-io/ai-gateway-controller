//go:build envtest

package envtest_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	netwv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/opendatahub-io/ai-gateway-controller/pkg/tenant"
)

func TestControlPlane(t *testing.T) {
	c := startControlPlane(t)

	t.Run("guardrail timeouts remain readable by typed clients", func(t *testing.T) {
		testGuardrailTimeoutAdmission(t, c)
	})

	t.Run("adding and deleting an external model manages its routing", func(t *testing.T) {
		// Given an existing Praxis tenant with a configured provider.
		const namespace = "model-publication"
		maas := maasFixture{client: c}
		maas.existingPraxisTenant(t, namespace)
		awaitPraxisInstalled(t, c, namespace)
		assertExternalModelIsolation(t, c, namespace)
		provider := createProvider(t, c, namespace)

		// When a user adds a model using that provider.
		model := externalModel("model", namespace,
			withModelName("chat"),
			withProviderModel(provider.Name, "provider-model"),
		)
		require.NoError(t, c.Create(t.Context(), model))

		// Then the requested model/provider mapping and its workload are configured.
		routeName := awaitModelPublished(t, c, model, provider)
		assertExternalModelIsolation(t, c, namespace)

		// When the user deletes that model.
		require.NoError(t, c.Delete(t.Context(), model))

		// Then its routing and workload are removed and the finalizer releases it.
		awaitModelRemoved(t, c, model, routeName)
		assertExternalModelIsolation(t, c, namespace)
	})
}

func assertExternalModelIsolation(t *testing.T, c client.Client, namespace string) {
	t.Helper()
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		var policies netwv1.NetworkPolicyList
		require.NoError(collect, c.List(t.Context(), &policies, client.InNamespace(namespace)))
		podLabels := labels.Set{tenant.LabelTenantInstance: tenant.PayloadProcessingExternalModelDeploymentName(namespace)}
		selected := []string{}
		for _, policy := range policies.Items {
			selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.PodSelector)
			require.NoError(collect, err)
			if selector.Matches(podLabels) {
				selected = append(selected, policy.Name)
				require.NotEmpty(collect, policy.Spec.Ingress)
				require.Len(collect, policy.Spec.Ingress[0].From, 1)
				require.Equal(collect, map[string]string{"gateway.networking.k8s.io/gateway-name": gatewayName}, policy.Spec.Ingress[0].From[0].PodSelector.MatchLabels)
			}
		}
		require.Equal(collect, []string{"external-model-extproc-" + namespace}, selected, "only the dedicated policy may select active or draining ExtProc pods")
	}, eventuallyTimeout, pollInterval)
}
