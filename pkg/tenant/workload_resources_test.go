/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

    10|Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tenant

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestWorkloadResourcesFromSpec(t *testing.T) {
	mtc := NewMaasTenantConfig()
	mtc.Object["spec"] = map[string]any{
		"payloadProcessing": map[string]any{
			"resources": map[string]any{
				"requests": map[string]any{"cpu": "125m", "memory": "192Mi"},
				"limits":   map[string]any{"cpu": "600m", "memory": "640Mi"},
			},
		},
		"payloadPreProcessing": map[string]any{
			"resources": map[string]any{
				"requests": map[string]any{"cpu": "75m", "memory": "96Mi"},
				"limits":   map[string]any{"cpu": "350m", "memory": "384Mi"},
			},
		},
	}

	processing := PayloadProcessingResources(mtc)
	if processing == nil {
		t.Fatal("expected payloadProcessing resources")
	}
	if processing.Requests["cpu"] != "125m" || processing.Limits["memory"] != "640Mi" {
		t.Fatalf("unexpected payloadProcessing resources: %+v", processing)
	}

	pre := PayloadPreProcessingResources(mtc)
	if pre == nil {
		t.Fatal("expected payloadPreProcessing resources")
	}
	if pre.Requests["memory"] != "96Mi" || pre.Limits["cpu"] != "350m" {
		t.Fatalf("unexpected payloadPreProcessing resources: %+v", pre)
	}

	if PayloadProcessingResources(NewMaasTenantConfig()) != nil {
		t.Fatal("empty MaasTenantConfig should yield nil overrides")
	}
}

func TestApplyWorkloadResourcesDistinctOverrides(t *testing.T) {
	resources := []unstructured.Unstructured{
		deploymentWithContainer(PayloadProcessingDeploymentName("oidc"), PayloadProcessingName),
		deploymentWithContainer(PayloadPreProcessingDeploymentName("oidc"), PayloadPreProcessingName),
	}

	processing := &ContainerResources{
		Requests: map[string]string{"cpu": "125m", "memory": "192Mi"},
		Limits:   map[string]string{"cpu": "600m", "memory": "640Mi"},
	}
	pre := &ContainerResources{
		Requests: map[string]string{"cpu": "75m", "memory": "96Mi"},
		Limits:   map[string]string{"cpu": "350m", "memory": "384Mi"},
	}
	if err := ApplyWorkloadResources(resources, "oidc", processing, pre); err != nil {
		t.Fatalf("ApplyWorkloadResources: %v", err)
	}

	gotProcessing := containerResources(t, resources, PayloadProcessingDeploymentName("oidc"), PayloadProcessingName)
	gotPre := containerResources(t, resources, PayloadPreProcessingDeploymentName("oidc"), PayloadPreProcessingName)
	assertResourceMap(t, gotProcessing, "requests", processing.Requests)
	assertResourceMap(t, gotProcessing, "limits", processing.Limits)
	assertResourceMap(t, gotPre, "requests", pre.Requests)
	assertResourceMap(t, gotPre, "limits", pre.Limits)
}

func deploymentWithContainer(name, containerName string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{
						map[string]any{
							"name": containerName,
							"resources": map[string]any{
								"requests": map[string]any{"cpu": "50m", "memory": "64Mi"},
								"limits":   map[string]any{"cpu": "500m", "memory": "256Mi"},
							},
						},
					},
				},
			},
		},
	}}
}

func containerResources(t *testing.T, resources []unstructured.Unstructured, deploymentName, containerName string) map[string]any {
	t.Helper()
	for i := range resources {
		u := &resources[i]
		if u.GetKind() != "Deployment" || u.GetName() != deploymentName {
			continue
		}
		containers, _, err := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "containers")
		if err != nil {
			t.Fatalf("NestedSlice: %v", err)
		}
		for _, c := range containers {
			cm, ok := c.(map[string]any)
			if !ok || cm["name"] != containerName {
				continue
			}
			res, _ := cm["resources"].(map[string]any)
			return res
		}
		t.Fatalf("container %q not found on %s", containerName, deploymentName)
	}
	t.Fatalf("deployment %q not found", deploymentName)
	return nil
}

func assertResourceMap(t *testing.T, resources map[string]any, section string, want map[string]string) {
	t.Helper()
	got, _ := resources[section].(map[string]any)
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s.%s: got %v want %v (full=%v)", section, k, got[k], v, resources)
		}
	}
}
