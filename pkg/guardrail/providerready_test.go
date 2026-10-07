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
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// nemoEndpoint is the address TrustyAI publishes for the standard fixture,
// formatted the way controllers/nemo_guardrails/status.go does.
const nemoEndpoint = "https://" + nemoName + "." + providerNS + ".svc.cluster.local"

// TestEvaluateProviderReadyGatesOnTheDiscoveredEndpoint covers the contract
// TrustyAI merged in PR #977: status.endpoint is the sole input, and
// everything else the CR reports is irrelevant to the verdict.
//
// The two cases worth reading carefully are the crossed ones. A provider with
// phase Ready and a True DeploymentReady condition but no endpoint must still
// be refused, and a provider with an endpoint and no phase at all must still
// be accepted. Those pin the rule that nothing here may consult phase or
// DeploymentReady: upstream's CheckDeploymentReady always returns true
// (RHAI-4448 / RHOAIENG-96107), and with spec.exposeRoute unset the phase it
// feeds is Ready unconditionally, so reading either would make this evaluator
// fail open against a dead server. PR #977 did not change that.
func TestEvaluateProviderReadyGatesOnTheDiscoveredEndpoint(t *testing.T) {
	withStatus := func(status map[string]any) *unstructured.Unstructured {
		u := NewNemoGuardrails()
		u.SetName(nemoName)
		u.SetNamespace(providerNS)
		u.Object["status"] = status
		return u
	}

	cases := []struct {
		name         string
		provider     *unstructured.Unstructured
		wantReady    bool
		wantEndpoint string
	}{
		{name: "no provider object"},
		{name: "no status", provider: NewNemoGuardrails()},
		{
			// What an auth-disabled server looks like: upstream writes the
			// field only when utils.RequiresAuth holds, and clears it
			// otherwise.
			name:     "endpoint present but empty",
			provider: withStatus(map[string]any{"endpoint": ""}),
		},
		{
			name:     "endpoint is not a URL",
			provider: withStatus(map[string]any{"endpoint": "nemo.provider-ns.svc.cluster.local"}),
		},
		{
			// ExtProc must not be handed a plaintext target for a server
			// whose whole point is an authenticated channel.
			name:     "endpoint is http, not https",
			provider: withStatus(map[string]any{"endpoint": "http://" + nemoName + "." + providerNS + ".svc.cluster.local"}),
		},
		{
			name:     "endpoint has a scheme but no host",
			provider: withStatus(map[string]any{"endpoint": "https:///v1/checks"}),
		},
		{
			name: "phase Ready and DeploymentReady True, but no endpoint",
			provider: withStatus(map[string]any{
				"phase": "Ready",
				"conditions": []any{
					map[string]any{"type": "DeploymentReady", "status": "True"},
					map[string]any{"type": "Route", "status": "True"},
				},
			}),
		},
		{
			name:         "endpoint published",
			provider:     withStatus(map[string]any{"endpoint": nemoEndpoint, "phase": "Ready"}),
			wantReady:    true,
			wantEndpoint: nemoEndpoint,
		},
		{
			// The inverse crossed case. TrustyAI writes the endpoint from its
			// not-ready path too, so discovery succeeding while the server is
			// still coming up is a state that genuinely occurs.
			name:         "endpoint published with no phase and a False DeploymentReady",
			provider:     withStatus(map[string]any{"endpoint": nemoEndpoint, "conditions": []any{map[string]any{"type": "DeploymentReady", "status": "False"}}}),
			wantReady:    true,
			wantEndpoint: nemoEndpoint,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ready, endpoint, reason, message := evaluateProviderReady(c.provider)
			if ready != c.wantReady {
				t.Fatalf("evaluateProviderReady = %v, want %v (reason %q, message %q)", ready, c.wantReady, reason, message)
			}
			if endpoint != c.wantEndpoint {
				t.Errorf("endpoint = %q, want %q", endpoint, c.wantEndpoint)
			}
			wantReason := reasonEndpointDiscoveryUnavailable
			if c.wantReady {
				wantReason = reasonProviderAvailable
			}
			if reason != wantReason {
				t.Errorf("reason = %q, want %q", reason, wantReason)
			}
			if message == "" {
				t.Error("message is empty; status has to say why this verdict was reached")
			}
		})
	}
}
