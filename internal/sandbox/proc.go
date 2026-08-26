package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Proc is a long-running sandboxed process (an integration service under
// test). It runs in its own process group; Stop kills the whole group.
type Proc struct {
	cmd  *exec.Cmd
	out  *cappedBuffer
	done chan error
}

// Start launches spec without waiting. Spec.LocalNetwork lets the process
// bind and accept on localhost under SAFE_SANDBOX (never outbound).
func (p Policy) Start(ctx context.Context, spec Spec) (*Proc, error) {
	dir, err := CanonicalUnder(p.WorkspaceRoot, spec.Dir)
	if err != nil {
		return nil, fmt.Errorf("working directory: %w", err)
	}
	argv := spec.Argv
	if p.Mode == ModeSafe {
		prof, err := p.sbplProfile(dir, spec)
		if err != nil {
			return nil, err
		}
		argv = append([]string{"/usr/bin/sandbox-exec", "-p", prof}, argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = p.Env(spec.ExtraEnv)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out := &cappedBuffer{max: p.MaxOutput}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Stdin = strings.NewReader("")
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pr := &Proc{cmd: cmd, out: out, done: make(chan error, 1)}
	go func() { pr.done <- cmd.Wait() }()
	return pr, nil
}

// Exited reports whether the process already terminated.
func (pr *Proc) Exited() bool {
	select {
	case err := <-pr.done:
		pr.done <- err
		return true
	default:
		return false
	}
}

// Stop kills the process group and returns the captured (redacted) output.
func (pr *Proc) Stop() (string, int) {
	killGroup(pr.cmd.Process.Pid)
	select {
	case <-pr.done:
	case <-time.After(5 * time.Second):
	}
	killGroup(pr.cmd.Process.Pid)
	text, n := Redact(pr.out.String())
	return text, n
}
