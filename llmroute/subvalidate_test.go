package llmroute

import (
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
)

// TestProvisionRequiresSub locks the config-load-time validation: models
// without a sub must fail provisioning, not panic per request.
func TestProvisionRequiresSub(t *testing.T) {
	// Provision validates before using ctx, so a zero Context suffices to
	// reach the SubRaw check.
	r := &Route{Models: []ModelRule{{Pattern: "m"}}}
	err := r.Provision(caddy.Context{})
	if err == nil {
		t.Fatal("Provision must reject models without a sub")
	}
	if !strings.Contains(err.Error(), "requires a sub") {
		t.Errorf("unexpected error: %v", err)
	}
}
