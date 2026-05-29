package toolfmt

import "testing"

func TestInternal(t *testing.T) {
	cases := map[string]bool{
		"mcp__agentgram__ask_user": true,  // claude naming
		"agentgram_send_photo":     true,  // opencode naming
		"agentgram_ask_user":       true,  // opencode naming
		"Read":                     false, // built-in
		"Bash":                     false,
		"mcp__othersrv__do_thing":  false, // a different MCP server
	}
	for name, want := range cases {
		if got := Internal(name); got != want {
			t.Errorf("Internal(%q) = %v, want %v", name, got, want)
		}
	}
}
