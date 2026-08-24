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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// FieldOwner is the SSA field manager used for every resource this
// controller applies, so ownership handoff between reconciles stays clean.
const FieldOwner = "ai-gateway-controller"

// Apply server-side-applies each resource with client.ForceOwnership: this
// controller is the sole manager of the praxis-extproc resources it renders,
// so a clean field-manager handoff is intentional, not a conflict to avoid.
func Apply(ctx context.Context, c client.Client, resources []unstructured.Unstructured) error {
	for i := range resources {
		u := resources[i].DeepCopy()
		unstructured.RemoveNestedField(u.Object, "metadata", "managedFields")
		unstructured.RemoveNestedField(u.Object, "metadata", "resourceVersion")
		unstructured.RemoveNestedField(u.Object, "status")

		//nolint:staticcheck // client.Client.Apply() only accepts typed runtime.ApplyConfiguration
		// objects; unstructured resources (including third-party CRDs like EnvoyFilter/DestinationRule
		// with no generated apply-configuration type) must still use client.Patch with client.Apply.
		if err := c.Patch(ctx, u, client.Apply, client.FieldOwner(FieldOwner), client.ForceOwnership); err != nil {
			return fmt.Errorf("apply %s %s/%s: %w", u.GetKind(), u.GetNamespace(), u.GetName(), err)
		}
	}
	return nil
}
