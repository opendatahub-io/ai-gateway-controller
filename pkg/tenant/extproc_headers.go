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

package tenant

// GuardedRequestHeaders is the single source of truth for the Praxis
// post-auth header-sanitization guard (the Praxis port of the legacy IPP
// maas-headers-guard plugin). It lists every header that must never reach
// the upstream (third-party) model: the caller's own credentials, the
// internal remote-MaaS loop-detection header, and every Authorino-injected
// MaaS identity/routing header.
//
// This list is a superset of the maas-controller
// deny-client-identity-headers AuthPolicy predicate list
// (models-as-a-service/maas-controller/pkg/controller/maas/
// maasauthpolicy_controller.go): every header that AuthPolicy rejects from
// client requests MUST also appear here, since Authorino injects the
// trusted value under the same name after auth succeeds. Keep both lists in
// lockstep whenever a new x-maas-* identity header is introduced.
//
// The dynamic ExternalModel generator (extprocPostAuthConfig) renders this
// slice directly. The static base chain's copy lives upstream, in
// praxis-extproc's deploy/base/config/configmap.yaml (vendored into
// config/manifests/praxis-extproc/base/config/configmap.yaml by
// hack/scripts/get-manifests.sh) -- edit it there, not here, and keep both
// lists in sync by hand.
//
// Only the static base chain additionally scopes the guard to inference
// requests (via Praxis `conditions`/`unless path_prefix` on the `headers`
// filter, in that same upstream configmap.yaml): it shares a gateway
// listener with maas-api, which needs its own authorization / x-maas-*
// headers. The dynamic ExternalModel chain never needs that scoping --
// its EnvoyFilter disables ext_proc on every route except the controller's
// own generated ExternalModel routes, which maas-api traffic never hits.
var GuardedRequestHeaders = []string{
	"authorization",
	"x-api-key",
	"x-origin-cluster",
	"x-maas-username",
	"x-maas-username-oc",
	"x-maas-username-token",
	"x-maas-group",
	"x-maas-group-oc",
	"x-maas-group-token",
	"x-maas-keyname",
	"x-maas-key-id",
	"x-maas-subscription",
	"x-maas-subscription-rate-limit-id",
	"x-maas-cost-center",
	"x-maas-organization-id",
	"x-maas-identity",
	"x-maas-admin-scope",
	"x-maas-tenant",
	"x-maas-invocation-mode",
	"x-maas-policy-revision",
	"x-maas-responses-owner",
	"x-maas-return-all-models",
	"x-maas-model",
}
