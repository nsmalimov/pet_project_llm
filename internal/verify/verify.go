// Package verify runs real verification (tests) inside a task's worktrees.
// The tester "agent" is deliberately not an LLM: running the project's test
// command directly is cheaper and produces trustworthy evidence.
package verify

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"orchestrator/internal/domain"
)

// DetectCommand returns the test command for a repo directory, or "" if none
// could be detected.
func DetectCommand(dir string) string {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		return "go test ./..."
	}
	if b, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(b, &pkg) == nil && pkg.Scripts["test"] != "" &&
			!strings.Contains(pkg.Scripts["test"], "no test specified") {
			return "npm test --silent"
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "Makefile")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "test:") {
				return "make test"
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "pytest.ini")); err == nil {
		return "python3 -m pytest -q"
	}
	if _, err := os.Stat(filepath.Join(dir, "pyproject.toml")); err == nil {
		if _, err := os.Stat(filepath.Join(dir, "tests")); err == nil {
			return "python3 -m pytest -q"
		}
	}
	return ""
}

// Run executes command (via sh -c) in dir and returns a TestResult.
func Run(ctx context.Context, repoName, dir, command string, timeout time.Duration) domain.TestResult {
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()

	res := domain.TestResult{Repo: repoName, Command: command, Passed: err == nil}
	if exitErr, ok := err.(*exec.ExitError); ok {
		res.ExitCode = exitErr.ExitCode()
	} else if err != nil {
		res.ExitCode = -1
		out.WriteString("\nrunner error: " + err.Error())
	}
	res.OutputTail = tail(out.String(), 4000)
	return res
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
