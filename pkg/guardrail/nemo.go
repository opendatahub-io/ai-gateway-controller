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

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// NewNemoGuardrails returns an empty unstructured object with the
// NemoGuardrails GVK set, ready for a Get by name/namespace.
func NewNemoGuardrails() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(NemoGuardrailsGVK)
	return u
}

// providerCRDInstalled reports whether TrustyAI's NemoGuardrails CRD is
// present on the cluster.
//
// This gates the whole reconciler. Watching a Kind whose CRD is absent fails
// the informer and brings the manager down with it, which would take the
// tenant and external-model reconcilers offline too — so on a cluster without
// TrustyAI, an AI Gateway that cannot do guardrails has to keep doing
// everything else.
//
// The manager's RESTMapper is used rather than a separate discovery client:
// it is already built from the same discovery data, and reading it needs no
// RBAC beyond the API discovery every client already performs.
//
// This is a startup-only decision. Installing TrustyAI afterwards does not
// enable the reconciler until this controller restarts, which is the usual
// trade for not running a CRD watch purely to detect its own activation.
func providerCRDInstalled(mapper apimeta.RESTMapper) (bool, error) {
	_, err := mapper.RESTMapping(NemoGuardrailsGVK.GroupKind(), NemoGuardrailsGVK.Version)
	switch {
	case err == nil:
		return true, nil
	case apimeta.IsNoMatchError(err):
		// The CRD is genuinely absent, which is a supported configuration
		// rather than a failure.
		return false, nil
	default:
		// Discovery itself failed. Starting up as though TrustyAI were absent
		// would silently disable guardrails on a cluster that has them, so
		// this is reported and left for the caller to treat as fatal.
		return false, fmt.Errorf("resolving %s: %w", NemoGuardrailsGVK, err)
	}
}
