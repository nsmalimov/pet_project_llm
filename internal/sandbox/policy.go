// Package sandbox is the execution boundary of Proofline. Everything that
// runs on behalf of a task — test commands, git, LLM agents — goes through
// it. Security here is structural (argv execution, canonical paths, env
// allowlists, process-group kill, OS sandbox where available), never a
// prompt.
//
// Two explicit modes exist because a portable full OS sandbox does not:
//
//   - SAFE_SANDBOX: every capability listed by Capabilities() is enforced by
//     the OS (macOS sandbox-exec today). Refuses to run when it cannot be.
//   - LOCAL_UNSAFE: argv/env/path/process-group/redaction protections apply,
//     but the host filesystem and network are reachable by test commands and
//     agents. Marked on every packet and in the API; must never be described
//     as production-safe.
package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type Mode string

const (
	ModeSafe   Mode = "SAFE_SANDBOX"
	ModeUnsafe Mode = "LOCAL_UNSAFE"
)

// Policy is the immutable execution policy of one Proofline instance.
type Policy struct {
	Mode Mode
	// WorkspaceRoot is the canonical directory under which every task
	// worktree, scratch HOME, cache and tmp lives. Nothing task-related may
	// exist outside it.
	WorkspaceRoot string
	// ReposRoots are canonical directories under which source repositories
	// must live. Empty in LOCAL_UNSAFE means "any canonical git repo".
	ReposRoots []string
	// Runners is the argv[0] allowlist for test/repro commands.
	Runners []string
	// Timeout / output / disk caps.
	Timeout     time.Duration
	MaxOutput   int   // bytes kept per command output (rest is truncated, flagged)
	MaxArtifact int   // bytes for any single persisted artifact field
	MaxDiff     int   // bytes of diff kept
	MaxWorktree int64 // bytes; exceeding it blocks the task
	MaxProcs    int   // RLIMIT_NPROC where supported
}

func DefaultRunners() []string {
	return []string{"go", "npm", "npx", "yarn", "pnpm", "pytest", "python3", "python", "make", "cargo", "mvn", "gradle", "./gradlew", "dotnet"}
}

// Default returns the LOCAL_UNSAFE policy rooted at dataDir. Callers opt in
// to SAFE_SANDBOX explicitly; it fails fast when the OS cannot enforce it.
func Default(dataDir string) (Policy, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return Policy{}, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return Policy{}, err
	}
	canon, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Policy{}, err
	}
	p := Policy{
		Mode: ModeUnsafe, WorkspaceRoot: canon, Runners: DefaultRunners(),
		Timeout: 10 * time.Minute, MaxOutput: 256 * 1024, MaxArtifact: 512 * 1024, MaxDiff: 1 << 20,
		MaxWorktree: 2 << 30, MaxProcs: 256,
	}
	for _, d := range []string{"home", "tmp", "cache"} {
		if err := os.MkdirAll(filepath.Join(canon, d), 0o755); err != nil {
			return Policy{}, err
		}
	}
	return p, nil
}

// WithMode switches the mode, validating that it can be enforced here.
func (p Policy) WithMode(m Mode) (Policy, error) {
	switch m {
	case ModeUnsafe:
		p.Mode = m
		return p, nil
	case ModeSafe:
		if !osSandboxAvailable() {
			return p, fmt.Errorf("SAFE_SANDBOX requested but no OS sandbox is available on %s/%s (need macOS sandbox-exec); refuse to run LOCAL_UNSAFE silently", runtime.GOOS, runtime.GOARCH)
		}
		p.Mode = m
		return p, nil
	}
	return p, fmt.Errorf("unknown sandbox mode %q", m)
}

// Capability names what the current mode actually enforces, so the API and
// packet can state it instead of implying it.
type Capability struct {
	Name     string `json:"name"`
	Enforced bool   `json:"enforced"`
	How      string `json:"how"`
}

func (p Policy) Capabilities() []Capability {
	safe := p.Mode == ModeSafe
	osHow := "not enforced (LOCAL_UNSAFE)"
	if safe {
		osHow = "macOS sandbox-exec profile per command"
	}
	return []Capability{
		{"argv_execution_no_shell", true, "test/repro commands are split into argv and exec'd directly; shell metacharacters rejected"},
		{"runner_allowlist", true, "argv[0] must be one of " + strings.Join(p.Runners, ",")},
		{"canonical_paths_under_workspace", true, "EvalSymlinks + prefix check on every repo, worktree and artifact path"},
		{"worktree_symlink_submodule_scan", true, "symlinks escaping the worktree and .gitmodules block the task"},
		{"git_hooks_disabled", true, "every git call runs with core.hooksPath=/dev/null"},
		{"env_allowlist", true, "commands see a constructed environment; host secrets in env are never inherited"},
		{"process_group_kill", true, "children run in their own process group; timeout/cancel kills the whole group"},
		{"output_and_artifact_caps", true, fmt.Sprintf("%d B output, %d B artifact, %d B diff, %d B worktree", p.MaxOutput, p.MaxArtifact, p.MaxDiff, p.MaxWorktree)},
		{"secret_redaction", true, "pattern-based redaction before any artifact/event/run is persisted"},
		{"filesystem_isolation", safe, osHow + ": reads of ~/.ssh, ~/.aws, ~/.gnupg, ~/.kube, ~/.docker, ~/.netrc, ~/.config denied; writes only under the workspace"},
		{"network_deny_for_commands", safe, osHow + ": test/repro commands get (deny network*)"},
		{"resource_limits", safe && runtime.GOOS != "darwin", "RLIMIT_NPROC/AS via prlimit (Linux only); on macOS only the process-group kill bounds runaway commands"},
	}
}

func osSandboxAvailable() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	_, err := os.Stat("/usr/bin/sandbox-exec")
	return err == nil
}

// Warning is the text every human-facing surface must show in LOCAL_UNSAFE.
func (p Policy) Warning() string {
	if p.Mode == ModeSafe {
		return ""
	}
	return "LOCAL_UNSAFE: test commands and agents can reach the host filesystem and network; not for untrusted repositories"
}
