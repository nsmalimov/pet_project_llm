package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ScriptExecutor replays pre-recorded agent responses. It exists so the whole
// workflow can be exercised end-to-end (including real worktrees, diffs and
// test runs) without any LLM calls — in unit tests and offline demos.
//
// A scenario maps "<role>" or "<role>:<attempt>" to a step. The attempt-
// qualified key wins. A step can also write files into the worktree before
// returning its output, which is how a scripted "developer" produces a diff.
type ScriptExecutor struct {
	Steps map[string]ScriptStep `json:"steps"`
}

type ScriptStep struct {
	// Output is the agent's final message. Must contain the role's JSON block.
	Output string `json:"output"`
	// Files are written relative to the task worktree root before returning.
	Files []ScriptFile `json:"files,omitempty"`
	// Fail simulates an executor failure.
	Fail string `json:"fail,omitempty"`
}

type ScriptFile struct {
	Path    string `json:"path"` // relative to worktree root, e.g. "repoA/pkg/x.go"
	Content string `json:"content"`
}

func LoadScript(path string) (*ScriptExecutor, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s ScriptExecutor
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse scenario %s: %w", path, err)
	}
	return &s, nil
}

func (s *ScriptExecutor) Name() string { return "mock" }

func (s *ScriptExecutor) Run(_ context.Context, req Request) (Result, error) {
	if req.WorkDir == "" {
		return Result{}, fmt.Errorf("mock executor: empty WorkDir")
	}
	step, ok := s.Steps[fmt.Sprintf("%s:%d", req.Role, req.Attempt)]
	if !ok {
		step, ok = s.Steps[req.Role]
	}
	if !ok {
		return Result{}, fmt.Errorf("mock executor: no scripted step for role %q (attempt %d)", req.Role, req.Attempt)
	}
	// Files are written before a scripted failure so a "crash after editing"
	// can be simulated (the realistic shape of a dead agent process).
	for _, f := range step.Files {
		if req.ReadOnly {
			return Result{}, fmt.Errorf("mock executor: scenario writes files but role %s is read-only", req.Role)
		}
		dst := filepath.Join(req.WorkDir, f.Path)
		if rel, err := filepath.Rel(req.WorkDir, dst); err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			return Result{}, fmt.Errorf("mock executor: scenario path %q escapes the worktree", f.Path)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return Result{}, err
		}
		if err := os.WriteFile(dst, []byte(f.Content), 0o644); err != nil {
			return Result{}, err
		}
	}
	if step.Fail != "" {
		return Result{}, fmt.Errorf("mock executor: scripted failure: %s", step.Fail)
	}
	return Result{Output: step.Output, NumTurns: 1, DurationMS: 1}, nil
}
