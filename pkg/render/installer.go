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
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Installer is a manager.Runnable that renders and SSA-applies the vendored
// praxis-extproc manifests. It does not watch any CR (see DESIGN.md, "Out of
// scope"), so it runs once at startup and again on every tick of
// ResyncInterval; that tick is the only re-apply trigger besides restart.
type Installer struct {
	// Client applies the rendered resources. Must have permission to SSA
	// patch every Kind the vendored overlay contains.
	Client client.Client
	// ManifestPath is the kustomize entrypoint, e.g.
	// config/manifests/praxis-extproc/overlays/odh.
	ManifestPath string
	// Params controls placeholder substitution; see PostRender.
	Params Params
	// ResyncInterval is how often to re-render and re-apply even without an
	// external trigger. Must be positive.
	ResyncInterval time.Duration
	// Log receives one entry per run, plus any render/apply error. Defaults
	// to a no-op logger if unset.
	Log logr.Logger
}

// NeedLeaderElection reports true: only the elected replica should apply,
// so concurrent replicas never race to SSA-patch the same resources.
func (r *Installer) NeedLeaderElection() bool {
	return true
}

// Start runs the install loop until ctx is cancelled, per the
// manager.Runnable contract.
func (r *Installer) Start(ctx context.Context) error {
	if r.ResyncInterval <= 0 {
		return fmt.Errorf("resync interval must be positive, got %v", r.ResyncInterval)
	}

	r.run(ctx)
	ticker := time.NewTicker(r.ResyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.run(ctx)
		}
	}
}

// run performs one render-and-apply pass, logging (not returning) any
// error: a transient failure should not crash the process, since the next
// tick retries.
func (r *Installer) run(ctx context.Context) {
	if err := r.install(ctx); err != nil {
		r.Log.Error(err, "praxis-extproc install failed; will retry next resync", "manifestPath", r.ManifestPath)
		return
	}
	r.Log.Info("praxis-extproc install applied", "namespace", r.Params.Namespace, "gatewayName", r.Params.GatewayName)
}

func (r *Installer) install(ctx context.Context) error {
	rendered, err := Build(r.ManifestPath)
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}

	resources := PostRender(rendered, r.Params)

	if err := Apply(ctx, r.Client, resources); err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	return nil
}
