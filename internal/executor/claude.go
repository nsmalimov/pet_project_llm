package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ClaudeCLI runs agents through the Claude Code CLI in non-interactive mode
// (`claude -p --output-format json`). The prompt is passed on stdin, the JSON
// envelope gives us cost/usage/turns for observability.
type ClaudeCLI struct {
	Bin string // defaults to "claude"
}

func NewClaudeCLI() *ClaudeCLI { return &ClaudeCLI{Bin: "claude"} }

func (c *ClaudeCLI) Name() string { return "claude" }

// claudeEnvelope is the subset of `claude -p --output-format json` we consume.
type claudeEnvelope struct {
	Type       string  `json:"type"`
	Subtype    string  `json:"subtype"`
	IsError    bool    `json:"is_error"`
	Result     string  `json:"result"`
	SessionID  string  `json:"session_id"`
	NumTurns   int     `json:"num_turns"`
	DurationMS int64   `json:"duration_ms"`
	TotalCost  float64 `json:"total_cost_usd"`
	Usage      struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

func (c *ClaudeCLI) Run(ctx context.Context, req Request) (Result, error) {
	if req.WorkDir == "" {
		return Result{}, fmt.Errorf("claude cli: empty WorkDir")
	}
	bin := c.Bin
	if bin == "" {
		bin = "claude"
	}
	timeout := req.Timeout
	if timeout == 0 {
		timeout = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"-p", "--output-format", "json"}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.ReadOnly {
		// Read-only roles get search/read tools plus non-mutating git.
		args = append(args,
			"--allowedTools", "Read Glob Grep Bash(git diff:*) Bash(git log:*) Bash(git show:*) Bash(git status:*)",
			"--disallowedTools", "Edit Write NotebookEdit",
		)
	} else {
		// Writing roles: auto-accept edits, allow bash for builds/tests.
		// The blast radius is limited by running inside isolated worktrees.
		args = append(args,
			"--permission-mode", "acceptEdits",
			"--allowedTools", "Read Edit Write Glob Grep Bash",
		)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = req.WorkDir
	cmd.Stdin = strings.NewReader(req.Prompt)
	// Drop nesting markers so the child CLI behaves like a fresh session.
	env := os.Environ()
	filtered := env[:0]
	for _, e := range env {
		if strings.HasPrefix(e, "CLAUDECODE=") || strings.HasPrefix(e, "CLAUDE_CODE_ENTRYPOINT=") {
			continue
		}
		filtered = append(filtered, e)
	}
	cmd.Env = filtered

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start).Milliseconds()

	var env2 claudeEnvelope
	if jsonErr := json.Unmarshal(stdout.Bytes(), &env2); jsonErr != nil {
		if runErr != nil {
			return Result{}, fmt.Errorf("claude cli: %w (stderr: %s)", runErr, tail(stderr.String(), 2000))
		}
		return Result{}, fmt.Errorf("claude cli: cannot parse output envelope: %v (stdout: %s)", jsonErr, tail(stdout.String(), 2000))
	}
	res := Result{
		Output:       env2.Result,
		InputTokens:  env2.Usage.InputTokens + env2.Usage.CacheReadInputTokens + env2.Usage.CacheCreationInputTokens,
		OutputTokens: env2.Usage.OutputTokens,
		NumTurns:     env2.NumTurns,
		CostUSD:      env2.TotalCost,
		DurationMS:   env2.DurationMS,
		SessionID:    env2.SessionID,
	}
	if res.DurationMS == 0 {
		res.DurationMS = elapsed
	}
	if env2.IsError {
		return res, fmt.Errorf("claude cli reported error (%s): %s", env2.Subtype, tail(env2.Result, 2000))
	}
	if runErr != nil {
		return res, fmt.Errorf("claude cli: %w (stderr: %s)", runErr, tail(stderr.String(), 2000))
	}
	return res, nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
