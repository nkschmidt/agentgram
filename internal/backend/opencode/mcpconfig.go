package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// configFile — project-level opencode config opencode reads from the working
// directory (the `directory` we pass on every request). It's the only
// per-directory hook opencode exposes, so it's how we give each user their own
// MCP server entry (with their own auth token) on a single shared serve process.
const configFile = "opencode.json"

// writeMCPConfig merges the bot's MCP server entry into the project-level
// opencode.json in dir. `full` is a complete opencode.json document produced by
// the mcp package; we take its `mcp` map and merge it in, preserving any other
// keys the user already has. Only the `mcp.<bot server>` entry is touched.
//
// If an existing opencode.json can't be parsed as plain JSON (e.g. it's jsonc
// with comments), we leave it untouched and return an error instead of risking
// clobbering the user's own config.
func writeMCPConfig(dir string, full []byte) error {
	if dir == "" || len(full) == 0 {
		return nil
	}
	var incoming map[string]any
	if err := json.Unmarshal(full, &incoming); err != nil {
		return fmt.Errorf("opencode mcp config: parse generated: %w", err)
	}
	incomingMCP, _ := incoming["mcp"].(map[string]any)
	if len(incomingMCP) == 0 {
		return nil
	}

	path := filepath.Join(dir, configFile)
	doc := map[string]any{}
	if existing, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(existing, &doc); err != nil {
			return fmt.Errorf("opencode mcp config: existing %s is not plain JSON, left untouched: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("opencode mcp config: read %s: %w", path, err)
	}

	if _, ok := doc["$schema"]; !ok {
		if sch, ok := incoming["$schema"]; ok {
			doc["$schema"] = sch
		}
	}
	mcp, _ := doc["mcp"].(map[string]any)
	if mcp == nil {
		mcp = map[string]any{}
	}
	for k, v := range incomingMCP {
		mcp[k] = v
	}
	doc["mcp"] = mcp

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("opencode mcp config: marshal: %w", err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("opencode mcp config: write %s: %w", path, err)
	}
	return nil
}
