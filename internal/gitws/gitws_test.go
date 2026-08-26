package gitws

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"orchestrator/internal/domain"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// A hostile repository configures hooks and fsmonitor that would run on
// ordinary git operations (worktree add, status, diff, commit). None of them
// may execute.
func TestMaliciousHooksAndFsmonitorNeverRun(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	os.MkdirAll(repo, 0o755)
	run(t, repo, "init", "-q")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a"), 0o644)
	run(t, repo, "add", "-A")
	run(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "i")
	marker := filepath.Join(tmp, "PWNED")
	hook := "#!/bin/sh\ntouch " + marker + "\n"
	for _, h := range []string{"post-checkout", "pre-commit", "post-commit", "post-index-change", "pre-auto-gc"} {
		os.WriteFile(filepath.Join(repo, ".git", "hooks", h), []byte(hook), 0o755)
	}
	run(t, repo, "config", "core.fsmonitor", "touch "+marker)
	run(t, repo, "config", "core.hooksPath", filepath.Join(repo, ".git", "hooks"))

	m := NewManager(filepath.Join(tmp, "wt"))
	task := &domain.Task{ID: "task_hooks", Repos: []domain.RepoRef{{Name: "repo", Path: repo}}}
	ctx := context.Background()
	if err := m.Prepare(ctx, task); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(task.State.WorktreeRoot, "repo", "b.txt"), []byte("b"), 0o644)
	if _, _, err := m.Diff(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Commit(ctx, task, "x"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Heads(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a repository hook or fsmonitor executed on the host")
	}
}

func TestScanBlocksSymlinkEscapeAfterCheckout(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	os.MkdirAll(repo, 0o755)
	run(t, repo, "init", "-q")
	os.Symlink("/etc", filepath.Join(repo, "etc"))
	run(t, repo, "add", "-A")
	run(t, repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "i")
	m := NewManager(filepath.Join(tmp, "wt"))
	task := &domain.Task{ID: "task_link", Repos: []domain.RepoRef{{Name: "repo", Path: repo}}}
	err := m.Prepare(context.Background(), task)
	if err == nil || !errorsIsHostile(err) {
		t.Fatalf("expected ErrHostileRepo, got %v", err)
	}
}

func errorsIsHostile(err error) bool {
	for e := err; e != nil; {
		if e == ErrHostileRepo {
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}
