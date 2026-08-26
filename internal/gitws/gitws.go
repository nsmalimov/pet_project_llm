// Package gitws manages task workspaces: for every repository taking part in
// a task it creates an isolated git worktree on a task branch, so agents can
// never damage the user's primary working tree. All repos of one task live
// under a single directory, which becomes the agents' working directory —
// multi-repo tasks get one coherent filesystem view for free.
package gitws

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"orchestrator/internal/domain"
)

type Manager struct {
	Root string // e.g. <data-dir>/worktrees
}

func NewManager(root string) *Manager { return &Manager{Root: root} }

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("git %s (in %s): %w\n%s", strings.Join(args, " "), dir, err, out.String())
	}
	return out.String(), nil
}

// Prepare creates worktrees for every repo of the task on branch orc/<task-id>
// and records base SHAs into the task state. Idempotent: an existing valid
// worktree is reused (needed for resume after restart).
func (m *Manager) Prepare(ctx context.Context, t *domain.Task) error {
	root := filepath.Join(m.Root, t.ID)
	branch := "orc/" + t.ID
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	if t.State.BaseSHAs == nil {
		t.State.BaseSHAs = map[string]string{}
	}
	for _, r := range t.Repos {
		dst := filepath.Join(root, r.Name)
		baseFile := filepath.Join(root, r.Name+".base")
		if _, err := os.Stat(filepath.Join(dst, ".git")); err == nil {
			// Worktree already exists (resume). Recover the base SHA from the
			// sidecar file if the crash happened before the task was saved.
			if t.State.BaseSHAs[r.Name] == "" {
				b, err := os.ReadFile(baseFile)
				if err != nil {
					return fmt.Errorf("worktree for %s exists but its base SHA is unknown: %w", r.Name, err)
				}
				t.State.BaseSHAs[r.Name] = strings.TrimSpace(string(b))
			}
			continue
		}
		sha, err := git(ctx, r.Path, "rev-parse", "HEAD")
		if err != nil {
			return fmt.Errorf("repo %s has no commits or is not a git repo: %w", r.Path, err)
		}
		sha = strings.TrimSpace(sha)
		// Persist the base SHA before creating the worktree so a crash in
		// between leaves enough on disk to resume.
		if err := os.WriteFile(baseFile, []byte(sha+"\n"), 0o644); err != nil {
			return err
		}
		if _, err := git(ctx, r.Path, "worktree", "add", "-b", branch, dst, "HEAD"); err != nil {
			// Branch may survive from a previous attempt; reuse it.
			if _, err2 := git(ctx, r.Path, "worktree", "add", dst, branch); err2 != nil {
				return err
			}
		}
		t.State.BaseSHAs[r.Name] = sha
	}
	t.State.WorktreeRoot = root
	t.State.Branch = branch
	return nil
}

// Diff returns the combined diff (committed + uncommitted, including new
// files) of every repo against its recorded base SHA. Also returns the list
// of changed files as "repo/path".
func (m *Manager) Diff(ctx context.Context, t *domain.Task) (diff string, files []string, err error) {
	var sb strings.Builder
	for _, r := range t.Repos {
		dir := filepath.Join(t.State.WorktreeRoot, r.Name)
		base := t.State.BaseSHAs[r.Name]
		if base == "" {
			return "", nil, fmt.Errorf("no base SHA recorded for repo %s", r.Name)
		}
		// Track untracked files so they appear in the diff.
		if _, err := git(ctx, dir, "add", "-A", "-N"); err != nil {
			return "", nil, err
		}
		d, err := git(ctx, dir, "diff", base)
		if err != nil {
			return "", nil, err
		}
		names, err := git(ctx, dir, "diff", "--name-only", base)
		if err != nil {
			return "", nil, err
		}
		// Undo the intent-to-add entries: reading a diff must not leave the
		// index modified for agents/tests that inspect git state.
		if _, err := git(ctx, dir, "reset", "-q"); err != nil {
			return "", nil, err
		}
		if strings.TrimSpace(d) != "" {
			sb.WriteString(fmt.Sprintf("### repo: %s\n%s\n", r.Name, d))
		}
		for _, f := range strings.Split(strings.TrimSpace(names), "\n") {
			if f != "" {
				files = append(files, r.Name+"/"+f)
			}
		}
	}
	return sb.String(), files, nil
}

// Commit stages and commits every change in each worktree that has one, so
// the packet can reference a concrete SHA. Returns repo name -> sha for the
// repos that received a commit. Repos without changes are skipped.
func (m *Manager) Commit(ctx context.Context, t *domain.Task, message string) (map[string]string, error) {
	out := map[string]string{}
	for _, r := range t.Repos {
		dir := filepath.Join(t.State.WorktreeRoot, r.Name)
		if _, err := git(ctx, dir, "add", "-A"); err != nil {
			return out, err
		}
		status, err := git(ctx, dir, "status", "--porcelain")
		if err != nil {
			return out, err
		}
		if strings.TrimSpace(status) == "" {
			continue
		}
		if _, err := git(ctx, dir, "-c", "user.name=orc", "-c", "user.email=orc@localhost",
			"commit", "-q", "--no-verify", "-m", message); err != nil {
			return out, err
		}
		sha, err := git(ctx, dir, "rev-parse", "HEAD")
		if err != nil {
			return out, err
		}
		out[r.Name] = strings.TrimSpace(sha)
	}
	return out, nil
}

// RepoDir returns the worktree directory of one repo within the task.
func RepoDir(t *domain.Task, repo domain.RepoRef) string {
	return filepath.Join(t.State.WorktreeRoot, repo.Name)
}
