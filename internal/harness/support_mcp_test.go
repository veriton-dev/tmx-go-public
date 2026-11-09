package harness

import (
	"github.com/blackcode-ch/tmx-go/internal/runner"
	"strings"
	"testing"
)

func TestSupportMCPApprovalScope(t *testing.T) {
	for _, tc := range []struct {
		name, home string
		full, want bool
	}{
		{"support-agent", "guarded-home", false, true},
		{"other", "guarded-home", false, false},
		{"support-agent", "", false, false},
		{"support-agent", "guarded-home", true, false},
	} {
		args := strings.Join(CodexAppServerArgs(runner.Spec{Name: tc.name, CodexGuardHome: tc.home, CodexFullAccess: tc.full, MCP: &runner.MCPServer{Command: "tmxg", Args: []string{"mcp"}}}), " ")
		if strings.Contains(args, `tools.chat_send.approval_mode="approve"`) != tc.want || strings.Contains(args, `tools.chat_read.approval_mode="approve"`) != tc.want {
			t.Fatalf("wrong scope: %s", args)
		}
		if strings.Contains(args, "default_tools_approval_mode") {
			t.Fatal("blanket MCP approval")
		}
	}
}
