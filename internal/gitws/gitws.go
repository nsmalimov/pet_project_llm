// Package gitws manages task workspaces: for every repository taking part in
// a task it creates an isolated git worktree on a task branch, so agents can
// never damage the user's primary working tree. All repos of one task live
// under a single directory, which becomes the agents' working directory —
// multi-repo tasks get one coherent filesystem view for free.
package gitws

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"orchestrator/internal/domain"
	"orchestrator/internal/sandbox"
)

type Manager struct {
	Root string // e.g. <data-dir>/worktrees
	// MaxWorktreeBytes blocks trees larger than this (0 = unlimited).
	MaxWorktreeBytes int64
}

func NewManager(root string) *Manager { return &Manager{Root: root} }

var gitHomeDir string

// gitHome is an empty HOME so no user-level git config/aliases/hooks apply.
func gitHome() string {
	if gitHomeDir == "" {
		d := filepath.Join(os.TempDir(), "proofline-git-home")
		_ = os.MkdirAll(d, 0o700)
		gitHomeDir = d
	}
	return gitHomeDir
}

// Scan checks every worktree of the task for hostile content. Called after
// checkout and before verification (an agent may have planted a symlink).
func (m *Manager) Scan(t *domain.Task) ([]sandbox.Violation, error) {
	var all []sandbox.Violation
	for _, r := range t.Repos {
		v, err := sandbox.ScanWorktree(filepath.Join(t.State.WorktreeRoot, r.Name), m.MaxWorktreeBytes)
		if err != nil {
			return nil, err
		}
		for i := range v {
			v[i].Path = r.Name + "/" + strings.TrimPrefix(strings.TrimPrefix(v[i].Path, filepath.Join(t.State.WorktreeRoot, r.Name)), "/")
		}
		all = append(all, v...)
	}
	return all, nil
}

// ErrHostileRepo marks repository content that must block the task rather
// than be retried: symlinks escaping the worktree, submodules, nested repos.
var ErrHostileRepo = errors.New("repository content violates the execution policy")

// gitHardening neutralises the ways a repository's own config can run code
// on the host during ordinary git operations.
var gitHardening = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
	"-c", "core.pager=cat",
	"-c", "core.editor=true",
	"-c", "protocol.file.allow=never",
	"-c", "core.sshCommand=/usr/bin/false",
	"-c", "credential.helper=",
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append(append([]string{}, gitHardening...), args...)...)
	cmd.Dir = dir
	// No host environment: no credential helpers, no GIT_* overrides, no
	// global config (HOME points at an empty directory).
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + gitHome(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LANG=C"}
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
		baseRef := "HEAD"
		if t.State.PinnedBase != "" {
			baseRef = t.State.PinnedBase
		}
		sha, err := git(ctx, r.Path, "rev-parse", "--verify", baseRef+"^{commit}")
		if err != nil {
			return fmt.Errorf("repo %s: base %s not found: %w", r.Path, baseRef, err)
		}
		sha = strings.TrimSpace(sha)
		// Persist the base SHA before creating the worktree so a crash in
		// between leaves enough on disk to resume.
		if err := os.WriteFile(baseFile, []byte(sha+"\n"), 0o644); err != nil {
			return err
		}
		if _, err := git(ctx, r.Path, "worktree", "add", "-b", branch, dst, sha); err != nil {
			// Branch may survive from a previous attempt; reuse it.
			if _, err2 := git(ctx, r.Path, "worktree", "add", dst, branch); err2 != nil {
				return err
			}
		}
		t.State.BaseSHAs[r.Name] = sha
	}
	t.State.WorktreeRoot = root
	t.State.Branch = branch
	if v, err := m.Scan(t); err != nil {
		return err
	} else if len(v) > 0 {
		return fmt.Errorf("%w: %s", ErrHostileRepo, describe(v))
	}
	return nil
}

func describe(v []sandbox.Violation) string {
	parts := make([]string, 0, len(v))
	for _, x := range v {
		parts = append(parts, x.Path+": "+x.Reason)
	}
	if len(parts) > 5 {
		parts = append(parts[:5], fmt.Sprintf("+%d more", len(parts)-5))
	}
	return strings.Join(parts, "; ")
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
		d, err := git(ctx, dir, "diff", "--no-ext-diff", "--no-textconv", base)
		if err != nil {
			return "", nil, err
		}
		names, err := git(ctx, dir, "diff", "--no-ext-diff", "--name-only", base)
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

// Fetch brings ref (a SHA or branch) from the repository's origin into the
// local object store so a PR head can be verified. Runs with the hardened
// git environment (no credential helpers, no hooks); a remote that needs
// credentials must be configured on the repository itself.
func (m *Manager) Fetch(ctx context.Context, repoPath, ref string) error {
	args := []string{"-c", "protocol.file.allow=always", "fetch", "--no-tags", "--no-recurse-submodules", "origin", ref}
	if _, err := git(ctx, repoPath, args...); err != nil {
		return fmt.Errorf("fetch %s: %w", ref, err)
	}
	return nil
}

// HasCommit reports whether sha exists in the repository.
func (m *Manager) HasCommit(ctx context.Context, repoPath, sha string) bool {
	_, err := git(ctx, repoPath, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// ApplyHead moves every worktree branch to ref (resolved in the shared
// object store of the original repository). Used by verify-only tasks: the
// baseline ran on the base, now the existing change under review is checked
// out. Returns repo -> sha.
func (m *Manager) ApplyHead(ctx context.Context, t *domain.Task, ref string) (map[string]string, error) {
	out := map[string]string{}
	for _, r := range t.Repos {
		dir := filepath.Join(t.State.WorktreeRoot, r.Name)
		sha, err := git(ctx, dir, "rev-parse", "--verify", ref+"^{commit}")
		if err != nil {
			return nil, fmt.Errorf("repo %s: ref %q not found: %w", r.Name, ref, err)
		}
		sha = strings.TrimSpace(sha)
		if _, err := git(ctx, dir, "reset", "-q", "--hard", sha); err != nil {
			return nil, err
		}
		out[r.Name] = sha
	}
	if v, err := m.Scan(t); err != nil {
		return nil, err
	} else if len(v) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrHostileRepo, describe(v))
	}
	return out, nil
}

// Heads returns the current HEAD per repo worktree and whether any worktree
// has uncommitted changes. This is the "source state" artifacts are bound to.
func (m *Manager) Heads(ctx context.Context, t *domain.Task) (map[string]string, bool, error) {
	heads := map[string]string{}
	dirty := false
	for _, r := range t.Repos {
		dir := filepath.Join(t.State.WorktreeRoot, r.Name)
		sha, err := git(ctx, dir, "rev-parse", "HEAD")
		if err != nil {
			return nil, false, err
		}
		heads[r.Name] = strings.TrimSpace(sha)
		st, err := git(ctx, dir, "status", "--porcelain")
		if err != nil {
			return nil, false, err
		}
		if strings.TrimSpace(st) != "" {
			dirty = true
		}
	}
	return heads, dirty, nil
}

// WithOriginalFiles temporarily restores the base-revision version of the
// given files (repo-relative "repo/path") in the worktrees, runs fn, then
// puts the current versions back. Files that did not exist at base are moved
// aside for the duration. Used to replay the pre-change tests against the
// changed code.
func (m *Manager) WithOriginalFiles(ctx context.Context, t *domain.Task, files []string, fn func() error) (err error) {
	type moved struct{ from, to string }
	var restoreCheckout = map[string][]string{} // repo -> paths checked out from base
	var aside []moved
	defer func() {
		for repo, paths := range restoreCheckout {
			dir := filepath.Join(t.State.WorktreeRoot, repo)
			if _, e := git(ctx, dir, append([]string{"checkout", "HEAD", "--"}, paths...)...); e != nil && err == nil {
				err = fmt.Errorf("restore %s: %w", repo, e)
			}
		}
		for i := len(aside) - 1; i >= 0; i-- {
			if e := os.Rename(aside[i].to, aside[i].from); e != nil && err == nil {
				err = e
			}
		}
	}()
	for _, f := range files {
		repo, rel, ok := strings.Cut(f, "/")
		if !ok {
			continue
		}
		base := t.State.BaseSHAs[repo]
		dir := filepath.Join(t.State.WorktreeRoot, repo)
		if _, e := git(ctx, dir, "cat-file", "-e", base+":"+rel); e == nil {
			if _, e := git(ctx, dir, "checkout", base, "--", rel); e != nil {
				return e
			}
			restoreCheckout[repo] = append(restoreCheckout[repo], rel)
		} else {
			from := filepath.Join(dir, filepath.FromSlash(rel))
			if _, e := os.Stat(from); e != nil {
				continue // deleted by the change; nothing to hide
			}
			to := from + ".orc-aside"
			if e := os.Rename(from, to); e != nil {
				return e
			}
			aside = append(aside, moved{from, to})
		}
	}
	return fn()
}

// RepoDir returns the worktree directory of one repo within the task.
func RepoDir(t *domain.Task, repo domain.RepoRef) string {
	return filepath.Join(t.State.WorktreeRoot, repo.Name)
}
