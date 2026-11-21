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

package consumerportal

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/opendatahub-io/ai-gateway-controller/pkg/tenant"
)

// ConsumerPortalProxyReadyCondition reports the health of the consumer portal auth
// proxy Deployment this controller deploys: True when the proxy is available,
// False with the pod-level failure detail (for example the x509 error that
// made it crash-loop) when it is not. It complements the AITenant's
// OIDCReady condition (owned by maas-controller), which reports issuer and
// configuration health: OIDCReady=True beside ConsumerPortalProxyReady=False means
// the issuer is fine but the proxy that fronts it is not.
//
// This condition is the one narrow exception to "ai-gateway-controller never
// writes AITenant": it is written here, on the status subresource only, and
// maas-controller must not write this condition type.
const ConsumerPortalProxyReadyCondition = "ConsumerPortalProxyReady"

// proxyUnavailableMessage is the detail-less failure message; a detailed one
// is proxyUnavailableMessage + ": " + detail.
const proxyUnavailableMessage = "consumer portal auth proxy is not available"

// maxProxyDetailLen caps the pod failure detail embedded in the condition
// message; the kubelet caps termination messages at a larger bound, so the
// condition stays readable regardless.
const maxProxyDetailLen = 200

// timestampPattern strips the timestamps proxy log lines start with: the
// termination message repeats on every container restart with a fresh
// timestamp, and a volatile message would rewrite the AITenant status on
// every resync and wake its watchers. Matches both ISO-8601
// (2026-11-19T18:34:57Z) and Go's log format (2026/11/19 05:28:38), which
// kube-auth-proxy uses.
var timestampPattern = regexp.MustCompile(`\[?\d{4}[-/]\d{2}[-/]\d{2}(?:[T ][\d:.]+(?:Z|[+-]\d{2}:?\d{2})?)?\]?`)

// proxyDeploymentHealth assesses the auth proxy Deployment and returns
// condition fields. status is a metav1.ConditionStatus string; message is
// deterministic for a given failure mode so repeated reconciles do not churn
// the AITenant status.
func (r *Reconciler) proxyDeploymentHealth(ctx context.Context, namespace string) (status, reason, message string, err error) {
	deployment := &unstructured.Unstructured{}
	deployment.SetGroupVersionKind(gvkDeployment)
	// APIReader bypasses the informer cache for the same reason as
	// routesReady: the Deployment was applied moments earlier in this
	// reconcile, so a cached Get could still return the pre-apply object and
	// report a just-rolled proxy as missing. The extra API-server read on
	// each resync is the price of an accurate first report; the Deployment
	// watch keeps later reports timely.
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: AuthProxyName}, deployment); err != nil {
		if apierrors.IsNotFound(err) {
			return "False", "NotDeployed", "consumer portal auth proxy Deployment was not found", nil
		}
		return "", "", "", fmt.Errorf("get Deployment %s/%s: %w", namespace, AuthProxyName, err)
	}

	// List the pods once: they are the authority on what is actually
	// running, and every decision below reads them.
	pods, podErr := r.listProxyPods(ctx, namespace)
	if podErr != nil {
		// Pod reads are best effort: the Deployment conditions still carry
		// the failure without the pod-level detail.
		r.Log.Info("cannot list auth proxy pods for the health report",
			"namespace", namespace, "detail", podErr)
	}
	reason, detail := proxyPodsFailure(pods)
	if reason != "" {
		if detail != "" {
			return "False", reason, proxyUnavailableMessage + ": " + detail, nil
		}
		return "False", reason, proxyUnavailableMessage, nil
	}
	// Available requires the newest pod to be Ready: a stalled rolling update
	// keeps the old replica (and with it the Deployment's Available
	// condition) healthy long after the rollout gave up — through the
	// restart-window seconds each crash attempt spends between container
	// states, and through the Progressing condition's transition to
	// ProgressDeadlineExceeded once the deadline passes. The newest pod is
	// the rollout's verdict: ready when it completed, crash-looping or
	// pending when it did not.
	if podErr == nil && len(pods) > 0 && !newestPodReady(pods) {
		return "False", "RolloutInProgress", proxyUnavailableMessage + ": the auth proxy rollout has not completed", nil
	}
	if deploymentConditionTrue(deployment, "Available") {
		return "True", "Available", "consumer portal auth proxy is available", nil
	}

	// The Deployment's own Progressing condition carries signal the pods
	// cannot (yet) provide, e.g. during the first rollout window before any
	// pod has failed; its message is deterministic for a given rollout state.
	_, prReason, prMessage := deploymentCondition(deployment, "Progressing")
	if prReason != "" {
		reason = prReason
	} else {
		reason = "NotAvailable"
	}
	if prMessage != "" {
		detail = sanitizeProxyDetail(prMessage)
	}
	if detail != "" {
		return "False", reason, proxyUnavailableMessage + ": " + detail, nil
	}
	return "False", reason, proxyUnavailableMessage, nil
}

// listProxyPods lists the auth proxy pods by their conventional label.
func (r *Reconciler) listProxyPods(ctx context.Context, namespace string) ([]*corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := r.APIReader.List(ctx, podList,
		client.InNamespace(namespace),
		client.MatchingLabels{"app.kubernetes.io/name": AuthProxyName},
	); err != nil {
		return nil, err
	}
	pods := make([]*corev1.Pod, 0, len(podList.Items))
	for i := range podList.Items {
		pods = append(pods, &podList.Items[i])
	}
	return pods, nil
}

// proxyPodsFailure reports the failure of the newest failing pod: with
// terminationMessagePolicy: FallbackToLogsOnError the kubelet copies the
// container's last log line (for example the x509 error) into
// status.containerStatuses[].lastState.terminated.message, and crash-looping
// containers surface state.waiting.reason instead. Keyed to the newest pod so
// the report is deterministic regardless of list order.
func proxyPodsFailure(pods []*corev1.Pod) (reason, detail string) {
	var newestFailure time.Time
	for _, pod := range pods {
		if !pod.GetDeletionTimestamp().IsZero() {
			// A deleting pod's lingering failure must not mask the Ready
			// replacement that is already serving.
			continue
		}
		created := pod.GetCreationTimestamp().Time
		if !newestFailure.IsZero() && !created.After(newestFailure) {
			continue
		}
		podReason, podDetail := proxyContainerFailure(pod)
		if podReason == "" {
			continue
		}
		newestFailure = created
		reason, detail = podReason, podDetail
	}
	return reason, detail
}

// newestPodReady reports whether the most recently created proxy pod passes
// its readiness probe: the rollout's verdict on the current template.
func newestPodReady(pods []*corev1.Pod) bool {
	var newest *corev1.Pod
	for _, pod := range pods {
		if !pod.GetDeletionTimestamp().IsZero() {
			continue
		}
		if newest == nil || pod.GetCreationTimestamp().After(newest.GetCreationTimestamp().Time) {
			newest = pod
		}
	}
	return newest != nil && podReady(newest)
}

// proxyContainerFailure extracts the current failure of a proxy pod's
// container: the crash-loop termination message when the container is in
// back-off, or the waiting reason (image pull, config errors) otherwise.
// Recovery is judged by pod readiness, not container state: a crash-looping
// container briefly runs during every restart attempt, and treating that as
// recovered would let an old replica's availability mask the stalled rollout
// for the seconds each assessment lands in that window.
func proxyContainerFailure(pod *corev1.Pod) (reason, detail string) {
	if pod.Status.Phase == corev1.PodRunning && podReady(pod) {
		return "", ""
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if waiting := cs.State.Waiting; waiting != nil {
			if waiting.Reason == "PodInitializing" || waiting.Reason == "ContainerCreating" {
				continue
			}
			if terminated := cs.LastTerminationState.Terminated; terminated != nil && terminated.Message != "" {
				return "CrashLoopBackOff", sanitizeProxyDetail(terminated.Message)
			}
			if waiting.Reason == "CrashLoopBackOff" {
				// The kubelet intermittently fails to capture the termination
				// message; the back-off timing in waiting.Message is noise, not
				// failure detail. Report the reason without detail and let the
				// no-downgrade rule keep the previously captured detail.
				return waiting.Reason, ""
			}
			if waiting.Reason != "" {
				return waiting.Reason, sanitizeProxyDetail(waiting.Message)
			}
		}
		// Not waiting (running or terminated) and the pod is not ready: a
		// lingering termination from the crash the container has not
		// recovered from — the pod would be ready otherwise — still counts.
		// The reason is reported even when the message is empty, so the
		// no-downgrade rule (keyed on reason) keeps previously captured
		// detail instead of the assessment degrading to a rollout-in-progress
		// report.
		if cs.State.Running != nil && !podReady(pod) {
			if terminated := cs.LastTerminationState.Terminated; terminated != nil {
				return "CrashLoopBackOff", sanitizeProxyDetail(terminated.Message)
			}
		}
	}
	return "", ""
}

// podReady reports whether every container of the pod passes its readiness
// probe.
func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// sanitizeProxyDetail strips timestamps and collapses whitespace so the
// condition message is stable across container restarts, and caps its length
// on a rune boundary so the message never carries invalid UTF-8.
func sanitizeProxyDetail(s string) string {
	s = timestampPattern.ReplaceAllString(s, "")
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimSpace(s)
	if runes := []rune(s); len(runes) > maxProxyDetailLen {
		s = string(runes[:maxProxyDetailLen])
	}
	return s
}

// reconcileProxyCondition assesses the proxy and upserts the
// ConsumerPortalProxyReady condition on the AITenant.
func (r *Reconciler) reconcileProxyCondition(ctx context.Context, aitenant *unstructured.Unstructured, infraNamespace string) error {
	status, reason, message, err := r.proxyDeploymentHealth(ctx, infraNamespace)
	if err != nil {
		return err
	}
	// observedGeneration identifies the generation whose resources the
	// assessment was based on: the reconciliation's AITenant snapshot, not
	// whatever generation is live by the time the condition is published.
	generation, _, _ := unstructured.NestedInt64(aitenant.Object, "metadata", "generation")
	return r.setProxyCondition(ctx, aitenant, status, reason, message, generation)
}

// setProxyCondition upserts the ConsumerPortalProxyReady condition on the AITenant
// status subresource. It only writes when the condition content actually
// changed, so a steady state does not wake AITenant watchers on every resync.
func (r *Reconciler) setProxyCondition(ctx context.Context, aitenant *unstructured.Unstructured, status, reason, message string, observedGeneration int64) error {
	return retry.OnError(retry.DefaultBackoff, apierrors.IsConflict, func() error {
		live := tenant.NewAITenant()
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(aitenant), live); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("get AITenant %s: %w", client.ObjectKeyFromObject(aitenant), err)
		}
		liveGeneration, _, _ := unstructured.NestedInt64(live.Object, "metadata", "generation")
		if liveGeneration != observedGeneration {
			// The AITenant changed since the snapshot this assessment is
			// based on: publishing it would stamp a stale verdict onto a
			// newer generation. The generation change already triggered a
			// re-reconcile, which will publish a fresh assessment.
			return nil
		}
		conditions, _, _ := unstructured.NestedSlice(live.Object, "status", "conditions")
		// NestedSlice returns the object's backing array, and
		// upsertProxyCondition mutates it in place. Snapshot the original
		// elements first, or the merge-patch base silently equals the
		// mutated live object and the patch becomes an empty no-op.
		original := make([]any, len(conditions))
		copy(original, conditions)
		next, changed := upsertProxyCondition(conditions, map[string]any{
			"type":               ConsumerPortalProxyReadyCondition,
			"status":             status,
			"reason":             reason,
			"message":            message,
			"observedGeneration": observedGeneration,
		})
		if !changed {
			return nil
		}
		if err := unstructured.SetNestedSlice(live.Object, next, "status", "conditions"); err != nil {
			return err
		}
		// Optimistic lock: without the resourceVersion precondition a merge
		// patch never conflicts, and a concurrent foreign-condition write
		// (e.g. maas-controller's OIDCReady) between the read and this patch
		// would be silently reverted. With it, the conflict triggers the
		// retry above with a fresh read.
		if err := r.Client.Status().Patch(ctx, live, client.MergeFromWithOptions(
			aitenantDeepCopyWithConditions(live, original), client.MergeFromWithOptimisticLock{})); err != nil {
			r.Log.Info("auth proxy condition patch failed", "detail", err)
			return err
		}
		return nil
	})
}

// removeProxyCondition removes the ConsumerPortalProxyReady condition, used when the
// proxy is torn down so the AITenant does not carry a stale report.
func (r *Reconciler) removeProxyCondition(ctx context.Context, aitenant *unstructured.Unstructured) error {
	if aitenant == nil {
		return nil
	}
	return retry.OnError(retry.DefaultBackoff, apierrors.IsConflict, func() error {
		live := tenant.NewAITenant()
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(aitenant), live); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("get AITenant %s: %w", client.ObjectKeyFromObject(aitenant), err)
		}
		conditions, _, _ := unstructured.NestedSlice(live.Object, "status", "conditions")
		kept := make([]any, 0, len(conditions))
		removed := false
		for _, condition := range conditions {
			if cm, ok := condition.(map[string]any); ok && cm["type"] == ConsumerPortalProxyReadyCondition {
				removed = true
				continue
			}
			kept = append(kept, condition)
		}
		if !removed {
			return nil
		}
		if err := unstructured.SetNestedSlice(live.Object, kept, "status", "conditions"); err != nil {
			return err
		}
		return r.Client.Status().Patch(ctx, live, client.MergeFromWithOptions(
			aitenantDeepCopyWithConditions(live, conditions), client.MergeFromWithOptimisticLock{}))
	})
}

// aitenantDeepCopyWithConditions returns a deep copy of live with the
// pre-mutation conditions restored, so MergeFrom produces a patch containing
// only the conditions change.
func aitenantDeepCopyWithConditions(live *unstructured.Unstructured, conditions []any) *unstructured.Unstructured {
	base := live.DeepCopy()
	_ = unstructured.SetNestedSlice(base.Object, conditions, "status", "conditions")
	return base
}

// upsertProxyCondition inserts or replaces the ConsumerPortalProxyReady condition in
// the conditions slice and reports whether anything changed. LastTransitionTime
// is preserved on update and set on insert.
func upsertProxyCondition(conditions []any, condition map[string]any) ([]any, bool) {
	now := time.Now().UTC().Format(time.RFC3339)
	condition = copyConditionMap(condition)
	changed := false
	for i, existing := range conditions {
		em, ok := existing.(map[string]any)
		if !ok || em["type"] != ConsumerPortalProxyReadyCondition {
			continue
		}
		condition["lastTransitionTime"] = em["lastTransitionTime"]
		if em["status"] != condition["status"] {
			condition["lastTransitionTime"] = now
		}
		// observedGeneration is compared numerically: the live condition
		// decodes it as float64 while the freshly built one carries an int64,
		// and comparing the two as interfaces would never be equal.
		contentChanged := em["reason"] != condition["reason"] || em["message"] != condition["message"] ||
			em["status"] != condition["status"] ||
			conditionNumber(em["observedGeneration"]) != conditionNumber(condition["observedGeneration"])
		// No-downgrade rule: for the same status and reason, a detail-less
		// assessment must not erase previously captured failure detail (the
		// kubelet captures the termination message only intermittently, so
		// assessments alternate between detailed and detail-less).
		existingMessage, _ := em["message"].(string)
		if contentChanged && em["reason"] == condition["reason"] && em["status"] == condition["status"] &&
			condition["message"] == proxyUnavailableMessage && strings.HasPrefix(existingMessage, proxyUnavailableMessage+":") {
			condition["message"] = existingMessage
			contentChanged = em["observedGeneration"] != condition["observedGeneration"] &&
				conditionNumber(em["observedGeneration"]) != conditionNumber(condition["observedGeneration"])
		}
		if contentChanged {
			conditions[i] = condition
			changed = true
		}
		return conditions, changed
	}
	condition["lastTransitionTime"] = now
	return append(conditions, condition), true
}

// conditionNumber normalizes a condition's numeric field for comparison
// across JSON-decoded (float64) and freshly built (int64) values.
func conditionNumber(v any) float64 {
	switch n := v.(type) {
	case int64:
		return float64(n)
	case float64:
		return n
	default:
		return -1
	}
}

func copyConditionMap(condition map[string]any) map[string]any {
	out := make(map[string]any, len(condition))
	for k, v := range condition {
		out[k] = v
	}
	return out
}

func deploymentConditionTrue(deployment *unstructured.Unstructured, conditionType string) bool {
	status, _, _ := deploymentCondition(deployment, conditionType)
	return status == "True"
}

func deploymentCondition(deployment *unstructured.Unstructured, conditionType string) (status, reason, message string) {
	conditions, _, _ := unstructured.NestedSlice(deployment.Object, "status", "conditions")
	for _, condition := range conditions {
		cm, ok := condition.(map[string]any)
		if !ok || cm["type"] != conditionType {
			continue
		}
		s, _ := cm["status"].(string)
		r, _ := cm["reason"].(string)
		m, _ := cm["message"].(string)
		return s, r, m
	}
	return "", "", ""
}

// maasTenantConfigForProxyDeployment maps the auth proxy Deployment's status
// changes to the MaasTenantConfigs whose infrastructure namespace contains
// it, so a crash loop or recovery re-reconciles the portal immediately
// instead of waiting for the resync interval.
func (r *Reconciler) maasTenantConfigForProxyDeployment(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetName() != AuthProxyName {
		return nil
	}
	return r.maasTenantConfigsForInfraNamespace(ctx, obj.GetNamespace())
}

// maasTenantConfigForProxyPod maps the auth proxy pods' status transitions
// (crash-loop restarts, back-off, recovery) to the MaasTenantConfigs whose
// infrastructure namespace contains them.
func (r *Reconciler) maasTenantConfigForProxyPod(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.maasTenantConfigsForInfraNamespace(ctx, obj.GetNamespace())
}

// maasTenantConfigsForInfraNamespace lists the MaasTenantConfigs whose
// infrastructure namespace is namespace.
func (r *Reconciler) maasTenantConfigsForInfraNamespace(ctx context.Context, namespace string) []reconcile.Request {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(tenant.MaasTenantConfigGVK.GroupVersion().WithKind(tenant.MaasTenantConfigGVK.Kind + "List"))
	if err := r.Client.List(ctx, list); err != nil {
		return nil
	}
	requests := []reconcile.Request{}
	for i := range list.Items {
		if InfraNamespace(&list.Items[i]) == namespace {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: list.Items[i].GetNamespace(),
				Name:      list.Items[i].GetName(),
			}})
		}
	}
	return requests
}
