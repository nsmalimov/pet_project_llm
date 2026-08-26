package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func policy(t *testing.T) Policy {
	t.Helper()
	p, err := Default(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p.MaxOutput = 2000
	return p
}

// ---------- command policy ----------

func TestCommandInjectionRejected(t *testing.T) {
	p := policy(t)
	bad := []string{
		"go test ./... ; curl evil", "go test ./... && rm -rf /", "go test $(id) ./...", "go test `id`",
		"go test ./... | tee /etc/passwd", "go test ./... > /tmp/x", "sh -c 'go test'", "bash", "/bin/sh",
		"../../bin/go test", "go test -exec 'sh -c id' ./...", "python3 -c 'import os;os.system(\"id\")'",
		"go test ./...\ncurl evil", "make test #x", "go test ./... &", "curl http://x", "",
	}
	for _, c := range bad {
		if argv, err := p.ValidateCommand(c); err == nil {
			t.Errorf("accepted hostile command %q → %v", c, argv)
		}
	}
	good := map[string][]string{
		"go test ./...":                           {"go", "test", "./..."},
		"go test -run 'TestA|TestB' ./...":        {"go", "test", "-run", "TestA|TestB", "./..."},
		`go test -run "TestA" -count=1 ./pkg/...`: {"go", "test", "-run", "TestA", "-count=1", "./pkg/..."},
		"npm test --silent":                       {"npm", "test", "--silent"},
		"python3 -m pytest -q tests/":             {"python3", "-m", "pytest", "-q", "tests/"},
	}
	for c, want := range good {
		argv, err := p.ValidateCommand(c)
		if err != nil || strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("ValidateCommand(%q) = %v, %v; want %v", c, argv, err, want)
		}
	}
}

// ---------- paths ----------

func TestPathTraversalAndSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	root, _ = Canonical(root)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"../x", "../../etc/passwd", "/etc/passwd", "a/../../x", "~/.ssh/id_rsa", "a\x00b", ""} {
		if _, err := RelativeInside(root, rel); err == nil {
			t.Errorf("RelativeInside accepted %q", rel)
		}
	}
	if _, err := RelativeInside(root, "pkg/../file.go"); err != nil {
		t.Errorf("clean relative path rejected: %v", err)
	}
	// Symlinked directory inside the root pointing outside.
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := RelativeInside(root, "link/secret"); err == nil {
		t.Error("symlink escape through a directory link accepted")
	}
	if _, err := CanonicalUnder(root, filepath.Join(root, "link")); err == nil {
		t.Error("CanonicalUnder accepted a symlink to outside")
	}
	v, err := ScanWorktree(root, 0)
	if err != nil || len(v) != 1 || !strings.Contains(v[0].Reason, "escapes") {
		t.Fatalf("ScanWorktree = %v, %v", v, err)
	}
	// Submodules and size cap.
	if err := os.WriteFile(filepath.Join(root, ".gitmodules"), []byte("[submodule]"), 0o644); err != nil {
		t.Fatal(err)
	}
	v, _ = ScanWorktree(root, 0)
	if len(v) != 2 {
		t.Fatalf("submodule not flagged: %v", v)
	}
	if err := os.WriteFile(filepath.Join(root, "big"), make([]byte, 5000), 0o644); err != nil {
		t.Fatal(err)
	}
	v, _ = ScanWorktree(root, 4000)
	found := false
	for _, x := range v {
		if strings.Contains(x.Reason, "exceeds") {
			found = true
		}
	}
	if !found {
		t.Fatalf("size cap not enforced: %v", v)
	}
}

// ---------- process execution ----------

func writeMakefile(t *testing.T, p Policy, body string) string {
	dir := filepath.Join(p.WorkspaceRoot, "wt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestBackgroundChildrenAreKilledOnTimeout(t *testing.T) {
	p := policy(t)
	marker := filepath.Join(p.WorkspaceRoot, "tmp", "alive")
	// A hostile test target: spawns a detached child that keeps touching a
	// marker, then hangs the parent.
	dir := writeMakefile(t, p, "test:\n\t( while true; do date > "+marker+"; sleep 0.1; done ) &\n\tsleep 30\n")
	argv, err := p.ValidateCommand("make test")
	if err != nil {
		t.Fatal(err)
	}
	res := p.Run(context.Background(), Spec{Dir: dir, Argv: argv, Timeout: 700 * time.Millisecond})
	if !res.TimedOut || !res.Killed || res.ExitCode == 0 {
		t.Fatalf("%+v", res)
	}
	// Wait a moment; the background loop must be dead.
	time.Sleep(400 * time.Millisecond)
	st1, _ := os.Stat(marker)
	time.Sleep(400 * time.Millisecond)
	st2, _ := os.Stat(marker)
	if st1 != nil && st2 != nil && st2.ModTime().After(st1.ModTime()) {
		t.Fatal("background child survived the process-group kill")
	}
}

func TestCancelKillsGroup(t *testing.T) {
	p := policy(t)
	dir := writeMakefile(t, p, "test:\n\tsleep 30\n")
	argv, _ := p.ValidateCommand("make test")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	start := time.Now()
	res := p.Run(ctx, Spec{Dir: dir, Argv: argv, Timeout: time.Minute})
	if !res.Killed || res.TimedOut || time.Since(start) > 5*time.Second {
		t.Fatalf("%+v after %s", res, time.Since(start))
	}
}

func TestHostEnvironmentNotInherited(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "hostsecretvalue123")
	t.Setenv("GITHUB_TOKEN", "ghp_hosttoken000000000000000000")
	p := policy(t)
	dir := writeMakefile(t, p, "test:\n\t@echo \"aws=[$$AWS_SECRET_ACCESS_KEY] gh=[$$GITHUB_TOKEN] home=$$HOME\"\n\t@env | grep -c . \n")
	argv, _ := p.ValidateCommand("make test")
	res := p.Run(context.Background(), Spec{Dir: dir, Argv: argv})
	if res.ExitCode != 0 || !strings.Contains(res.Output, "aws=[] gh=[]") {
		t.Fatalf("host env leaked: %+v", res)
	}
	if !strings.Contains(res.Output, "home="+filepath.Join(p.WorkspaceRoot, "home")) {
		t.Fatalf("HOME not redirected into the workspace: %s", res.Output)
	}
}

func TestOutputCapAndRedaction(t *testing.T) {
	p := policy(t)
	dir := writeMakefile(t, p, "test:\n\t@echo token=abcdefghijklmnop12345\n\t@yes 0123456789 | head -c 100000\n\t@echo AKIAABCDEFGHIJKLMNOP\n")
	argv, _ := p.ValidateCommand("make test")
	res := p.Run(context.Background(), Spec{Dir: dir, Argv: argv})
	if !res.Truncated || len(res.Output) > 20000 {
		t.Fatalf("output not capped: truncated=%v len=%d", res.Truncated, len(res.Output))
	}
	if strings.Contains(res.Output, "abcdefghijklmnop12345") || strings.Contains(res.Output, "AKIAABCDEFGHIJKLMNOP") || res.Redacted < 2 {
		t.Fatalf("secrets not redacted (%d): %s", res.Redacted, res.Output[:200])
	}
}

func TestRunRefusesDirectoryOutsideWorkspace(t *testing.T) {
	p := policy(t)
	res := p.Run(context.Background(), Spec{Dir: t.TempDir(), Argv: []string{"go", "version"}})
	if res.Err == nil || res.ExitCode != -1 {
		t.Fatalf("ran outside the workspace: %+v", res)
	}
}

func TestRedactPatterns(t *testing.T) {
	in := "ghp_abcdefghijklmnopqrstuvwxyz0123 and password=hunter2secret and -----BEGIN RSA PRIVATE KEY-----\nabc\n-----END RSA PRIVATE KEY----- and Authorization: Bearer abcdefghijklmnopqrstu"
	out, n := Redact(in)
	if n < 4 || strings.Contains(out, "hunter2secret") || strings.Contains(out, "abcdefghijklmnopqrstuvwxyz0123") || strings.Contains(out, "\nabc\n") {
		t.Fatalf("%d: %s", n, out)
	}
}

// ---------- SAFE_SANDBOX (macOS only) ----------

func TestSafeSandboxDeniesNetworkAndHostSecrets(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("SAFE_SANDBOX needs macOS sandbox-exec")
	}
	p, err := policy(t).WithMode(ModeSafe)
	if err != nil {
		t.Skip(err)
	}
	home, _ := os.UserHomeDir()
	ssh := filepath.Join(home, ".ssh")
	if _, err := os.Stat(ssh); err != nil {
		t.Skip("no ~/.ssh on this machine to probe")
	}
	dir := writeMakefile(t, p, "test:\n\t@ls "+ssh+" && echo SSH_READ_OK || echo SSH_DENIED\n\t@ls "+filepath.Join(home, ".aws")+" >/dev/null 2>&1 && echo AWS_READ_OK || echo AWS_DENIED\n\t@curl -s --max-time 3 https://example.com >/dev/null && echo NET_OK || echo NET_DENIED\n\t@touch "+filepath.Join(home, "proofline-escape-test")+" 2>/dev/null && echo HOME_WRITE_OK || echo HOME_WRITE_DENIED\n\t@echo hello > ./inside && echo WT_WRITE_OK\n")
	argv, _ := p.ValidateCommand("make test")
	res := p.Run(context.Background(), Spec{Dir: dir, Argv: argv})
	_ = os.Remove(filepath.Join(home, "proofline-escape-test"))
	for _, want := range []string{"SSH_DENIED", "AWS_DENIED", "NET_DENIED", "HOME_WRITE_DENIED", "WT_WRITE_OK"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("want %s in output:\n%s\nerr=%v", want, res.Output, res.Err)
		}
	}
}

func TestSafeSandboxCanRunGoTests(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	p, err := policy(t).WithMode(ModeSafe)
	if err != nil {
		t.Skip(err)
	}
	p.MaxOutput = 64 * 1024
	dir := filepath.Join(p.WorkspaceRoot, "wt")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "m_test.go"), []byte("package m\nimport \"testing\"\nfunc TestOK(t *testing.T){}\n"), 0o644)
	argv, _ := p.ValidateCommand("go test -v ./...")
	res := p.Run(context.Background(), Spec{Dir: dir, Argv: argv, Timeout: 2 * time.Minute})
	if res.ExitCode != 0 || !strings.Contains(res.Output, "--- PASS: TestOK") {
		t.Fatalf("go test inside SAFE_SANDBOX failed: exit=%d err=%v\n%s", res.ExitCode, res.Err, res.Output)
	}
}
