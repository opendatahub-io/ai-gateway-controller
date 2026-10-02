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
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ContainerResources is a full-replacement resources block for a Deployment
// container. Quantity values are already string-encoded (e.g. "128Mi", "100m")
// as stored on MaasTenantConfig.spec.*.resources.
type ContainerResources struct {
	Requests map[string]string
	Limits   map[string]string
}

// PayloadProcessingResources reads
// MaasTenantConfig.spec.payloadProcessing.resources. Absent or empty yields nil
// so PostRender keeps the vendored manifest defaults.
func PayloadProcessingResources(mtc *unstructured.Unstructured) *ContainerResources {
	return workloadResourcesFromSpec(mtc, "payloadProcessing")
}

// PayloadPreProcessingResources reads
// MaasTenantConfig.spec.payloadPreProcessing.resources.
func PayloadPreProcessingResources(mtc *unstructured.Unstructured) *ContainerResources {
	return workloadResourcesFromSpec(mtc, "payloadPreProcessing")
}

func workloadResourcesFromSpec(mtc *unstructured.Unstructured, field string) *ContainerResources {
	if mtc == nil {
		return nil
	}
	raw, found, err := unstructured.NestedMap(mtc.Object, "spec", field, "resources")
	if err != nil || !found || len(raw) == 0 {
		return nil
	}
	out := &ContainerResources{}
	if requests, ok, _ := unstructured.NestedStringMap(raw, "requests"); ok && len(requests) > 0 {
		out.Requests = requests
	}
	if limits, ok, _ := unstructured.NestedStringMap(raw, "limits"); ok && len(limits) > 0 {
		out.Limits = limits
	}
	if len(out.Requests) == 0 && len(out.Limits) == 0 {
		return nil
	}
	return out
}

// ApplyWorkloadResources patches renamed payload-processing and
// payload-pre-processing Deployments with MaasTenantConfig resource overrides.
// Nil overrides leave the corresponding Deployment unchanged.
func ApplyWorkloadResources(
	resources []unstructured.Unstructured,
	tenantID string,
	processing, preProcessing *ContainerResources,
) error {
	processingName := PayloadProcessingDeploymentName(tenantID)
	preProcessingName := PayloadPreProcessingDeploymentName(tenantID)

	for i := range resources {
		u := &resources[i]
		if u.GetKind() != "Deployment" {
			continue
		}
		switch u.GetName() {
		case processingName:
			if processing == nil {
				continue
			}
			if err := setContainerResources(u, PayloadProcessingName, processing); err != nil {
				return fmt.Errorf("payload-processing resources: %w", err)
			}
		case preProcessingName:
			if preProcessing == nil {
				continue
			}
			if err := setContainerResources(u, PayloadPreProcessingName, preProcessing); err != nil {
				return fmt.Errorf("payload-pre-processing resources: %w", err)
			}
		}
	}
	return nil
}

func setContainerResources(u *unstructured.Unstructured, containerName string, res *ContainerResources) error {
	containers, found, err := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "containers")
	if err != nil || !found {
		return fmt.Errorf("containers not found")
	}
	for i, c := range containers {
		cm, ok := c.(map[string]any)
		if !ok || cm["name"] != containerName {
			continue
		}
		resMap := make(map[string]any)
		if len(res.Requests) > 0 {
			req := make(map[string]any, len(res.Requests))
			for k, v := range res.Requests {
				req[k] = v
			}
			resMap["requests"] = req
		}
		if len(res.Limits) > 0 {
			lim := make(map[string]any, len(res.Limits))
			for k, v := range res.Limits {
				lim[k] = v
			}
			resMap["limits"] = lim
		}
		cm["resources"] = resMap
		containers[i] = cm
		return unstructured.SetNestedSlice(u.Object, containers, "spec", "template", "spec", "containers")
	}
	return fmt.Errorf("container %q not found", containerName)
}
