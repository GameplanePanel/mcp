package main

import (
	"context"
	"strings"
	"testing"
)

// A token without a panel URL must stop start-up rather than silently fall
// back to break-glass mode against the local kubeconfig.
func TestRunServeRefusesTokenWithoutPanelURL(t *testing.T) {
	t.Setenv("GAMEPLANE_PANEL_URL", "")
	t.Setenv("GAMEPLANE_MCP_TOKEN", "gpmcp_x")
	err := runServe(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "GAMEPLANE_PANEL_URL") {
		t.Fatalf("runServe error = %v, want one naming GAMEPLANE_PANEL_URL", err)
	}
}
