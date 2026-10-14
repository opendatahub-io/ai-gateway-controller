package tenant

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	"github.com/opendatahub-io/ai-gateway-controller/pkg/render"
)

func responsesPlatformSecrets() (*corev1.Secret, *corev1.Secret) {
	return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: responsesSecretName, Namespace: "infra", ResourceVersion: "1"},
			Data:       map[string][]byte{"DB_CONNECTION_URL": []byte("postgresql://responses@postgres.infra.svc/responses"), "tls.crt": []byte("fixture-cert"), "tls.key": []byte("fixture-key")},
		},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: responsesCAName, Namespace: "infra", ResourceVersion: "1"}, Data: map[string][]byte{"ca.crt": []byte("fixture-ca")}}
}

func TestResponsesBindingValidatesPlatformOptIn(t *testing.T) {
	connection, ca := responsesPlatformSecrets()
	for _, tt := range []struct {
		name                   string
		spec                   map[string]any
		wantBinding, wantError bool
	}{
		{name: "omitted"},
		{name: "disabled", spec: map[string]any{"enabled": false}},
		{name: "enabled defaults", spec: map[string]any{"enabled": true}, wantBinding: true},
		{name: "supported storage", spec: map[string]any{"enabled": true, "storage": map[string]any{"mode": "PlatformDefault", "deletionPolicy": "Retain"}}, wantBinding: true},
		{name: "wrong boolean", spec: map[string]any{"enabled": "true"}, wantError: true},
		{name: "unsupported storage", spec: map[string]any{"enabled": true, "storage": map[string]any{"mode": "SQLite"}}, wantError: true},
		{name: "unsupported purge", spec: map[string]any{"enabled": true, "storage": map[string]any{"deletionPolicy": "Delete"}}, wantError: true},
		{name: "unsupported retention", spec: map[string]any{"enabled": true, "retention": map[string]any{"maxAge": "1h"}}, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			reads := 0
			c := fake.NewClientBuilder().WithScheme(mtcSchemeForTests()).WithObjects(connection, ca).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					reads++
					return c.Get(ctx, key, obj, opts...)
				},
			}).Build()
			owner := NewAITenant()
			owner.SetName("demo")
			owner.SetUID("tenant-uid")
			if tt.spec != nil {
				owner.Object["spec"] = map[string]any{"responses": tt.spec}
			}
			binding, err := (&Reconciler{Client: c, APIReader: c, InfrastructureNamespace: "infra"}).responsesBinding(t.Context(), owner, "tenant-a")
			if (err != nil) != tt.wantError || (binding != nil) != tt.wantBinding {
				t.Fatalf("has binding=%v error=%v", binding != nil, err)
			}
			if !tt.wantBinding && reads != 0 {
				t.Fatal("disabled/invalid Responses must not read a Secret")
			}
		})
	}
	owner := NewAITenant()
	owner.Object["spec"] = map[string]any{"responses": map[string]any{"enabled": true}}
	c := fake.NewClientBuilder().WithScheme(mtcSchemeForTests()).Build()
	if _, err := (&Reconciler{Client: c}).responsesBinding(t.Context(), owner, "tenant-a"); err == nil {
		t.Fatal("missing binding must fail closed")
	}
}

func TestResponsesSharedProjectionAndIdentityFence(t *testing.T) {
	connection, ca := responsesPlatformSecrets()
	connection.Data["unused"] = []byte("must-not-project")
	c := fake.NewClientBuilder().WithScheme(mtcSchemeForTests()).WithObjects(connection, ca).Build()
	// A cached credential read would populate another informer with Secret data.
	cache := fake.NewClientBuilder().WithScheme(mtcSchemeForTests()).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			t.Fatal("credential resolution must use APIReader")
			return nil
		},
	}).Build()
	r := &Reconciler{Client: cache, APIReader: c, InfrastructureNamespace: "infra"}
	owner := NewAITenant()
	owner.SetName("demo")
	owner.SetUID("tenant-uid")
	owner.Object["spec"] = map[string]any{"responses": map[string]any{"enabled": true}}
	binding, err := r.responsesBinding(t.Context(), owner, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	r.Client = c
	if err := r.projectResponsesBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: "tenant-a", Name: responsesRuntimeSecretName("demo")}
	projection := &corev1.Secret{}
	if err := c.Get(t.Context(), key, projection); err != nil || len(projection.Data) != 4 {
		t.Fatalf("private projection unavailable or has unexpected keys: %v", err)
	}
	for name, secret := range map[string]*corev1.Secret{responsesSecretName: connection, responsesCAName: ca} {
		if err := c.Get(t.Context(), client.ObjectKeyFromObject(secret), secret); err != nil {
			t.Fatal(err)
		}
		if name == responsesCAName {
			secret.Data["ca.crt"] = []byte("rotated-ca")
		} else {
			secret.Data["tls.key"] = []byte("rotated-key")
		}
		if err := c.Update(t.Context(), secret); err != nil {
			t.Fatal(err)
		}
		next, err := r.responsesBinding(t.Context(), owner, "tenant-a")
		if err != nil || next.resourceVersion == binding.resourceVersion {
			t.Fatalf("%s change must roll the runtime: %v", name, err)
		}
		if err := r.projectResponsesBinding(t.Context(), next); err != nil {
			t.Fatal(err)
		}
		binding = next
	}
	owner.SetUID("replacement-uid")
	if _, err := r.responsesBinding(t.Context(), owner, "tenant-a"); err == nil {
		t.Fatal("same-name replacement must not adopt another UID's binding")
	}
	owner.SetUID("tenant-uid")
	connection.Data["DB_CONNECTION_URL"] = []byte("postgresql://responses@other.infra.svc/responses")
	if err := c.Update(t.Context(), connection); err != nil {
		t.Fatal(err)
	}
	if _, err := r.responsesBinding(t.Context(), owner, "tenant-a"); err == nil {
		t.Fatal("endpoint changes must not silently move the store")
	}
	if err := r.cleanupResponsesProjection(t.Context(), "demo", "tenant-a"); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), key, projection); !apierrors.IsNotFound(err) {
		t.Fatal("tenant cleanup must remove only the private binding")
	}
	for _, secret := range []*corev1.Secret{connection, ca} {
		if err := c.Get(t.Context(), client.ObjectKeyFromObject(secret), secret); err != nil {
			t.Fatal("platform source must survive tenant cleanup")
		}
	}
}

func TestResponsesBindingRejectsIncompleteForeignAndMaaSBindings(t *testing.T) {
	for _, scenario := range []string{"missing CA", "missing key", "foreign projection", "MaaS reuse", "endpoint override"} {
		t.Run(scenario, func(t *testing.T) {
			connection, ca := responsesPlatformSecrets()
			objects := []client.Object{connection, ca}
			switch scenario {
			case "missing CA":
				objects = objects[:1]
			case "missing key":
				delete(connection.Data, "tls.key")
			case "foreign projection":
				objects = append(objects, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: responsesRuntimeSecretName("demo"), Namespace: "tenant-a"}})
			case "MaaS reuse":
				objects = append(objects, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: "maas-db-config", Namespace: "infra"},
					Data:       map[string][]byte{"DB_CONNECTION_URL": []byte("postgres://different@POSTGRES.infra.svc:5432/responses")},
				})
			case "endpoint override":
				connection.Data["DB_CONNECTION_URL"] = []byte("postgresql://responses@postgres.infra.svc/responses?host=other")
			}
			c := fake.NewClientBuilder().WithScheme(mtcSchemeForTests()).WithObjects(objects...).Build()
			owner := NewAITenant()
			owner.SetName("demo")
			owner.SetUID("tenant-uid")
			owner.Object["spec"] = map[string]any{"responses": map[string]any{"enabled": true}}
			if _, err := (&Reconciler{Client: c, APIReader: c, InfrastructureNamespace: "infra"}).responsesBinding(t.Context(), owner, "tenant-a"); err == nil {
				t.Fatal("invalid platform binding must fail closed")
			}
		})
	}
}

func TestResponsesPlatformSecretEventsFanOut(t *testing.T) {
	var objects []client.Object
	for _, name := range []string{"one", "two", "disabled"} {
		owner := newAITenantOwner(name, "owners", AITenantPhaseActive, name+"-gateway", "gateways", "tenant-"+name)
		owner.Object["spec"] = map[string]any{"responses": map[string]any{"enabled": name != "disabled"}}
		objects = append(objects, owner)
	}
	c := fake.NewClientBuilder().WithScheme(mtcSchemeForTests()).WithObjects(objects...).Build()
	r := &Reconciler{Client: c, InfrastructureNamespace: "infra"}
	for _, name := range []string{responsesSecretName, responsesCAName, "maas-db-config"} {
		requests := r.maasTenantConfigsForSecret(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "infra"}})
		if len(requests) != 2 {
			t.Fatal("platform change must reconcile all enabled tenants")
		}
		for _, request := range requests {
			if request.Namespace == "tenant-disabled" || request.Namespace == "infra" {
				t.Fatal("incorrect platform event target")
			}
		}
	}
}

func TestResponsesConfigAndMountsRemainTenantLocal(t *testing.T) {
	rendered, err := render.Build(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	resources := render.PostRender(rendered, render.Params{Namespace: "gateway-system", GatewayName: "demo-gateway", Image: "example/extproc:test"})
	resources, err = Rename(resources, "demo", "gateway-system")
	if err != nil {
		t.Fatal(err)
	}
	resources, err = SplitPostAuthResources(resources, "demo", "gateway-system", "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	var sharedBefore []byte
	for i := range resources {
		if resources[i].GetNamespace() == "gateway-system" && resources[i].GetKind() == "ConfigMap" {
			sharedBefore, err = resources[i].MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	binding := &responsesBinding{
		resourceVersion: "revision-2", tenantUID: "tenant-uid",
		secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: responsesRuntimeSecretName("demo")}},
	}
	if err := configureExternalModelExtProcWithResponses(resources, "tenant-a", nil, binding); err != nil {
		t.Fatal(err)
	}
	var config string
	for i := range resources {
		obj := &resources[i]
		if obj.GetNamespace() == "gateway-system" && obj.GetKind() == "ConfigMap" {
			after, err := obj.MarshalJSON()
			if err != nil || string(after) != string(sharedBefore) {
				t.Fatal("shared pre/post config changed")
			}
		}
		if obj.GetNamespace() != "tenant-a" {
			continue
		}
		switch obj.GetKind() {
		case "ConfigMap":
			config, _, err = unstructured.NestedString(obj.Object, "data", "extproc.yaml")
			if err != nil {
				t.Fatal(err)
			}
		case "Deployment":
			containers, _, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
			if err != nil {
				t.Fatal(err)
			}
			container, ok := containers[0].(map[string]any)
			if !ok {
				t.Fatal("malformed container")
			}
			service, _, err := unstructured.NestedString(container, "readinessProbe", "grpc", "service")
			if err != nil || service != "envoy.service.ext_proc.v3.ExternalProcessor" {
				t.Fatal("readiness must honor NotServing")
			}
			if _, found, _ := unstructured.NestedFieldNoCopy(container, "readinessProbe", "tcpSocket"); found {
				t.Fatal("TCP readiness must be removed")
			}
			init, _, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "initContainers")
			if err != nil || len(init) != 1 {
				t.Fatal("key preparation init container missing")
			}
			revision, _, err := unstructured.NestedString(obj.Object, "spec", "template", "metadata", "annotations", "ai-gateway-controller.opendatahub.io/responses-binding-revision")
			if err != nil || revision != "revision-2" {
				t.Fatal("binding change must trigger rollout")
			}
			volumes, _, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "volumes")
			if err != nil {
				t.Fatal(err)
			}
			for _, raw := range volumes {
				volume, ok := raw.(map[string]any)
				if !ok {
					t.Fatal("malformed volume")
				}
				if volume["name"] == "responses-binding" {
					name, _, err := unstructured.NestedString(volume, "secret", "secretName")
					if err != nil || name != responsesRuntimeSecretName("demo") {
						t.Fatal("wrong Secret binding")
					}
				}
			}
		}
	}
	for _, want := range []string{
		"database_url_file:", "ssl_mode: verify-full", "require_certificate_authentication: true", "subject: {header: X-MaaS-Username}",
		"tenant: {static: \"tenant-uid\"}", "openai_responses_rehydrate", "openai_conversations", "conditions: [{when: {path_prefix: /v1/responses}}]",
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("config missing %s", want)
		}
	}
	if strings.Contains(config, "\n        database_url:") {
		t.Fatal("URL must stay out of the ConfigMap")
	}
	var parsed map[string]any
	if err := yaml.UnmarshalStrict([]byte(config), &parsed); err != nil {
		t.Fatal(err)
	}
	assertConversationFilterScopes(t, parsed)
	if path := os.Getenv("PRAXIS_TEST_CONFIG_OUT"); path != "" {
		//nolint:gosec // Test-runner-selected fixture path, never API input.
		if err := os.WriteFile(path, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResponsesMountsPreserveUnrelatedFields(t *testing.T) {
	mount := map[string]any{"name": "existing", "mountPath": "/existing", "futureField": "preserved"}
	volume := map[string]any{"name": "existing", "emptyDir": map[string]any{}, "futureField": "preserved"}
	init := map[string]any{"name": "existing", "image": "init:test", "futureField": "preserved"}
	sidecar := map[string]any{"name": "sidecar", "image": "sidecar:test", "futureField": "preserved"}
	deployment := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
		"futureField": "preserved",
		"containers":  []any{map[string]any{"name": "extproc", "image": "extproc:test", "futureField": "preserved", "volumeMounts": []any{mount}}, sidecar},
		"volumes":     []any{volume}, "initContainers": []any{init},
	}}}}}
	binding := responsesBinding{tenantUID: "tenant-uid", resourceVersion: "revision", secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "runtime"}}}
	for range 2 {
		if err := mountResponsesBinding(deployment, binding); err != nil {
			t.Fatal(err)
		}
	}
	spec, _, err := unstructured.NestedMap(deployment.Object, "spec", "template", "spec")
	if err != nil {
		t.Fatal(err)
	}
	containers, _, err := unstructured.NestedSlice(spec, "containers")
	if err != nil || len(containers) != 2 {
		t.Fatal("containers changed")
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		t.Fatal("malformed serving container")
	}
	if spec["futureField"] != "preserved" || container["futureField"] != "preserved" || !reflect.DeepEqual(containers[1], sidecar) {
		t.Fatal("unrelated pod/container fields changed")
	}
	for field, original := range map[string]map[string]any{"volumes": volume, "initContainers": init} {
		items, _, err := unstructured.NestedSlice(spec, field)
		if err != nil || len(items) == 0 || !reflect.DeepEqual(items[0], original) {
			t.Fatalf("existing %s changed", field)
		}
	}
	mounts, _, err := unstructured.NestedSlice(container, "volumeMounts")
	if err != nil || len(mounts) == 0 || !reflect.DeepEqual(mounts[0], mount) {
		t.Fatal("existing mount changed")
	}
	var pod corev1.PodSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(spec, &pod); err != nil {
		t.Fatal(err)
	}
	if len(pod.Volumes) != 3 || len(pod.InitContainers) != 2 || len(pod.Containers[0].VolumeMounts) != 3 {
		t.Fatal("repeated configuration duplicated Responses resources")
	}
	if *pod.Volumes[1].Secret.DefaultMode != 0444 || len(pod.Volumes[1].Secret.Items) != 4 || pod.Volumes[2].EmptyDir.Medium != corev1.StorageMediumMemory || pod.Volumes[2].EmptyDir.SizeLimit.String() != "1Mi" {
		t.Fatal("Responses volume settings changed")
	}
	keyInit := pod.InitContainers[1]
	security := keyInit.SecurityContext
	if keyInit.Image != "extproc:test" || !keyInit.VolumeMounts[0].ReadOnly || keyInit.VolumeMounts[1].ReadOnly {
		t.Fatal("client key preparation mounts changed")
	}
	if *security.AllowPrivilegeEscalation || !*security.ReadOnlyRootFilesystem || !*security.RunAsNonRoot ||
		security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault || !reflect.DeepEqual(security.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		t.Fatal("client key preparation settings changed")
	}
}

func assertConversationFilterScopes(t *testing.T, parsed map[string]any) {
	t.Helper()
	chains, _, err := unstructured.NestedSlice(parsed, "filter_chains")
	if err != nil || len(chains) != 1 {
		t.Fatal("expected one post-auth chain")
	}
	chain, ok := chains[0].(map[string]any)
	if !ok {
		t.Fatal("malformed filter chain")
	}
	filters, _, err := unstructured.NestedSlice(chain, "filters")
	if err != nil {
		t.Fatal(err)
	}
	conversationPaths := map[string]bool{}
	for _, raw := range filters {
		filter, ok := raw.(map[string]any)
		if !ok {
			t.Fatal("malformed filter")
		}
		if filter["filter"] != "openai_conversations" {
			continue
		}
		conditions, _, err := unstructured.NestedSlice(filter, "conditions")
		if err != nil || len(conditions) != 1 {
			t.Fatal("each Conversations entry must have one path condition")
		}
		condition, ok := conditions[0].(map[string]any)
		if !ok {
			t.Fatal("malformed condition")
		}
		path, _, err := unstructured.NestedString(condition, "when", "path_prefix")
		if err != nil || (path != "/v1/conversations" && path != "/v1/responses") || conversationPaths[path] {
			t.Fatalf("unexpected or duplicate Conversations path %q", path)
		}
		conversationPaths[path] = true
		for key, want := range map[string]string{"conversations_table": "openai_conversations", "items_table": "openai_conversation_items"} {
			if filter[key] != want {
				t.Fatalf("Conversations entries must share %s", key)
			}
		}
	}
	if len(conversationPaths) != 2 {
		t.Fatal("Conversations must handle state operations and Responses append-back")
	}
}

func TestResponsesRoutesHaveFailClosedSinkAndExactFilterOverrides(t *testing.T) {
	scheme := mtcSchemeForTests()
	scheme.AddKnownTypeWithName(responsesRouteGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(responsesRouteGVK.GroupVersion().WithKind("HTTPRouteList"), &unstructured.UnstructuredList{})
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "provider-openai", Namespace: "tenant-a", Labels: map[string]string{LabelManagedBy: ManagedByAIGatewayController}},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "api.example.test", Ports: []corev1.ServicePort{{Port: 443}}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(service).Build()
	owner := newAITenantOwner("demo", "owners", AITenantPhaseActive, "gateway", "gateway-system", "tenant-a")
	r := &Reconciler{Client: c}
	resources, err := r.responsesRouteResources(t.Context(), owner, "demo", "tenant-a", "gateway", "gateway-system")
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 3 {
		t.Fatal("sink, route and filter are required")
	}
	rules, _, err := unstructured.NestedSlice(resources[1].Object, "spec", "rules")
	if err != nil || len(rules) != 2 {
		t.Fatalf("rules=%v error=%v", rules, err)
	}
	for _, raw := range rules {
		rule, ok := raw.(map[string]any)
		if !ok {
			t.Fatal("malformed rule")
		}
		backends, _, err := unstructured.NestedSlice(rule, "backendRefs")
		if err != nil || len(backends) != 1 {
			t.Fatal("missing backend")
		}
		backend, ok := backends[0].(map[string]any)
		if !ok {
			t.Fatal("malformed backend")
		}
		if backend["name"] != "responses-api-sink-demo" {
			t.Fatal("stored content must never route to a provider")
		}
	}
	patches, _, err := unstructured.NestedSlice(resources[2].Object, "spec", "configPatches")
	if err != nil || len(patches) != len(rules) {
		t.Fatal("every rule needs an exact ExtProc override")
	}
	for index, raw := range patches {
		patch, ok := raw.(map[string]any)
		if !ok {
			t.Fatal("malformed patch")
		}
		name, _, err := unstructured.NestedString(patch, "match", "routeConfiguration", "vhost", "route", "name")
		if err != nil || !strings.Contains(name, ".responses-api-demo.") {
			t.Fatalf("wildcard override at %d", index)
		}
		mode, _, err := unstructured.NestedString(patch, "patch", "value", "typed_per_filter_config", "envoy.filters.http.ext_proc.external-model", "overrides", "processing_mode", "request_body_mode")
		if err != nil || mode != "BUFFERED" {
			t.Fatal("managed create requests must remain buffered before upstream dispatch")
		}
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(resources[0].Object, "spec", "selector"); found {
		t.Fatal("sink must have no endpoints")
	}
	other := newAITenantOwner("other", "owners", AITenantPhaseActive, "gateway", "gateway-system", "tenant-b")
	if err := c.Create(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	if _, err := r.responsesRouteResources(t.Context(), owner, "demo", "tenant-a", "gateway", "gateway-system"); err == nil {
		t.Fatal("shared Gateway must be rejected")
	}
}

func TestResponsesCleanupRetainsDatabaseBinding(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: responsesSecretName, Namespace: "infra"}}
	isolation, err := externalModelNetworkPolicy("demo", "tenant-a", "gateway", "gateway-system")
	if err != nil {
		t.Fatal(err)
	}
	resources := []client.Object{secret}
	for _, obj := range []unstructured.Unstructured{
		{Object: map[string]any{"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute", "metadata": map[string]any{"name": "responses-api-demo", "namespace": "tenant-a"}}},
		{Object: map[string]any{"apiVersion": "networking.istio.io/v1alpha3", "kind": "EnvoyFilter", "metadata": map[string]any{"name": "responses-api-filters-demo", "namespace": "gateway-system"}}},
		{Object: map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": map[string]any{"name": "responses-api-sink-demo", "namespace": "tenant-a"}}},
		isolation,
	} {
		obj.SetLabels(map[string]string{LabelManagedBy: ManagedByAIGatewayController})
		resources = append(resources, obj.DeepCopy())
	}
	c := fake.NewClientBuilder().WithScheme(mtcSchemeForTests()).WithObjects(resources...).Build()
	if err := (&Reconciler{Client: c}).cleanupResponsesRoutes(t.Context(), "demo", "tenant-a", "gateway-system"); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(secret), &corev1.Secret{}); err != nil {
		t.Fatal("database binding must survive cleanup")
	}
	policyKey := client.ObjectKey{Namespace: "tenant-a", Name: "external-model-extproc-demo"}
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(gvkNetworkPolicy)
	if err := c.Get(t.Context(), policyKey, policy); err != nil {
		t.Fatalf("isolation must survive API cleanup for draining pods: %v", err)
	}
	if err := (&Reconciler{Client: c}).cleanup(t.Context(), "demo", "gateway-system", "tenant-a"); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), policyKey, policy); !apierrors.IsNotFound(err) {
		t.Fatal("tenant cleanup must remove the isolation policy")
	}
}

func TestResponsesApplyFenceRejectsNewAlreadyReadyGeneration(t *testing.T) {
	owner := newAITenantOwner("demo", "owners", AITenantPhaseActive, "gateway", "gateway-system", "tenant-a")
	owner.SetUID("owner-uid")
	owner.SetGeneration(2)
	conditions, _, err := unstructured.NestedSlice(owner.Object, "status", "conditions")
	if err != nil {
		t.Fatal(err)
	}
	condition, ok := conditions[0].(map[string]any)
	if !ok {
		t.Fatal("malformed condition")
	}
	condition["observedGeneration"] = int64(2)
	if err := unstructured.SetNestedSlice(owner.Object, conditions, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(mtcSchemeForTests()).WithObjects(owner).Build()
	mtc := newMTC("tenant-a", "demo", PayloadProcessingBackendPraxis, "demo", "owners")
	r := &Reconciler{Client: c, APIReader: c}
	valid, err := r.aiTenantStillValidForApply(t.Context(), mtc, owner.GetUID(), "gateway", "gateway-system", 1)
	if err != nil || valid {
		t.Fatalf("stale Responses spec must not be applied: valid=%v error=%v", valid, err)
	}
	valid, err = r.aiTenantStillValidForApply(t.Context(), mtc, owner.GetUID(), "gateway", "gateway-system", 2)
	if err != nil || !valid {
		t.Fatalf("current Responses spec must pass: valid=%v error=%v", valid, err)
	}
}
