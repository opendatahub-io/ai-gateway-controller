/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package guardrail

import (
	"fmt"
	"net/url"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// evaluateProviderReady resolves the referenced NeMo server's checks endpoint,
// returning it alongside the condition reason and message to publish.
//
// What this establishes is endpoint *discovery*, not liveness. TrustyAI
// publishes status.endpoint as the in-cluster Service URL
// (https://<name>.<namespace>.svc.cluster.local) and writes it from both its
// ready and its not-ready status paths, so a published endpoint proves the
// server is addressable and authenticated — not that its pods are up. That is
// precisely the gate the guardrails proposal specifies: "Missing endpoint
// discovery reports ProviderReady=False and prevents activation"
// (02-guardrails-low-level-details.md:1339). The success message says what was
// actually established, so ProviderReady=True is not mistaken for a probe.
//
// status.phase and the DeploymentReady condition are still deliberately NOT
// consulted; TrustyAI PR #977 added the endpoint without touching either.
// Upstream's controllers/utils/deployment.go CheckDeploymentReady passes a nil
// *appsv1.Deployment to client.Get so nothing is ever populated, returns an
// error from its retry closure on every path including success (discarding
// CheckPodsReady's verdict), and then ends in an unconditional
// "return true, nil" that never inspects the error — so it always reports
// ready. Its caller, controllers/nemo_guardrails/status.go, ANDs that with
// routeReady to set phase, meaning a NemoGuardrails with spec.exposeRoute
// unset or false reports phase Ready unconditionally, whatever its pods are
// doing. That is RHAI-4448 / RHOAIENG-96107, and it is why those two fields
// cannot gate activation even now that discovery exists.
//
// The OpenShift Route was evaluated as an endpoint source and rejected; the
// merged contract agrees, publishing the Service URL and leaving the Route out
// of it. The Route exists only when the optional spec.exposeRoute is true, it
// is an external ingress path with edge or reencrypt TLS while ExtProc calls
// the server in-cluster, and its condition is set to False with reason
// RouteDisabled when not exposed — which a naive True check would read as
// failure.
//
// An absent endpoint is reported uniformly, whatever the cause. The common one
// is authentication being switched off: upstream writes the field only when
// utils.RequiresAuth holds, so security.opendatahub.io/enable-auth: "false"
// clears it. Re-reading that annotation here to word a more specific message
// would put a second interpretation of an upstream predicate in this package,
// and the proposal asks for authenticated internal connectivity regardless
// (:1338) — an unauthenticated server is not one a binding should activate
// against.
//
// The value is parsed rather than trusted. status.endpoint is owned by another
// controller, and anything that is not an absolute https URL with a host is a
// target ExtProc cannot call; rejecting it here fails closed instead of
// handing a malformed or plaintext address downstream.
func evaluateProviderReady(nemo *unstructured.Unstructured) (ready bool, endpoint, reason, message string) {
	const undiscovered = "the referenced NemoGuardrails provider publishes no status.endpoint, so no supported " +
		"checks endpoint could be discovered"

	if nemo == nil {
		return false, "", reasonEndpointDiscoveryUnavailable, undiscovered
	}

	discovered, found, err := unstructured.NestedString(nemo.Object, "status", "endpoint")
	if err != nil || !found || discovered == "" {
		return false, "", reasonEndpointDiscoveryUnavailable, undiscovered
	}

	// A DNS name is not a credential, so the rejected value is echoed: without
	// it the only actionable detail — what upstream actually wrote — is lost.
	parsed, err := url.Parse(discovered)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return false, "", reasonEndpointDiscoveryUnavailable, fmt.Sprintf(
			"the referenced NemoGuardrails provider publishes a status.endpoint that is not an absolute "+
				"https URL (%q), so no supported checks endpoint could be discovered", discovered)
	}

	return true, discovered, reasonProviderAvailable,
		"referenced NemoGuardrails provider publishes an authenticated in-cluster checks endpoint; " +
			"this confirms discovery, not that the server is serving"
}
