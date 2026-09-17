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
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func migrationTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(MaasTenantConfigGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(MaasTenantConfigGVK.GroupVersion().WithKind("MaasTenantConfigList"), &unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(gvkDeployment, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gvkDeployment.GroupVersion().WithKind("DeploymentList"), &unstructured.UnstructuredList{})
	return scheme
}

func newMTCFixture(namespace string, annotations map[string]string) *unstructured.Unstructured {
	u := NewMaasTenantConfig()
	u.SetName(MaasTenantConfigInstanceName)
	u.SetNamespace(namespace)
	if annotations != nil {
		u.SetAnnotations(annotations)
	}
	return u
}

func TestPraxisBundleExists(t *testing.T) {
	const (
		gwNS     = "openshift-ingress"
		tenantID = "team-a"
	)
	scheme := migrationTestScheme()

	t.Run("absent when no Deployment exists", func(t *testing.T) {
		cl := fake.NewClientBuilder().WithScheme(scheme).Build()
		got, err := PraxisBundleExists(context.Background(), cl, tenantID, gwNS)
		if err != nil {
			t.Fatalf("PraxisBundleExists: %v", err)
		}
		if got {
			t.Fatal("got true, want false when no Deployment exists")
		}
	})

	t.Run("present when Deployment exists", func(t *testing.T) {
		dep := &unstructured.Unstructured{}
		dep.SetGroupVersionKind(gvkDeployment)
		dep.SetName(PayloadProcessingDeploymentName(tenantID))
		dep.SetNamespace(gwNS)
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep).Build()
		got, err := PraxisBundleExists(context.Background(), cl, tenantID, gwNS)
		if err != nil {
			t.Fatalf("PraxisBundleExists: %v", err)
		}
		if !got {
			t.Fatal("got false, want true when the Deployment exists")
		}
	})
}

func TestClaimIPPMigrationMarker(t *testing.T) {
	scheme := migrationTestScheme()

	// TestClaimIPPMigrationMarker/absent_marker_is_blocked covers both the
	// "cleanup genuinely in flight" case and the "tenant has never swapped
	// backends before" case: absent is the marker's blocked resting state in
	// both. maas-controller seeds every new MaasTenantConfig with
	// IPPMigrationMarkerClearValue at creation time precisely so a
	// brand-new tenant never hits this path; this fixture models a tenant
	// this controller reconciles without that seeding ever having applied to
	// it (e.g. a pre-existing tenant, or a unit test that constructs the
	// fixture directly).
	t.Run("absent marker is blocked", func(t *testing.T) {
		mtc := newMTCFixture("ns-absent", nil)
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mtc).Build()

		claimed, err := ClaimIPPMigrationMarker(context.Background(), cl, mtc)
		if err != nil {
			t.Fatalf("ClaimIPPMigrationMarker: %v", err)
		}
		if claimed {
			t.Fatal("claimed = true, want false: an absent marker must block a transitioning-in party")
		}
	})

	t.Run("cleared marker is claimable and claiming deletes it", func(t *testing.T) {
		mtc := newMTCFixture("ns-clear", map[string]string{AnnotationIPPMigrationCleanupComplete: IPPMigrationMarkerClearValue})
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mtc).Build()

		claimed, err := ClaimIPPMigrationMarker(context.Background(), cl, mtc)
		if err != nil {
			t.Fatalf("ClaimIPPMigrationMarker: %v", err)
		}
		if !claimed {
			t.Fatal("claimed = false, want true for a cleared marker")
		}

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(MaasTenantConfigGVK)
		if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "ns-clear", Name: MaasTenantConfigInstanceName}, got); err != nil {
			t.Fatalf("Get: %v", err)
		}
		if _, present := got.GetAnnotations()[AnnotationIPPMigrationCleanupComplete]; present {
			t.Fatal("claiming must delete the annotation (return it to its blocked resting state), not write a sentinel value")
		}
	})

	t.Run("unexpected non-clear value is also blocked", func(t *testing.T) {
		mtc := newMTCFixture("ns-unexpected", map[string]string{AnnotationIPPMigrationCleanupComplete: "some-other-value"})
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mtc).Build()

		claimed, err := ClaimIPPMigrationMarker(context.Background(), cl, mtc)
		if err != nil {
			t.Fatalf("ClaimIPPMigrationMarker: %v", err)
		}
		if claimed {
			t.Fatal("claimed = true, want false: only the exact IPPMigrationMarkerClearValue may be claimed")
		}
	})

	t.Run("missing tenant config surfaces an error, not a false claim", func(t *testing.T) {
		mtc := newMTCFixture("ns-missing", nil)
		cl := fake.NewClientBuilder().WithScheme(scheme).Build() // not seeded

		claimed, err := ClaimIPPMigrationMarker(context.Background(), cl, mtc)
		if err == nil {
			t.Fatal("expected an error when the MaasTenantConfig does not exist")
		}
		if claimed {
			t.Fatal("claimed = true, want false on error")
		}
	})

	// TestClaimIPPMigrationMarker/concurrent_claim_attempts simulates the
	// exact race this mechanism exists to close: two independent readers
	// (standing in for maas-controller and ai-gateway-controller) both
	// observe the marker as "clear to deploy" from the same object
	// revision, and both attempt to claim it. Exactly one of the two
	// concurrent Updates must win; the other must observe a Conflict
	// (surfaced here as claimed=false, err=nil) and back off rather than
	// also proceeding to deploy.
	t.Run("concurrent claim attempts: exactly one wins", func(t *testing.T) {
		mtc := newMTCFixture("ns-flip-flop", map[string]string{AnnotationIPPMigrationCleanupComplete: IPPMigrationMarkerClearValue})
		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mtc).Build()

		readerA := &unstructured.Unstructured{}
		readerA.SetGroupVersionKind(MaasTenantConfigGVK)
		if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "ns-flip-flop", Name: MaasTenantConfigInstanceName}, readerA); err != nil {
			t.Fatalf("Get readerA: %v", err)
		}
		readerB := &unstructured.Unstructured{}
		readerB.SetGroupVersionKind(MaasTenantConfigGVK)
		if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "ns-flip-flop", Name: MaasTenantConfigInstanceName}, readerB); err != nil {
			t.Fatalf("Get readerB: %v", err)
		}

		claimedA, errA := ClaimIPPMigrationMarker(context.Background(), cl, readerA)
		if errA != nil {
			t.Fatalf("claim A: %v", errA)
		}
		claimedB, errB := ClaimIPPMigrationMarker(context.Background(), cl, readerB)
		if errB != nil {
			t.Fatalf("claim B: %v", errB)
		}

		if claimedA == claimedB {
			t.Fatalf("claimedA=%v claimedB=%v: exactly one of the two concurrent claim attempts must win", claimedA, claimedB)
		}
	})
}

func TestMarkIPPMigrationCleanupComplete(t *testing.T) {
	scheme := migrationTestScheme()
	mtc := newMTCFixture("ns-a", nil) // absent marker: represents a cleanup in flight
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mtc).Build()

	if err := MarkIPPMigrationCleanupComplete(context.Background(), cl, mtc); err != nil {
		t.Fatalf("MarkIPPMigrationCleanupComplete: %v", err)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(MaasTenantConfigGVK)
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "ns-a", Name: MaasTenantConfigInstanceName}, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v := got.GetAnnotations()[AnnotationIPPMigrationCleanupComplete]; v != IPPMigrationMarkerClearValue {
		t.Fatalf("marker = %q, want %q", v, IPPMigrationMarkerClearValue)
	}
}

