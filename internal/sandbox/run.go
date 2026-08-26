package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// Spec describes one command execution inside the boundary.
type Spec struct {
	Dir     string        // must be canonical and under WorkspaceRoot
	Argv    []string      // already validated
	Timeout time.Duration // 0 → policy default
	// Network allows outbound network (only LLM agents need it).
	Network bool
	// ExtraEnv is appended to the constructed environment (e.g. agent auth).
	ExtraEnv []string
	// ReadRoots are extra canonical directories readable in SAFE mode
	// besides the workspace and system paths.
	ReadRoots []string
	// Profile selects the OS sandbox flavour: "command" (default) or "agent".
	Profile string
	// LocalNetwork allows binding/accepting on localhost only (integration
	// services under test). Outbound stays denied.
	LocalNetwork bool
}

// Result is the outcome; Output is capped, Truncated says so.
type Result struct {
	ExitCode  int
	Output    string
	Truncated bool
	TimedOut  bool
	Killed    bool // process group had to be killed (timeout/cancel)
	Redacted  int
	Duration  time.Duration
	Command   []string // what actually ran (sandbox wrapper included)
	Mode      Mode
	Err       error // runner-level error (not a non-zero exit)
}

// Env builds the constructed environment: nothing from the host except PATH
// (and, for agents, the explicit auth variables the caller passes).
func (p Policy) Env(extra []string) []string {
	cache := filepath.Join(p.WorkspaceRoot, "cache")
	if shared := os.Getenv("PROOFLINE_SHARED_CACHE"); shared != "" {
		// Test suites reuse one build cache across many workspaces; the
		// cache holds compiled objects only, never task state.
		cache = shared
	}
	env := []string{
		"PATH=" + hostPath(),
		"HOME=" + filepath.Join(p.WorkspaceRoot, "home"),
		"TMPDIR=" + filepath.Join(p.WorkspaceRoot, "tmp"),
		"GOCACHE=" + filepath.Join(cache, "go-build"),
		"GOMODCACHE=" + filepath.Join(cache, "gomod"),
		"GOPATH=" + filepath.Join(cache, "gopath"),
		"GOFLAGS=-mod=mod",
		"GOTOOLCHAIN=local",
		"npm_config_cache=" + filepath.Join(cache, "npm"),
		"PIP_CACHE_DIR=" + filepath.Join(cache, "pip"),
		"LANG=C.UTF-8", "LC_ALL=C.UTF-8", "TERM=dumb", "CI=true",
		"PROOFLINE_SANDBOX=" + string(p.Mode),
	}
	if p.Mode == ModeSafe {
		env = append(env, "GOPROXY=off", "GONOSUMDB=*", "GOFLAGS=-mod=mod")
	}
	return append(env, extra...)
}

func hostPath() string {
	if v := os.Getenv("PATH"); v != "" {
		return v
	}
	return "/usr/local/bin:/usr/bin:/bin"
}

// Run executes spec. Never uses a shell. The child gets its own process
// group; on timeout or context cancel the whole group is killed and the
// result says so. Output is captured up to MaxOutput bytes and redacted.
func (p Policy) Run(ctx context.Context, spec Spec) Result {
	res := Result{Mode: p.Mode}
	if len(spec.Argv) == 0 {
		res.Err = fmt.Errorf("empty argv")
		res.ExitCode = -1
		return res
	}
	dir, err := CanonicalUnder(p.WorkspaceRoot, spec.Dir)
	if err != nil {
		res.Err = fmt.Errorf("working directory: %w", err)
		res.ExitCode = -1
		return res
	}
	timeout := spec.Timeout
	if timeout == 0 {
		timeout = p.Timeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	argv := spec.Argv
	if p.Mode == ModeSafe {
		prof, err := p.sbplProfile(dir, spec)
		if err != nil {
			res.Err = err
			res.ExitCode = -1
			return res
		}
		argv = append([]string{"/usr/bin/sandbox-exec", "-p", prof}, argv...)
	}
	res.Command = argv

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = p.Env(spec.ExtraEnv)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out := &cappedBuffer{max: p.MaxOutput}
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Stdin = strings.NewReader("")

	start := time.Now()
	if err := cmd.Start(); err != nil {
		res.Err = err
		res.ExitCode = -1
		res.Output = "runner error: " + err.Error()
		return res
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		killGroup(cmd.Process.Pid)
		res.Killed = true
		res.TimedOut = ctx.Err() == context.DeadlineExceeded
		select {
		case waitErr = <-done:
		case <-time.After(5 * time.Second):
			waitErr = fmt.Errorf("process group %d did not exit after SIGKILL", cmd.Process.Pid)
		}
	}
	// Belt and braces: kill anything left in the group (background children
	// that outlived the parent), so no test process survives verification.
	killGroup(cmd.Process.Pid)
	res.Duration = time.Since(start)

	if exitErr, ok := waitErr.(*exec.ExitError); ok {
		res.ExitCode = exitErr.ExitCode()
		if res.ExitCode == -1 { // killed by signal
			res.ExitCode = 137
		}
	} else if waitErr != nil {
		res.ExitCode = -1
		res.Err = waitErr
	}
	text := out.String()
	if res.TimedOut {
		text += "\nrunner: timed out after " + timeout.String() + "; process group killed"
	} else if res.Killed {
		text += "\nrunner: cancelled; process group killed"
	}
	if res.Err != nil {
		text += "\nrunner error: " + res.Err.Error()
	}
	res.Output, res.Redacted = Redact(text)
	res.Truncated = out.truncated
	return res
}

func killGroup(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// cappedBuffer keeps the head of the output up to max bytes and records
// truncation. Head (not tail) so the command line and first failure survive;
// the tail is kept in a small ring for the "last lines" the reader wants.
type cappedBuffer struct {
	head      bytes.Buffer
	tail      []byte
	max       int
	dropped   int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	room := b.max - b.head.Len()
	if room > 0 {
		if len(p) <= room {
			b.head.Write(p)
			return n, nil
		}
		b.head.Write(p[:room])
		p = p[room:]
	}
	b.truncated = true
	b.dropped += len(p)
	tailMax := 8 * 1024
	b.tail = append(b.tail, p...)
	if len(b.tail) > tailMax {
		b.tail = b.tail[len(b.tail)-tailMax:]
	}
	return n, nil
}

func (b *cappedBuffer) String() string {
	if !b.truncated {
		return b.head.String()
	}
	return b.head.String() + fmt.Sprintf("\n…[output truncated: %d bytes dropped]…\n", b.dropped-len(b.tail)) + string(b.tail)
}

// AgentProfile is the OS sandbox profile for an LLM CLI working in dir:
// network allowed, host secret locations denied, writes confined to the
// workspace and the CLI's own state directory.
func (p Policy) AgentProfile(dir string) (string, error) {
	return p.sbplProfile(dir, Spec{Profile: "agent"})
}

// sbplProfile builds the macOS sandbox profile for one execution.
//   - command profile: deny network, read system + workspace + explicit
//     roots, write only under the workspace.
//   - agent profile: network allowed (LLM API), same filesystem rules plus
//     the agent's own config dir; host secret locations always denied.
func (p Policy) sbplProfile(dir string, spec Spec) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("SAFE_SANDBOX profile only implemented for darwin")
	}
	home, _ := os.UserHomeDir()
	var sb strings.Builder
	sb.WriteString("(version 1)\n(deny default)\n")
	sb.WriteString("(allow process-exec*)\n(allow process-fork)\n(allow signal (target same-sandbox))\n")
	sb.WriteString("(allow sysctl-read)\n(allow mach-lookup)\n(allow ipc-posix*)\n(allow system-socket)\n")
	sb.WriteString("(allow file-read-metadata)\n")
	// System paths readable.
	for _, r := range []string{"/usr", "/bin", "/sbin", "/opt", "/Library", "/System", "/private/etc", "/etc", "/dev", "/private/var/db", "/var/db", "/Applications"} {
		fmt.Fprintf(&sb, "(allow file-read* (subpath %q))\n", r)
	}
	sb.WriteString("(allow file-read* (literal \"/\"))\n(allow file-write-data (literal \"/dev/null\"))\n(allow file-read* file-write* (subpath \"/dev\"))\n")
	// Toolchains in the user's home (go, node via brew/nvm, claude CLI).
	for _, r := range []string{"go", "sdk", ".nvm", ".npm", ".local", ".cache", ".claude", ".claude.json", "Library/Caches", "Library/Application Support/Claude", ".proofline"} {
		fmt.Fprintf(&sb, "(allow file-read* (subpath %q))\n", filepath.Join(home, r))
	}
	if goroot := os.Getenv("GOROOT"); goroot != "" {
		fmt.Fprintf(&sb, "(allow file-read* (subpath %q))\n", goroot)
	}
	// Workspace: full read/write.
	fmt.Fprintf(&sb, "(allow file-read* file-write* (subpath %q))\n", p.WorkspaceRoot)
	for _, r := range spec.ReadRoots {
		fmt.Fprintf(&sb, "(allow file-read* (subpath %q))\n", r)
	}
	// Temp for the toolchain itself.
	sb.WriteString("(allow file-read* file-write* (subpath \"/private/tmp\") (subpath \"/private/var/folders\") (subpath \"/tmp\"))\n")
	if spec.Profile == "agent" {
		// The LLM CLI needs its config/state, the login keychain and network.
		fmt.Fprintf(&sb, "(allow file-read* file-write* (subpath %q) (literal %q) (literal %q) (subpath %q) (subpath %q) (subpath %q))\n",
			filepath.Join(home, ".claude"), filepath.Join(home, ".claude.json"), filepath.Join(home, ".claude.json.backup"),
			filepath.Join(home, ".npm"), filepath.Join(home, "Library", "Caches", "claude-cli-nodejs"), filepath.Join(home, "Library", "Application Support", "Claude"))
		fmt.Fprintf(&sb, "(allow file-read* (subpath %q) (subpath %q) (subpath %q))\n",
			filepath.Join(home, "Library", "Keychains"), "/Library/Keychains", filepath.Join(home, "Library", "Preferences"))
		sb.WriteString("(allow file-write* (subpath \"" + filepath.Join(home, "Library", "Keychains") + "\"))\n")
		sb.WriteString("(allow network*)\n")
	} else if spec.Network {
		sb.WriteString("(allow network*)\n")
	} else if spec.LocalNetwork {
		sb.WriteString("(deny network*)\n(allow network-bind (local ip \"localhost:*\"))\n(allow network-inbound (local ip \"localhost:*\"))\n(allow network-outbound (remote ip \"localhost:*\"))\n")
	} else {
		sb.WriteString("(deny network*)\n")
	}
	// Host secret locations: denied last so they override every allow.
	// ".proofline-probe" is a decoy used by the boundary self-test so the
	// mechanism can be verified without touching real credential paths.
	for _, r := range []string{".proofline-probe", ".ssh", ".aws", ".gnupg", ".kube", ".docker", ".config/gh", ".netrc", ".git-credentials", ".azure", ".gcloud", ".config/gcloud", ".zsh_history", ".bash_history", ".env"} {
		fmt.Fprintf(&sb, "(deny file-read* file-write* (subpath %q) (literal %q))\n", filepath.Join(home, r), filepath.Join(home, r))
	}
	return sb.String(), nil
}
