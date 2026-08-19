// Package llm runs prompts through the locally installed `claude` CLI in
// print mode. No API key or config needed: it reuses the user's existing
// Claude Code authentication.
package llm

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Complete sends prompt to `claude -p` and returns the text response.
func Complete(ctx context.Context, model, prompt string) (string, error) {
	if _, err := exec.LookPath("claude"); err != nil {
		return "", fmt.Errorf("`claude` CLI not found in PATH — hindsight uses it as its LLM backend (install Claude Code, or see TODO.md for direct API support)")
	}
	args := []string{"-p", "--model", model, "--settings", `{"disableAllHooks": true}`}
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Stdin = strings.NewReader(prompt)
	// Avoid nested-session interference when hindsight itself runs inside Claude Code.
	cmd.Env = append(os.Environ(), "CLAUDE_CODE_ENTRYPOINT=hindsight")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude -p failed: %w\n%s", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}
