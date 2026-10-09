/*
Copyright 2026 The opendatahub.io Authors.

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

package guardrail

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestCheckID(t *testing.T) {
	tests := []struct {
		name          string
		namespace     string
		guardrailName string
		checkName     string
		want          string
	}{
		{
			name:          "base identity",
			namespace:     "tenant-ns",
			guardrailName: "safety",
			checkName:     "toxicity",
			want:          "b95b4f7de6306e96c5e33d32e945b5044f61cf0a2d26fead8ae67c1fdd1fc59b",
		},
		{
			name:          "different namespace",
			namespace:     "tenant-alt",
			guardrailName: "safety",
			checkName:     "toxicity",
			want:          "a823c3e4d6b396f6de309cb50591f90a6edecc4d61164dbb7f409ebdb4c6310e",
		},
		{
			name:          "different guardrail name",
			namespace:     "tenant-ns",
			guardrailName: "content-safety",
			checkName:     "toxicity",
			want:          "e5895ca8d8aa8ef4b767f07410ba0453b06bfabe382560a70c811421a179a42c",
		},
		{
			name:          "different check name",
			namespace:     "tenant-ns",
			guardrailName: "safety",
			checkName:     "pii",
			want:          "c6ac1d644fd97a8576beda5fee53688718ddefcfd8c3b87ffd7d6306f71bc4dd",
		},
		{
			name:          "JSON escaping",
			namespace:     "tenant\"ns",
			guardrailName: "guard\\rail",
			checkName:     "check\n<>&",
			want:          "eb52b9d3e193855536703f3f1bbde48b4151c7891e17042be2cbc53f1b71cd22",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := CheckID(tt.namespace, tt.guardrailName, tt.checkName)
			if first != tt.want {
				t.Fatalf("CheckID() = %q, want %q", first, tt.want)
			}

			if second := CheckID(tt.namespace, tt.guardrailName, tt.checkName); second != first {
				t.Errorf("repeated CheckID() = %q, want %q", second, first)
			}
		})
	}
}

func TestCheckHeaderName(t *testing.T) {
	const wantID = "b95b4f7de6306e96c5e33d32e945b5044f61cf0a2d26fead8ae67c1fdd1fc59b"

	header := CheckHeaderName("tenant-ns", "safety", "toxicity")
	if want := "x-aigateway-guardrail-" + wantID; header != want {
		t.Fatalf("CheckHeaderName() = %q, want %q", header, want)
	}

	id := strings.TrimPrefix(header, checkHeaderPrefix)
	if len(id) != sha256.Size*2 {
		t.Errorf("CheckID length = %d, want %d", len(id), sha256.Size*2)
	}
	if _, err := hex.DecodeString(id); err != nil {
		t.Errorf("CheckID %q is not hexadecimal: %v", id, err)
	}
	if id != strings.ToLower(id) {
		t.Errorf("CheckID %q is not lowercase", id)
	}
}
