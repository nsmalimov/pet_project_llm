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

// MaxOutput caps the full output kept in artifacts.
const MaxOutput = 256 * 1024

// Run executes command (via sh -c) in dir and returns a TestResult.
func Run(ctx context.Context, repoName, dir, command string, timeout time.Duration) domain.TestResult {
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	effective := Verbose(command)
	cmd := exec.CommandContext(ctx, "sh", "-c", effective)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()

	res := domain.TestResult{Repo: repoName, Command: command, Passed: err == nil}
	if effective != command {
		res.Effective = effective
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		res.ExitCode = exitErr.ExitCode()
	} else if err != nil {
		res.ExitCode = -1
		out.WriteString("\nrunner error: " + err.Error())
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.Passed = false
		out.WriteString("\nrunner: timed out after " + timeout.String())
	}
	res.OutputTail = tail(out.String(), 4000)
	res.Output = tail(out.String(), MaxOutput)
	res.Tests = ParseTests(effective, out.String())
	res.TestsParsed = res.Tests != nil
	return res
}

// Verbose makes known runners report individual test names so the packet
// can verify that the reproduction test actually executed (exit 0 with the
// test filtered out, skipped or deleted must not count as verification).
func Verbose(command string) string {
	fields := strings.Fields(command)
	if len(fields) >= 2 && fields[0] == "go" && fields[1] == "test" {
		for _, f := range fields[2:] {
			if f == "-v" || f == "-json" {
				return command
			}
		}
		return "go test -v " + strings.Join(fields[2:], " ")
	}
	if isPytest(fields) {
		for _, f := range fields {
			if f == "-v" || f == "-vv" || f == "--verbose" {
				return command
			}
		}
		return command + " -v"
	}
	return command
}

func isPytest(fields []string) bool {
	if len(fields) == 0 {
		return false
	}
	if fields[0] == "pytest" {
		return true
	}
	return len(fields) >= 3 && strings.HasPrefix(fields[0], "python") && fields[1] == "-m" && fields[2] == "pytest"
}

// ParseTests extracts per-test results from go test -v or pytest -v output.
// Returns nil when the runner is unknown (the caller must treat test
// identity as unverifiable).
func ParseTests(command, output string) []domain.TestCase {
	fields := strings.Fields(command)
	var out []domain.TestCase
	switch {
	case len(fields) >= 2 && fields[0] == "go" && fields[1] == "test":
		for _, line := range strings.Split(output, "\n") {
			l := strings.TrimSpace(line)
			var st string
			switch {
			case strings.HasPrefix(l, "--- PASS: "):
				st, l = "pass", strings.TrimPrefix(l, "--- PASS: ")
			case strings.HasPrefix(l, "--- FAIL: "):
				st, l = "fail", strings.TrimPrefix(l, "--- FAIL: ")
			case strings.HasPrefix(l, "--- SKIP: "):
				st, l = "skip", strings.TrimPrefix(l, "--- SKIP: ")
			default:
				continue
			}
			name := strings.Fields(l)
			if len(name) > 0 {
				out = append(out, domain.TestCase{Name: name[0], Status: st})
			}
		}
		if out == nil {
			out = []domain.TestCase{}
		}
		return out
	case isPytest(fields):
		for _, line := range strings.Split(output, "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 && strings.Contains(f[0], "::") {
				switch f[1] {
				case "PASSED":
					out = append(out, domain.TestCase{Name: f[0], Status: "pass"})
				case "FAILED", "ERROR":
					out = append(out, domain.TestCase{Name: f[0], Status: "fail"})
				case "SKIPPED", "XFAIL":
					out = append(out, domain.TestCase{Name: f[0], Status: "skip"})
				}
			}
		}
		if out == nil {
			out = []domain.TestCase{}
		}
		return out
	}
	return nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
