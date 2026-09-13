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
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/inference/v1alpha1"
	"github.com/opendatahub-io/ai-gateway-controller/pkg/render"
)

// notReadyRequeueInterval is used when an AITenant has opted into praxis
// but is not yet ready for it (see IsActive / GatewayRef). This is a "come
// back shortly" wait, distinct from ResyncInterval's steady-state resync.
const notReadyRequeueInterval = 10 * time.Second

// Reconciler watches AITenant CRs and, for every tenant whose
// AnnotationPayloadProcessingType annotation is "praxis", renders and
// SSA-applies a per-tenant copy of the vendored praxis-extproc manifests
// into that tenant's Gateway namespace (from status.gatewayRef). Tenants
// that don't opt into praxis (absent/empty/"ipp") are ignored:
// maas-controller's own TenantReconciler owns their IPP deployment.
//
// Reconciler does not write AITenant status. It does track, via
// PraxisCleanupFinalizer, whether it has (or may have) applied resources
// for a tenant, so it can delete them again when the tenant switches away
// from praxis or the AITenant is deleted — SSA only ever upserts the
// current render set, it never deletes what falls out of it.
type Reconciler struct {
	// Client applies the rendered resources and reads/updates AITenant.
	Client client.Client
	// ManifestPath is the kustomize entrypoint, e.g.
	// config/manifests/praxis-extproc/overlays/odh.
	ManifestPath string
	// Image replaces the vendored overlay's placeholder container image.
	Image string
	// PraxisImage is the standalone Praxis AI image used for final-hop routing.
	PraxisImage string
	// PraxisImagePullPolicy controls image pulling for the standalone Praxis
	// Deployment. Production defaults to IfNotPresent; local Kind can use Never.
	PraxisImagePullPolicy string
	// MaaSAPIRouteNameBase is the base name used to disable ext_proc on
	// maas-api's own HTTPRoute rules; suffixed per tenant like every other
	// resource this package renames.
	MaaSAPIRouteNameBase string
	// ResyncInterval is the RequeueAfter used once a tenant's resources have
	// been successfully applied, so drift gets corrected periodically even
	// without a new watch event (mirrors maas-controller's TenantReconciler
	// setFinalStatus, and this controller's own former --resync-interval
	// ticker). Must be positive.
	ResyncInterval time.Duration
	// DeletionTimeout bounds how long Reconcile keeps retrying cleanup
	// before force-removing PraxisCleanupFinalizer without confirming
	// cleanup succeeded (mirrors maas-controller's
	// AITenantReconciler.DeletionTimeout / forceRemoveAITenantFinalizer):
	// without this, a persistent cleanup failure (or this controller being
	// down) would block AITenant deletion forever. Zero disables the
	// timeout and retries indefinitely.
	DeletionTimeout time.Duration
	// Log receives one entry per reconcile, plus any render/apply/cleanup
	// error.
	Log logr.Logger
}

// SetupWithManager registers the AITenant watch.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(NewAITenant()).
		Watches(&v1alpha1.ExternalProvider{}, handler.EnqueueRequestsFromMapFunc(r.tenantsForNamespace)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.tenantsForNamespace)).
		Complete(r)
}

// tenantsForNamespace re-renders the standalone Praxis pod template when a
// provider reference or referenced Secret changes. It maps only to AITenants
// whose resolved tenant namespace is the changed object's namespace.
func (r *Reconciler) tenantsForNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	tenantList := &unstructured.UnstructuredList{}
	tenantList.SetGroupVersionKind(AITenantGVK.GroupVersion().WithKind("AITenantList"))
	if err := r.Client.List(ctx, tenantList); err != nil {
		r.Log.Error(err, "list tenants for dataplane configuration event", "namespace", obj.GetNamespace())
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for i := range tenantList.Items {
		tenant := &tenantList.Items[i]
		resolved, _, _ := unstructured.NestedString(tenant.Object, "status", "tenantNamespace")
		if resolved == "" {
			resolved = tenant.GetNamespace()
		}
		if resolved == obj.GetNamespace() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(tenant)})
		}
	}
	return requests
}

// Reconcile implements the logic documented on Reconciler.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("aitenant", req.NamespacedName)

	aitenant := NewAITenant()
	if err := r.Client.Get(ctx, req.NamespacedName, aitenant); err != nil {
		if apierrors.IsNotFound(err) {
			// Already fully gone: our finalizer (if we ever added one)
			// must have already been cleared, or we never added one.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get AITenant %s: %w", req.NamespacedName, err)
	}

	tenantID := ID(req.Name)

	if !aitenant.GetDeletionTimestamp().IsZero() {
		return r.reconcileDelete(ctx, log, aitenant, tenantID)
	}

	if !UsesPraxis(aitenant) {
		return r.reconcileNotPraxis(ctx, log, aitenant, tenantID)
	}

	return r.reconcilePraxis(ctx, log, aitenant, tenantID)
}

// reconcilePraxis is the steady-state path for a tenant that currently
// opts into praxis: ensure the cleanup finalizer is present (before doing
// anything else, so even a partially-applied tenant is guaranteed a
// cleanup pass later), wait for readiness, then render/apply.
func (r *Reconciler) reconcilePraxis(ctx context.Context, log logr.Logger, aitenant *unstructured.Unstructured, tenantID string) (ctrl.Result, error) {
	if err := r.ensureFinalizer(ctx, aitenant); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure finalizer: %w", err)
	}

	if !IsActive(aitenant) {
		log.Info("AITenant opted into praxis but is not Active yet; will retry")
		return ctrl.Result{RequeueAfter: notReadyRequeueInterval}, nil
	}

	gatewayName, gatewayNamespace, ready := GatewayRef(aitenant)
	if !ready {
		log.Info("AITenant is Active but status.gatewayRef is not populated; will retry")
		return ctrl.Result{RequeueAfter: notReadyRequeueInterval}, nil
	}
	tenantNamespace := ResolvedNamespace(aitenant)
	if tenantNamespace == "" {
		log.Info("AITenant is Active but status.tenantNamespace is not populated; will retry")
		return ctrl.Result{RequeueAfter: notReadyRequeueInterval}, nil
	}

	rendered, err := render.Build(r.ManifestPath)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("render: %w", err)
	}

	resources := render.PostRender(rendered, render.Params{
		Namespace:        gatewayNamespace,
		GatewayName:      gatewayName,
		Image:            r.Image,
		MaaSAPIRouteName: ResourceName(r.MaaSAPIRouteNameBase, tenantID),
	})

	resources, err = Rename(resources, tenantID, gatewayNamespace)
	if err != nil {
		// Not retryable until the AITenant's name (spec) changes: don't
		// requeue, or every resync would fail identically for the same
		// reason. A future spec update re-triggers reconciliation via the
		// watch.
		log.Error(err, "cannot render praxis-extproc resources for this tenant name; will not retry until the AITenant changes")
		return ctrl.Result{}, nil
	}
	// The vendored ExtProc manifests intentionally carry no controller-specific
	// ownership marker. Stamp the complete tenant render before the handoff
	// check so resources successfully applied by this reconciler are recognized
	// as ours on the next reconcile. This label is also the cleanup guard; the
	// shared reader ClusterRole remains exempt from takeover/cleanup checks.
	for i := range resources {
		labels := resources[i].GetLabels()
		if labels == nil {
			labels = make(map[string]string)
		}
		labels[managedByLabel] = render.FieldOwner
		resources[i].SetLabels(labels)
	}
	providerList := &unstructured.UnstructuredList{}
	providerList.SetGroupVersionKind(schema.GroupVersionKind{Group: "inference.opendatahub.io", Version: "v1alpha1", Kind: "ExternalProviderList"})
	if err := r.Client.List(ctx, providerList, client.InNamespace(tenantNamespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("list tenant ExternalProviders: %w", err)
	}
	providers := make([]v1alpha1.ExternalProvider, 0, len(providerList.Items))
	modelList := &unstructured.UnstructuredList{}
	modelList.SetGroupVersionKind(schema.GroupVersionKind{Group: "inference.opendatahub.io", Version: "v1alpha1", Kind: "ExternalModelList"})
	if err := r.Client.List(ctx, modelList, client.InNamespace(tenantNamespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("list tenant ExternalModels: %w", err)
	}
	referencedProviders := map[string]bool{}
	for i := range modelList.Items {
		refs, found, err := unstructured.NestedSlice(modelList.Items[i].Object, "spec", "externalProviderRefs")
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("read provider refs for ExternalModel %s: %w", modelList.Items[i].GetName(), err)
		}
		if !found {
			continue
		}
		for _, raw := range refs {
			ref, ok := raw.(map[string]any)
			if !ok {
				return ctrl.Result{}, fmt.Errorf("invalid provider ref in ExternalModel %s", modelList.Items[i].GetName())
			}
			name, _, err := unstructured.NestedString(ref, "ref", "name")
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("read provider ref in ExternalModel %s: %w", modelList.Items[i].GetName(), err)
			}
			if name != "" {
				referencedProviders[name] = true
			}
		}
	}
	for i := range providerList.Items {
		if !referencedProviders[providerList.Items[i].GetName()] {
			continue
		}
		var provider v1alpha1.ExternalProvider
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(providerList.Items[i].Object, &provider); err != nil {
			return ctrl.Result{}, fmt.Errorf("decode tenant ExternalProvider %s: %w", providerList.Items[i].GetName(), err)
		}
		providers = append(providers, provider)
	}
	praxisImage := r.PraxisImage
	if praxisImage == "" {
		praxisImage = "quay.io/opendatahub/praxis-ai:odh-stable"
	}
	praxisResources, err := StandalonePraxisResources(tenantID, tenantNamespace, praxisImage, r.PraxisImagePullPolicy, providers)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("render standalone praxis: %w", err)
	}
	resources = append(resources, praxisResources...)

	// MaaS and this controller intentionally use the same tenant-derived
	// payload-processing names. During an IPP-to-Praxis handoff, MaaS may
	// still be deleting its operands when the annotation watch reaches us.
	// Never force-SSA over a foreign object: apart from violating the
	// single-writer contract, merging two pod templates can produce an invalid
	// Deployment (for example duplicate named ports). Wait for the owning
	// controller's cleanup and let the next watch/reconcile apply our complete
	// resource set.
	if err := r.waitForForeignOwnership(ctx, resources); err != nil {
		log.Info("praxis-extproc resources are still owned by another controller; waiting for handoff", "error", err)
		return ctrl.Result{RequeueAfter: notReadyRequeueInterval}, nil
	}

	if err := render.Apply(ctx, r.Client, resources); err != nil {
		log.Error(err, "praxis-extproc apply failed for tenant; will retry")
		return ctrl.Result{}, fmt.Errorf("apply: %w", err)
	}

	log.Info("praxis-extproc install applied",
		"tenantID", tenantID, "namespace", gatewayNamespace, "gatewayName", gatewayName)
	return ctrl.Result{RequeueAfter: r.ResyncInterval}, nil
}

const managedByLabel = "app.kubernetes.io/managed-by"

// waitForForeignOwnership prevents a forced SSA apply from taking over an
// existing object while another controller still owns it. An object already
// labeled by this controller is safe to update; an unlabeled object is also
// treated as foreign because common names are not proof of ownership.
func (r *Reconciler) waitForForeignOwnership(ctx context.Context, resources []unstructured.Unstructured) error {
	for i := range resources {
		desired := &resources[i]
		// The reader ClusterRole is a shared, pre-existing RBAC primitive.
		// This controller deliberately owns only each tenant's binding, not
		// the shared role itself, so its absence of our managed-by label is
		// not an ownership conflict.
		if desired.GetKind() == "ClusterRole" && desired.GetName() == "payload-processing-reader" {
			continue
		}
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(desired.GroupVersionKind())
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: desired.GetNamespace(), Name: desired.GetName()}, current); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("inspect %s %s/%s: %w", desired.GetKind(), desired.GetNamespace(), desired.GetName(), err)
		}
		if current.GetLabels()[managedByLabel] != render.FieldOwner {
			return fmt.Errorf("%s %s/%s is managed by %q", desired.GetKind(), desired.GetNamespace(), desired.GetName(), current.GetLabels()[managedByLabel])
		}
	}
	return nil
}

// reconcileNotPraxis handles a tenant that currently does not opt into
// praxis. If our finalizer is present, this tenant previously opted in and
// has since switched away (or dropped the annotation): clean up whatever
// was applied before releasing the finalizer. Otherwise this tenant never
// used praxis and there is nothing to do.
func (r *Reconciler) reconcileNotPraxis(ctx context.Context, log logr.Logger, aitenant *unstructured.Unstructured, tenantID string) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(aitenant, PraxisCleanupFinalizer) {
		log.V(1).Info("AITenant does not use the praxis payload processing backend; nothing to do")
		return ctrl.Result{}, nil
	}

	if _, gatewayNamespace, ready := GatewayRef(aitenant); ready {
		tenantNamespace := ResolvedNamespace(aitenant)
		if tenantNamespace == "" {
			tenantNamespace = gatewayNamespace
		}
		if err := r.cleanup(ctx, tenantID, gatewayNamespace, tenantNamespace); err != nil {
			log.Error(err, "praxis-extproc cleanup failed after switching away from praxis; will retry", "namespace", gatewayNamespace)
			return ctrl.Result{}, fmt.Errorf("cleanup: %w", err)
		}
		log.Info("praxis-extproc resources cleaned up after switching away from praxis", "tenantID", tenantID, "namespace", gatewayNamespace)
	}

	if err := r.removeFinalizer(ctx, aitenant); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// reconcileDelete handles a tenant whose AITenant is being deleted.
func (r *Reconciler) reconcileDelete(ctx context.Context, log logr.Logger, aitenant *unstructured.Unstructured, tenantID string) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(aitenant, PraxisCleanupFinalizer) {
		// We never opted this tenant in (or already finished cleanup):
		// nothing for us to do, and no finalizer of ours blocking deletion.
		return ctrl.Result{}, nil
	}

	if r.DeletionTimeout > 0 && time.Since(aitenant.GetDeletionTimestamp().Time) >= r.DeletionTimeout {
		log.Error(nil, "praxis-extproc cleanup exceeded deletion timeout; force-removing finalizer without confirming cleanup succeeded",
			"tenantID", tenantID, "timeout", r.DeletionTimeout)
		return ctrl.Result{}, r.removeFinalizer(ctx, aitenant)
	}

	_, gatewayNamespace, ready := GatewayRef(aitenant)
	if !ready {
		// Never got far enough to have a target namespace, so nothing was
		// ever applied for this tenant.
		log.Info("AITenant deleted before status.gatewayRef was ever populated; skipping cleanup")
		return ctrl.Result{}, r.removeFinalizer(ctx, aitenant)
	}

	tenantNamespace := ResolvedNamespace(aitenant)
	if tenantNamespace == "" {
		tenantNamespace = gatewayNamespace
	}
	if err := r.cleanup(ctx, tenantID, gatewayNamespace, tenantNamespace); err != nil {
		log.Error(err, "praxis-extproc cleanup failed for deleted tenant; will retry", "namespace", gatewayNamespace)
		return ctrl.Result{}, fmt.Errorf("cleanup: %w", err)
	}

	log.Info("praxis-extproc resources cleaned up for deleted tenant", "tenantID", tenantID, "namespace", gatewayNamespace)
	return ctrl.Result{}, r.removeFinalizer(ctx, aitenant)
}

// ensureFinalizer adds PraxisCleanupFinalizer if not already present.
func (r *Reconciler) ensureFinalizer(ctx context.Context, aitenant *unstructured.Unstructured) error {
	if controllerutil.ContainsFinalizer(aitenant, PraxisCleanupFinalizer) {
		return nil
	}
	base := aitenant.DeepCopy()
	controllerutil.AddFinalizer(aitenant, PraxisCleanupFinalizer)
	if err := r.Client.Patch(ctx, aitenant, client.MergeFrom(base)); err != nil {
		return err
	}
	return nil
}

// removeFinalizer removes PraxisCleanupFinalizer if present.
func (r *Reconciler) removeFinalizer(ctx context.Context, aitenant *unstructured.Unstructured) error {
	if !controllerutil.ContainsFinalizer(aitenant, PraxisCleanupFinalizer) {
		return nil
	}
	base := aitenant.DeepCopy()
	controllerutil.RemoveFinalizer(aitenant, PraxisCleanupFinalizer)
	if err := r.Client.Patch(ctx, aitenant, client.MergeFrom(base)); err != nil {
		return err
	}
	return nil
}

// cleanup deletes (ignoring not-found) every resource this controller
// would have applied for tenantID in namespace, mirroring
// tenantreconcile.cleanupTenantResources / cleanupPayloadProcessingHPA in
// maas-controller: SSA only ever upserts the current render set, it never
// deletes what falls out of it. The shared payload-processing-reader
// ClusterRole is never deleted here: every tenant's ClusterRoleBinding
// references that one role, so deleting it would break every other
// praxis tenant sharing the cluster.
func (r *Reconciler) cleanup(ctx context.Context, tenantID, gatewayNamespace, tenantNamespace string) error {
	type target struct {
		gvk       schema.GroupVersionKind
		name      string
		namespace string
	}

	targets := []target{
		{gvkServiceAccount, ResourceName(praxisServiceAccount, tenantID), tenantNamespace},
		{gvkConfigMap, ResourceName(praxisConfigMapName, tenantID), tenantNamespace},
		{gvkService, ResourceName(praxisServiceName, tenantID), tenantNamespace},
		{gvkDeployment, ResourceName(praxisDeploymentName, tenantID), tenantNamespace},
		{gvkDeployment, PayloadProcessingDeploymentName(tenantID), gatewayNamespace},
		{gvkDeployment, PayloadPreProcessingDeploymentName(tenantID), gatewayNamespace},
		{gvkService, PayloadProcessingServiceName(tenantID), gatewayNamespace},
		{gvkService, PayloadPreProcessingServiceName(tenantID), gatewayNamespace},
		{gvkConfigMap, PayloadProcessingPluginsConfigMapForTenant(tenantID), gatewayNamespace},
		{gvkServiceAccount, PayloadProcessingServiceAccountName(tenantID), gatewayNamespace},
		{gvkNetworkPolicy, PayloadProcessingNetworkPolicyName(tenantID), gatewayNamespace},
		{gvkEnvoyFilter, PayloadProcessingEnvoyFilterName(tenantID), gatewayNamespace},
		{gvkDestinationRule, PayloadProcessingServiceName(tenantID), gatewayNamespace},
		{gvkDestinationRule, PayloadPreProcessingServiceName(tenantID), gatewayNamespace},
		{gvkClusterRoleBinding, PayloadProcessingReaderClusterRoleBindingNameForTenant(tenantID), ""},
	}

	for _, t := range targets {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(t.gvk)
		obj.SetName(t.name)
		obj.SetNamespace(t.namespace)
		if err := client.IgnoreNotFound(r.Client.Delete(ctx, obj)); err != nil {
			return fmt.Errorf("delete %s %s/%s: %w", t.gvk.Kind, t.namespace, t.name, err)
		}
	}
	return nil
}
