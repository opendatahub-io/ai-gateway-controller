package consumerportal

import (
	"strings"
	"testing"
)

func TestSanitizeProxyDetailStripsGoLogTimestamps(t *testing.T) {
	raw := "[2026/11/19 05:28:38] [provider.go:57] Performing OIDC Discovery... [2026/11/19 05:28:38] [main.go:83] ERROR: Failed to initialise OAuth2 Proxy"
	got := sanitizeProxyDetail(raw)
	want := "[provider.go:57] Performing OIDC Discovery... [main.go:83] ERROR: Failed to initialise OAuth2 Proxy"
	if got != want {
		t.Fatalf("sanitizeProxyDetail = %q, want %q", got, want)
	}
	iso := "[2026-11-19T18:34:57Z] x509: certificate signed by unknown authority"
	if got := sanitizeProxyDetail(iso); got != "x509: certificate signed by unknown authority" {
		t.Fatalf("ISO format not stripped: %q", got)
	}
}

func TestUpsertProxyConditionDoesNotDowngradeDetail(t *testing.T) {
	existing := []any{map[string]any{
		"type": ConsumerPortalProxyReadyCondition, "status": "False",
		"reason":             "CrashLoopBackOff",
		"message":            proxyUnavailableMessage + ": [main.go:83] ERROR: Failed to initialise OAuth2 Proxy",
		"observedGeneration": float64(5),
	}}
	// Detail-less assessment for the same failure: the captured detail must
	// survive, and no change may be reported.
	next, changed := upsertProxyCondition(existing, map[string]any{
		"type": ConsumerPortalProxyReadyCondition, "status": "False",
		"reason": "CrashLoopBackOff", "message": proxyUnavailableMessage,
		"observedGeneration": int64(5),
	})
	if changed {
		t.Fatal("a detail-less assessment of the same failure must not rewrite the condition")
	}
	message, _ := next[0].(map[string]any)["message"].(string)
	if !strings.Contains(message, "Failed to initialise") {
		t.Fatalf("captured failure detail was downgraded: %v", next[0])
	}
}
