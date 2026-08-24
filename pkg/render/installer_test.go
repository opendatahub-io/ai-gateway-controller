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
	"sync/atomic"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestInstallerNeedsLeaderElection(t *testing.T) {
	installer := &Installer{}
	if !installer.NeedLeaderElection() {
		t.Fatal("NeedLeaderElection() = false, want true: concurrent replicas must not race to SSA-apply")
	}
}

func TestInstallerStartRejectsNonPositiveResyncInterval(t *testing.T) {
	installer := &Installer{ResyncInterval: 0}

	if err := installer.Start(context.Background()); err == nil {
		t.Fatal("expected an error for a non-positive resync interval, got nil")
	}
}

func TestInstallerStartRunsImmediatelyAndOnEveryTick(t *testing.T) {
	const manifestPath = "../../config/manifests/praxis-extproc/overlays/odh"

	var patchCalls atomic.Int32
	fakeClient := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
			patchCalls.Add(1)
			return nil
		},
	}).Build()

	installer := &Installer{
		Client:       fakeClient,
		ManifestPath: manifestPath,
		Params: Params{
			Namespace:        "istio-system",
			GatewayName:      "my-gateway",
			Image:            "quay.io/example/praxis-extproc:v1",
			MaaSAPIRouteName: "maas-api-route",
		},
		ResyncInterval: 10 * time.Millisecond,
	}

	if _, err := Build(manifestPath); err != nil {
		t.Skipf("vendored manifests not present at %s; run hack/scripts/get-manifests.sh first: %v", manifestPath, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Millisecond)
	defer cancel()

	if err := installer.Start(ctx); err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	// One immediate run plus at least one tick; exact count is timing-sensitive,
	// so only assert the lower bound (immediate run + >=1 resync).
	if got := patchCalls.Load(); got < 2 {
		t.Fatalf("expected at least 2 apply passes (immediate + resync) within the test window, got %d", got)
	}
}

func TestInstallerRunLogsAndContinuesOnApplyError(t *testing.T) {
	const manifestPath = "../../config/manifests/praxis-extproc/overlays/odh"

	if _, err := Build(manifestPath); err != nil {
		t.Skipf("vendored manifests not present at %s; run hack/scripts/get-manifests.sh first: %v", manifestPath, err)
	}

	fakeClient := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
			return errors.New("boom")
		},
	}).Build()

	installer := &Installer{
		Client:       fakeClient,
		ManifestPath: manifestPath,
		Params: Params{
			Namespace:        "istio-system",
			GatewayName:      "my-gateway",
			Image:            "quay.io/example/praxis-extproc:v1",
			MaaSAPIRouteName: "maas-api-route",
		},
		ResyncInterval: time.Hour,
	}

	// run() must not panic and must swallow the error (logged, not returned);
	// the caller learns about persistent failure via logs/metrics, not a crash.
	installer.run(context.Background())
}
