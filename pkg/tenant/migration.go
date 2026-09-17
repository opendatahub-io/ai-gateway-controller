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

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PraxisBundleExists reports whether this tenant's praxis-extproc Deployment
// is already present in the gateway namespace. Once true,
// AnnotationIPPMigrationCleanupComplete is irrelevant to this tenant going
// forward: it only sequences the initial transition-in after a backend
// swap, never ongoing reconciliation of an already-deployed backend (a
// crash/restart mid-deploy, or a routine drift-correction resync, must both
// keep applying unconditionally rather than getting stuck re-reading a
// marker that was already consumed on a previous reconcile).
func PraxisBundleExists(ctx context.Context, c client.Client, tenantID, gatewayNamespace string) (bool, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvkDeployment)
	key := client.ObjectKey{Namespace: gatewayNamespace, Name: PayloadProcessingDeploymentName(tenantID)}
	if err := c.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get praxis-extproc deployment %s/%s: %w", key.Namespace, key.Name, err)
	}
	return true, nil
}

// ClaimIPPMigrationMarker atomically consumes a "clear to deploy" marker
// (IPPMigrationMarkerClearValue) on a MaasTenantConfig before praxis-extproc
// is (re)deployed for a tenant that does not currently have its own bundle,
// by deleting the annotation — returning it to its "blocked" (absent)
// resting state — rather than writing a sentinel value. It uses an
// optimistic-concurrency Update — not a blind merge patch — so that if
// maas-controller is concurrently attempting the mirror-image claim for the
// same tenant config at nearly the same time (e.g. a rapid
// praxis→legacy→praxis flip before the original cleanup's signal has been
// consumed), only one of the two controllers can win: the other observes a
// Conflict, does not proceed, and re-evaluates from a fresh read on its next
// reconcile.
//
// A false return (with a nil error) means the marker is currently absent —
// a cleanup is genuinely in flight, or was just claimed by maas-controller —
// and the caller must wait, not that an error occurred.
func ClaimIPPMigrationMarker(ctx context.Context, c client.Client, mtc *unstructured.Unstructured) (claimed bool, err error) {
	latest := &unstructured.Unstructured{}
	latest.SetGroupVersionKind(mtc.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKeyFromObject(mtc), latest); err != nil {
		return false, fmt.Errorf("get MaasTenantConfig for IPP migration marker claim: %w", err)
	}
	annotations := latest.GetAnnotations()
	if annotations == nil || annotations[AnnotationIPPMigrationCleanupComplete] != IPPMigrationMarkerClearValue {
		return false, nil
	}
	delete(annotations, AnnotationIPPMigrationCleanupComplete)
	latest.SetAnnotations(annotations)
	if err := c.Update(ctx, latest); err != nil {
		if apierrors.IsConflict(err) {
			return false, nil
		}
		return false, fmt.Errorf("claim IPP migration marker: %w", err)
	}
	return true, nil
}

// MarkIPPMigrationCleanupComplete is this controller's switch-off "done"
// signal, mirroring maas-controller's markIPPMigrationCleanupComplete: it is
// only ever called after a full, successful cleanup of praxis-extproc's own
// bundle (see Reconciler.cleanup), so it is safe to use a plain merge patch
// rather than the CAS-guarded claim used on the transition-in side (only one
// controller is ever the switch-off party for a given swap at a time, so
// there is no cross-controller writer race on this specific write).
func MarkIPPMigrationCleanupComplete(ctx context.Context, c client.Client, mtc *unstructured.Unstructured) error {
	base := mtc.DeepCopy()
	annotations := mtc.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	if annotations[AnnotationIPPMigrationCleanupComplete] == IPPMigrationMarkerClearValue {
		return nil
	}
	annotations[AnnotationIPPMigrationCleanupComplete] = IPPMigrationMarkerClearValue
	mtc.SetAnnotations(annotations)
	return c.Patch(ctx, mtc, client.MergeFrom(base))
}
