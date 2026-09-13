package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/inference/v1alpha1"
	"github.com/opendatahub-io/ai-gateway-controller/pkg/envelope"
	"github.com/opendatahub-io/ai-gateway-controller/pkg/publisher"
	"github.com/opendatahub-io/ai-gateway-controller/pkg/render"
	"github.com/opendatahub-io/ai-gateway-controller/pkg/resolver"
	"github.com/opendatahub-io/ai-gateway-controller/pkg/tenant"
)

func controllerTestClient(t *testing.T, objects ...client.Object) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.ExternalModel{}, &v1alpha1.ExternalProvider{}).
		WithObjects(objects...).Build()}
}

func nestedSlice(t *testing.T, object map[string]any, fields ...string) []any {
	t.Helper()
	values, found, err := unstructured.NestedSlice(object, fields...)
	if err != nil || !found {
		t.Fatalf("missing %v: found=%v err=%v", fields, found, err)
	}
	return values
}

func nestedString(t *testing.T, object map[string]any, fields ...string) string {
	t.Helper()
	value, found, err := unstructured.NestedString(object, fields...)
	if err != nil || !found {
		t.Fatalf("missing %v: found=%v err=%v", fields, found, err)
	}
	return value
}

func nestedMapAt(t *testing.T, values []any, index int) map[string]any {
	t.Helper()
	if index < 0 || index >= len(values) {
		t.Fatalf("index %d out of range for %d values", index, len(values))
	}
	value, ok := values[index].(map[string]any)
	if !ok {
		t.Fatalf("value %d has type %T, want map[string]any", index, values[index])
	}
	return value
}

func TestModelHTTPRoutePreservesPathAndBodyRouting(t *testing.T) {
	route := resolver.Route{Model: "model", ClientName: "client-model"}
	obj := modelHTTPRoute(route, "tenant-a", "gateway", "maas-system", "praxis")
	parentRefs := nestedSlice(t, obj.Object, "spec", "parentRefs")
	parent := nestedMapAt(t, parentRefs, 0)
	if nestedString(t, parent, "namespace") != "maas-system" {
		t.Fatalf("route parent namespace = %q, want maas-system", nestedString(t, parent, "namespace"))
	}
	rules, found, err := unstructured.NestedSlice(obj.Object, "spec", "rules")
	if err != nil || !found || len(rules) != 2 {
		t.Fatalf("expected path and body routing rules, got found=%v len=%d err=%v", found, len(rules), err)
	}
	pathMatches := nestedSlice(t, nestedMapAt(t, rules, 0), "matches")
	path := nestedString(t, nestedMapAt(t, pathMatches, 0), "path", "value")
	if path != "/tenant-a/client-model" {
		t.Fatalf("path route = %q", path)
	}
	filters := nestedSlice(t, nestedMapAt(t, rules, 0), "filters")
	rewrite := nestedMapAt(t, filters, 0)
	if nestedString(t, rewrite, "type") != "URLRewrite" || nestedString(t, rewrite, "urlRewrite", "path", "type") != "ReplacePrefixMatch" || nestedString(t, rewrite, "urlRewrite", "path", "replacePrefixMatch") != "/" {
		t.Fatalf("path route rewrite = %#v", rewrite)
	}
	backendRefs := nestedSlice(t, nestedMapAt(t, rules, 0), "backendRefs")
	backend := nestedMapAt(t, backendRefs, 0)
	if _, found, err := unstructured.NestedString(backend, "namespace"); err != nil || found {
		t.Fatal("route backend must remain in the tenant namespace; unexpected cross-namespace backend reference")
	}
	bodyMatches := nestedSlice(t, nestedMapAt(t, rules, 1), "matches")
	headers := nestedSlice(t, nestedMapAt(t, bodyMatches, 0), "headers")
	name := nestedString(t, nestedMapAt(t, headers, 0), "name")
	value := nestedString(t, nestedMapAt(t, headers, 0), "value")
	if name != "X-Gateway-Model-Name" || value != "client-model" {
		t.Fatalf("body route header = %q=%q", name, value)
	}
}

func TestDependentEventsEnqueueOnlyAffectedNamespaceModels(t *testing.T) {
	modelA := &v1alpha1.ExternalModel{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "tenant-a"},
		Spec:       v1alpha1.ExternalModelSpec{ExternalProviderRefs: []v1alpha1.ExternalProviderRef{{Ref: v1alpha1.NameReference{Name: "provider"}}}},
	}
	modelC := &v1alpha1.ExternalModel{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "tenant-a"}}
	modelB := &v1alpha1.ExternalModel{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "tenant-b"}}
	provider := &v1alpha1.ExternalProvider{ObjectMeta: metav1.ObjectMeta{Name: "provider", Namespace: "tenant-a"}}
	r := controllerTestClient(t, modelA, modelB, modelC, provider)
	requests := r.providerModels(context.Background(), provider)
	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(modelA) {
		t.Fatalf("provider event enqueued %#v", requests)
	}

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credential", Namespace: "tenant-a"}}
	provider.Spec.Auth.SecretRef.Name = secret.Name
	r = controllerTestClient(t, modelA, modelB, modelC, provider, secret)
	requests = r.secretModels(context.Background(), secret)
	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(modelA) {
		t.Fatalf("secret event enqueued %#v", requests)
	}
}

func TestPraxisTenantUsesAnnotation(t *testing.T) {
	ait := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "maas.opendatahub.io/v1alpha1", "kind": "AITenant",
		"metadata": map[string]any{"name": "tenant", "namespace": "models-as-a-service", "annotations": map[string]any{tenant.AnnotationPayloadProcessingType: "praxis"}},
		"status":   map[string]any{"tenantNamespace": "tenant-a", "phase": "Active"},
	}}
	ait.SetGroupVersionKind(tenant.AITenantGVK)
	r := controllerTestClient(t)
	if err := r.Create(context.Background(), ait); err != nil {
		t.Fatal(err)
	}
	got, found, err := r.praxisTenantForNamespace(context.Background(), "tenant-a")
	if err != nil || !found || got.GetName() != "tenant" {
		t.Fatalf("annotation tenant lookup = %v, %v, %v", got, found, err)
	}
}

func TestTenantModelsMapsStatusNamespace(t *testing.T) {
	ait := tenant.NewAITenant()
	ait.SetNamespace("models-as-a-service")
	ait.SetName("tenant")
	ait.Object["status"] = map[string]any{"tenantNamespace": "tenant-a"}
	model := &v1alpha1.ExternalModel{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "tenant-a"}}
	r := controllerTestClient(t, model)
	requests := r.tenantModels(context.Background(), ait)
	want := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(model)}
	if len(requests) != 1 || requests[0] != want {
		t.Fatalf("tenant event enqueued %#v", requests)
	}
}

func TestReconcileCreatesTransportAndOverlayFromOneRouteSet(t *testing.T) {
	provider := &v1alpha1.ExternalProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "provider", Namespace: "tenant-a", UID: "provider-uid"},
		Spec: v1alpha1.ExternalProviderSpec{
			Provider: "openai", Endpoint: "api.example.com",
			Auth: v1alpha1.AuthConfig{Type: "apikey", SecretRef: v1alpha1.NameReference{Name: "credentials"}},
		},
	}
	model := &v1alpha1.ExternalModel{
		ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "tenant-a", UID: "model-uid"},
		Spec: v1alpha1.ExternalModelSpec{ModelName: "client-model", ExternalProviderRefs: []v1alpha1.ExternalProviderRef{{
			Ref: v1alpha1.NameReference{Name: provider.Name}, TargetModel: "gpt", APIFormat: "openai-chat", Path: "/v1/chat/completions",
		}}},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: "tenant-a"}, Data: map[string][]byte{"api-key": []byte("must-not-be-published")}}
	ait := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "maas.opendatahub.io/v1alpha1", "kind": "AITenant",
		"metadata": map[string]any{"name": "tenant", "namespace": "models-as-a-service", "annotations": map[string]any{tenant.AnnotationPayloadProcessingType: "praxis"}},
		"status":   map[string]any{"tenantNamespace": "tenant-a", "phase": "Active"},
	}}
	ait.SetGroupVersionKind(tenant.AITenantGVK)
	r := controllerTestClient(t, provider, model, secret)
	if err := r.Create(context.Background(), ait); err != nil {
		t.Fatal(err)
	}
	r.Namespace, r.GatewayName, r.GatewayNamespace, r.Network = "tenant-a", "gateway", "tenant-a", "external-model"
	r.KnownClusters = []string{"provider-provider"}
	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(model)}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var firstOverlay corev1.ConfigMap
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "tenant-a", Name: "routing-overlay"}, &firstOverlay); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	for _, object := range []struct{ kind, name string }{{"Service", "provider-provider"}, {"ServiceEntry", "provider-provider"}, {"DestinationRule", "provider-provider"}, {"HTTPRoute", "external-model-model"}} {
		got := &unstructured.Unstructured{}
		groups := map[string]string{
			"Service": "", "ServiceEntry": "networking.istio.io",
			"DestinationRule": "networking.istio.io", "HTTPRoute": "gateway.networking.k8s.io",
		}
		got.SetGroupVersionKind(schema.GroupVersionKind{Group: groups[object.kind], Version: "v1", Kind: object.kind})
		if err := r.Get(context.Background(), client.ObjectKey{Namespace: "tenant-a", Name: object.name}, got); err != nil {
			t.Fatalf("get %s: %v", object.kind, err)
		}
		if len(got.GetOwnerReferences()) != 1 {
			t.Fatalf("%s owner references = %#v", object.kind, got.GetOwnerReferences())
		}
		wantOwner := "provider"
		if object.kind == "HTTPRoute" {
			wantOwner = "model"
		}
		if got.GetOwnerReferences()[0].Name != wantOwner {
			t.Fatalf("%s owner = %q, want %q", object.kind, got.GetOwnerReferences()[0].Name, wantOwner)
		}
	}
	var overlay corev1.ConfigMap
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "tenant-a", Name: "routing-overlay"}, &overlay); err != nil {
		t.Fatal(err)
	}
	if overlay.Data["routing-overlay.json"] != firstOverlay.Data["routing-overlay.json"] {
		t.Fatal("semantic no-op rewrote overlay bytes")
	}
	if strings.Contains(overlay.Data["routing-overlay.json"], "must-not-be-published") {
		t.Fatal("Secret bytes entered overlay")
	}
	if !strings.Contains(overlay.Data["routing-overlay.json"], "provider-provider") {
		t.Fatal("provider cluster missing from overlay")
	}
	var gotModel v1alpha1.ExternalModel
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(model), &gotModel); err != nil {
		t.Fatal(err)
	}
	if gotModel.Status.Phase != resolver.PhaseReady || gotModel.Status.HTTPRouteName != "external-model-model" {
		t.Fatalf("model status = %#v", gotModel.Status)
	}
	var gotProvider v1alpha1.ExternalProvider
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(provider), &gotProvider); err != nil {
		t.Fatal(err)
	}
	if gotProvider.Status.Phase != resolver.PhaseReady || gotProvider.Status.ObservedGeneration != provider.Generation {
		t.Fatalf("provider status = %#v", gotProvider.Status)
	}
	if len(gotModel.Status.Conditions) < 2 {
		t.Fatalf("expected Ready and OverlayDistributed conditions: %#v", gotModel.Status.Conditions)
	}
	ait.SetAnnotations(nil)
	if err := r.Update(context.Background(), ait); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	removedRoute := &unstructured.Unstructured{}
	removedRoute.SetGroupVersionKind(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"})
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "tenant-a", Name: "external-model-model"}, removedRoute); !apierrors.IsNotFound(err) {
		t.Fatalf("switch-away route error = %v, want NotFound", err)
	}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "tenant-a", Name: "routing-overlay"}, &overlay); !apierrors.IsNotFound(err) {
		t.Fatalf("switch-away overlay error = %v, want NotFound", err)
	}
}

func TestReconcileExcludesProviderAfterReadinessLossAndRecovers(t *testing.T) {
	provider := &v1alpha1.ExternalProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "provider", Namespace: "tenant-a", UID: "provider-uid"},
		Spec: v1alpha1.ExternalProviderSpec{
			Provider: "openai", Endpoint: "api.example.com",
			Auth: v1alpha1.AuthConfig{Type: "apikey", SecretRef: v1alpha1.NameReference{Name: "credentials"}},
		},
	}
	model := &v1alpha1.ExternalModel{
		ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "tenant-a", UID: "model-uid"},
		Spec: v1alpha1.ExternalModelSpec{ExternalProviderRefs: []v1alpha1.ExternalProviderRef{{
			Ref: v1alpha1.NameReference{Name: provider.Name}, TargetModel: "gpt", APIFormat: "openai-chat", Path: "/v1/chat/completions",
		}}},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: "tenant-a"}, Data: map[string][]byte{"api-key": []byte("secret")}}
	ait := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "maas.opendatahub.io/v1alpha1", "kind": "AITenant",
		"metadata": map[string]any{"name": "tenant", "namespace": "models-as-a-service", "annotations": map[string]any{tenant.AnnotationPayloadProcessingType: "praxis"}},
		"status":   map[string]any{"tenantNamespace": "tenant-a", "phase": "Active"},
	}}
	ait.SetGroupVersionKind(tenant.AITenantGVK)
	r := controllerTestClient(t, provider, model, secret)
	if err := r.Create(context.Background(), ait); err != nil {
		t.Fatal(err)
	}
	r.Namespace, r.GatewayName, r.GatewayNamespace, r.Network = "tenant-a", "gateway", "tenant-a", "external-model"
	r.KnownClusters = []string{"provider-provider"}
	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(model)}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var before corev1.ConfigMap
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "tenant-a", Name: "routing-overlay"}, &before); err != nil {
		t.Fatal(err)
	}

	var stored v1alpha1.ExternalProvider
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(provider), &stored); err != nil {
		t.Fatal(err)
	}
	stored.Status.Phase = "Failed"
	if err := r.Status().Update(context.Background(), &stored); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), req); !errors.Is(err, resolver.ErrNoRoutes) {
		t.Fatalf("readiness-loss reconcile error = %v, want %v", err, resolver.ErrNoRoutes)
	}
	var after corev1.ConfigMap
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "tenant-a", Name: "routing-overlay"}, &after); err != nil {
		t.Fatal(err)
	}
	if after.Data["routing-overlay.json"] != before.Data["routing-overlay.json"] {
		t.Fatal("readiness loss replaced the last-known-good overlay")
	}
	var failed v1alpha1.ExternalModel
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(model), &failed); err != nil {
		t.Fatal(err)
	}
	if failed.Status.Phase != "Failed" || !hasConditionReason(failed.Status.Conditions, conditionReady, reasonProviderNotReady) {
		t.Fatalf("readiness-loss status = %#v", failed.Status)
	}

	stored.Status.Phase = resolver.PhaseReady
	if err := r.Status().Update(context.Background(), &stored); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var recovered v1alpha1.ExternalModel
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(model), &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.Status.Phase != resolver.PhaseReady || recovered.Status.OverlayDigest == "" || recovered.Status.OverlayGeneration == 0 {
		t.Fatalf("recovery status = %#v", recovered.Status)
	}
}

func hasConditionReason(conditions []metav1.Condition, typ, reason string) bool {
	for _, condition := range conditions {
		if condition.Type == typ && condition.Reason == reason && condition.Status == metav1.ConditionFalse {
			return true
		}
	}
	return false
}

func TestReconcileFailureBoundariesRetainPublishedOverlay(t *testing.T) {
	r, model := reconcilerFixture(t)
	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(model)}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var before corev1.ConfigMap
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "tenant-a", Name: "routing-overlay"}, &before); err != nil {
		t.Fatal(err)
	}

	r.ApplyResource = func(context.Context, client.Client, unstructured.Unstructured) error {
		return errors.New("injected transport apply failure")
	}
	if _, err := r.Reconcile(context.Background(), req); err == nil || !strings.Contains(err.Error(), "injected transport") {
		t.Fatalf("transport failure = %v", err)
	}
	var afterTransport corev1.ConfigMap
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "tenant-a", Name: "routing-overlay"}, &afterTransport); err != nil {
		t.Fatal(err)
	}
	if afterTransport.Data["routing-overlay.json"] != before.Data["routing-overlay.json"] {
		t.Fatal("transport failure published a new overlay")
	}

	r.ApplyResource = nil
	var provider v1alpha1.ExternalProvider
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "tenant-a", Name: "provider"}, &provider); err != nil {
		t.Fatal(err)
	}
	provider.Status.Phase = resolver.PhaseReady
	if err := r.Status().Update(context.Background(), &provider); err != nil {
		t.Fatal(err)
	}
	r.PublishOverlay = func(context.Context, *resolver.ResolvedRouteSet, envelope.Scope, envelope.Options) (publisher.Result, error) {
		return publisher.Result{}, errors.New("injected overlay publication failure")
	}
	if _, err := r.Reconcile(context.Background(), req); err == nil || !strings.Contains(err.Error(), "injected overlay") {
		t.Fatalf("publication failure = %v", err)
	}
	var afterPublish corev1.ConfigMap
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "tenant-a", Name: "routing-overlay"}, &afterPublish); err != nil {
		t.Fatal(err)
	}
	if afterPublish.Data["routing-overlay.json"] != before.Data["routing-overlay.json"] {
		t.Fatal("publication failure changed the last-known-good overlay")
	}
}

func TestReconcileCanFailEachSSAApplyBoundary(t *testing.T) {
	for _, boundary := range []string{"Service", "ServiceEntry", "DestinationRule", "HTTPRoute"} {
		t.Run(boundary, func(t *testing.T) {
			r, model := reconcilerFixture(t)
			req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(model)}
			if _, err := r.Reconcile(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			var before corev1.ConfigMap
			if err := r.Get(context.Background(), client.ObjectKey{Namespace: "tenant-a", Name: "routing-overlay"}, &before); err != nil {
				t.Fatal(err)
			}
			r.ApplyResource = func(ctx context.Context, _ client.Client, object unstructured.Unstructured) error {
				if object.GetKind() == boundary {
					return fmt.Errorf("injected %s apply failure", boundary)
				}
				return render.Apply(ctx, r.Client, []unstructured.Unstructured{object})
			}
			if _, err := r.Reconcile(context.Background(), req); err == nil || !strings.Contains(err.Error(), "injected "+boundary) {
				t.Fatalf("%s failure = %v", boundary, err)
			}
			var after corev1.ConfigMap
			if err := r.Get(context.Background(), client.ObjectKey{Namespace: "tenant-a", Name: "routing-overlay"}, &after); err != nil {
				t.Fatal(err)
			}
			if after.Data["routing-overlay.json"] != before.Data["routing-overlay.json"] {
				t.Fatalf("%s failure changed the published overlay", boundary)
			}
		})
	}
}

type failingStatusClient struct {
	client.Client
	err error
}

//nolint:ireturn // client.Status() is intentionally an interface boundary.
func (c failingStatusClient) Status() client.StatusWriter {
	return failingStatusWriter{SubResourceWriter: c.Client.Status(), err: c.err}
}

type failingStatusWriter struct {
	client.SubResourceWriter
	err error
}

func (w failingStatusWriter) Patch(context.Context, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
	return w.err
}

func TestStatusWriteErrorsAreReturned(t *testing.T) {
	errorsToTest := []error{
		apierrors.NewConflict(schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: "externalmodels"}, "model", errors.New("conflict")),
		apierrors.NewForbidden(schema.GroupResource{Group: v1alpha1.GroupVersion.Group, Resource: "externalmodels"}, "model", errors.New("forbidden")),
		errors.New("transient API failure"),
	}
	for _, injected := range errorsToTest {
		t.Run(injected.Error(), func(t *testing.T) {
			model := &v1alpha1.ExternalModel{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "tenant-a"}}
			r := controllerTestClient(t, model)
			r.Client = failingStatusClient{Client: r.Client, err: injected}
			if err := r.updateModelStatus(context.Background(), model, true, reasonReconciled, "status test", nil); err == nil {
				t.Fatalf("status update returned nil for %v", injected)
			}
		})
	}
}

func reconcilerFixture(t *testing.T) (*Reconciler, *v1alpha1.ExternalModel) {
	t.Helper()
	provider := &v1alpha1.ExternalProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "provider", Namespace: "tenant-a", UID: "provider-uid"},
		Spec:       v1alpha1.ExternalProviderSpec{Provider: "openai", Endpoint: "api.example.com", Auth: v1alpha1.AuthConfig{Type: "apikey", SecretRef: v1alpha1.NameReference{Name: "credentials"}}},
	}
	model := &v1alpha1.ExternalModel{
		ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "tenant-a", UID: "model-uid"},
		Spec: v1alpha1.ExternalModelSpec{ExternalProviderRefs: []v1alpha1.ExternalProviderRef{{
			Ref: v1alpha1.NameReference{Name: provider.Name}, TargetModel: "gpt",
			APIFormat: "openai-chat", Path: "/v1/chat/completions",
		}}},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: "tenant-a"}, Data: map[string][]byte{"api-key": []byte("fixture-only-secret")}}
	ait := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "maas.opendatahub.io/v1alpha1", "kind": "AITenant",
		"metadata": map[string]any{"name": "tenant", "namespace": "models-as-a-service", "annotations": map[string]any{tenant.AnnotationPayloadProcessingType: "praxis"}},
		"status":   map[string]any{"tenantNamespace": "tenant-a", "phase": "Active"},
	}}
	ait.SetGroupVersionKind(tenant.AITenantGVK)
	r := controllerTestClient(t, provider, model, secret)
	if err := r.Create(context.Background(), ait); err != nil {
		t.Fatal(err)
	}
	r.Namespace, r.GatewayName, r.GatewayNamespace, r.Network = "tenant-a", "gateway", "tenant-a", "external-model"
	r.KnownClusters = []string{"provider-provider"}
	return r, model
}
