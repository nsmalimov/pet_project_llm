package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"orchestrator/internal/sandbox"
)

// ClaudeCLI runs agents through the Claude Code CLI in non-interactive mode
// (`claude -p --output-format json`). The prompt is passed on stdin, the JSON
// envelope gives us cost/usage/turns for observability.
type ClaudeCLI struct {
	Bin string // defaults to "claude"
	// Policy, when set, provides the execution boundary: constructed
	// environment (no host secrets) and, in SAFE_SANDBOX, an OS sandbox
	// around the CLI that denies host secret locations and confines writes
	// to the workspace while keeping network for the model API.
	Policy *sandbox.Policy
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
		// Read-only roles get search/read tools, non-mutating git, and the
		// common test runners so an independent reviewer can actually
		// execute the suite instead of hand-tracing it. No Edit/Write.
		args = append(args,
			"--allowedTools", "Read Glob Grep Bash(git diff:*) Bash(git log:*) Bash(git show:*) Bash(git status:*) Bash(git rev-parse:*) Bash(git ls-files:*) Bash(git branch:*) Bash(git blame:*) Bash(go test:*) Bash(go vet:*) Bash(npm test:*) Bash(pytest:*) Bash(python3 -m pytest:*) Bash(make test:*)",
			"--disallowedTools", "Edit Write NotebookEdit",
		)
	} else {
		// Writing role: edits auto-accepted inside the worktree; Bash limited
		// to build/test runners and read-only git. No blanket shell. The
		// filesystem boundary itself is the OS sandbox (SAFE_SANDBOX), not
		// this list.
		args = append(args,
			"--permission-mode", "acceptEdits",
			// Build/test verbs only: no `go run`, `go generate`, `go tool`,
			// `npm run <script>`, arbitrary make targets or python code.
			"--allowedTools", "Read Edit Write Glob Grep "+
				"Bash(go test:*) Bash(go build:*) Bash(go vet:*) Bash(go mod tidy:*) Bash(go mod download:*) Bash(gofmt:*) "+
				"Bash(npm test:*) Bash(npm ci:*) Bash(npm install:*) Bash(npm run build:*) Bash(npm run lint:*) Bash(npx jest:*) Bash(npx vitest:*) Bash(npx tsc:*) Bash(npx eslint:*) "+
				"Bash(python3 -m pytest:*) Bash(pytest:*) Bash(make test:*) Bash(make build:*) Bash(cargo test:*) Bash(cargo build:*) Bash(cargo check:*) "+
				"Bash(git status:*) Bash(git diff:*) Bash(git log:*) Bash(git show:*) Bash(git add:*) Bash(git rev-parse:*) Bash(git ls-files:*) Bash(git blame:*) Bash(ls:*)",
			"--disallowedTools", "WebFetch WebSearch",
		)
	}

	argv := append([]string{bin}, args...)
	if c.Policy != nil && c.Policy.Mode == sandbox.ModeSafe {
		prof, err := c.Policy.AgentProfile(req.WorkDir)
		if err != nil {
			return Result{}, err
		}
		argv = append([]string{"/usr/bin/sandbox-exec", "-p", prof}, argv...)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = req.WorkDir
	cmd.Stdin = strings.NewReader(req.Prompt)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = agentEnv()
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

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

// agentEnv is the allowlisted environment for the CLI: PATH, HOME (the CLI
// keeps its auth/config there), locale/terminal, and only the variables the
// CLI itself documents. Cloud credentials, tokens and everything else on the
// host are never inherited. Nesting markers are dropped so the child CLI
// behaves like a fresh session.
func agentEnv() []string {
	allow := []string{"PATH", "HOME", "USER", "LANG", "LC_ALL", "TERM", "TMPDIR", "SHELL", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "NODE_OPTIONS", "NO_COLOR"}
	prefixes := []string{"ANTHROPIC_", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_USE_", "CLAUDE_CODE_MAX_", "CLAUDE_CODE_DISABLE_", "DISABLE_", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY"}
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		ok := false
		for _, a := range allow {
			if k == a {
				ok = true
			}
		}
		for _, p := range prefixes {
			if strings.HasPrefix(k, p) {
				ok = true
			}
		}
		if ok {
			out = append(out, kv)
		}
	}
	return append(out, "CI=true", "PROOFLINE_AGENT=1")
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
