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

package render

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestApplyUsesServerSideApplyWithFieldOwnerAndForce(t *testing.T) {
	var (
		gotPatchType client.Patch
		gotOpts      []client.PatchOption
		gotObj       client.Object
	)

	fakeClient := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			gotPatchType = patch
			gotOpts = opts
			gotObj = obj
			return nil
		},
	}).Build()

	resource := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":            "payload-processing-plugins",
			"namespace":       "istio-system",
			"resourceVersion": "123",
			"managedFields":   []any{map[string]any{"manager": "someone-else"}},
		},
		"status": map[string]any{"phase": "leftover"},
		"data":   map[string]any{"key": "value"},
	}}

	if err := Apply(context.Background(), fakeClient, []unstructured.Unstructured{resource}); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}

	//nolint:staticcheck // asserting the deprecated-but-still-required client.Apply patch type; see apply.go.
	if gotPatchType != client.Apply {
		t.Fatalf("patch type = %v, want client.Apply (server-side apply)", gotPatchType)
	}

	u, ok := gotObj.(*unstructured.Unstructured)
	if !ok {
		t.Fatalf("patched object is %T, want *unstructured.Unstructured", gotObj)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "metadata", "managedFields"); found {
		t.Error("managedFields was not stripped before patching")
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "metadata", "resourceVersion"); found {
		t.Error("resourceVersion was not stripped before patching")
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "status"); found {
		t.Error("status was not stripped before patching")
	}
	if data, found, _ := unstructured.NestedStringMap(u.Object, "data"); !found || data["key"] != "value" {
		t.Error("Apply must not strip fields other than managedFields/resourceVersion/status")
	}

	var (
		haveFieldOwner bool
		haveForce      bool
	)
	for _, opt := range gotOpts {
		if fo, ok := opt.(client.FieldOwner); ok {
			haveFieldOwner = string(fo) == FieldOwner
		}
		if opt == client.ForceOwnership {
			haveForce = true
		}
	}
	if !haveFieldOwner {
		t.Errorf("expected client.FieldOwner(%q) patch option, got %v", FieldOwner, gotOpts)
	}
	if !haveForce {
		t.Errorf("expected client.ForceOwnership patch option, got %v", gotOpts)
	}
}

func TestApplyPropagatesPatchError(t *testing.T) {
	fakeClient := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
			return errors.New("boom")
		},
	}).Build()

	resource := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "payload-processing-plugins", "namespace": "istio-system"},
	}}

	err := Apply(context.Background(), fakeClient, []unstructured.Unstructured{resource})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
}
