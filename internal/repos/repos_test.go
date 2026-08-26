package repos

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"orchestrator/internal/sandbox"
)

func gitInit(t *testing.T, dir string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	for _, a := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "i"}} {
		c := exec.Command("git", a...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
}

func TestSafeModeRefusesRawPathsAndOutsideRoots(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("SAFE_SANDBOX only on macOS")
	}
	tmp := t.TempDir()
	pol, err := sandbox.Default(filepath.Join(tmp, "data"))
	if err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(tmp, "roots", "svc")
	outside := filepath.Join(tmp, "elsewhere", "svc")
	gitInit(t, inside)
	gitInit(t, outside)
	safe, err := pol.WithMode(sandbox.ModeSafe)
	if err != nil {
		t.Skip(err)
	}
	// SAFE without roots: refuses everything.
	r := Open(filepath.Join(tmp, "data"), safe)
	if _, err := r.Add(inside, ""); err == nil {
		t.Fatal("SAFE without repos root accepted a repository")
	}
	root, _ := sandbox.Canonical(filepath.Join(tmp, "roots"))
	safe.ReposRoots = []string{root}
	r = Open(filepath.Join(tmp, "data"), safe)
	rp, err := r.Add(inside, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Add(outside, ""); err == nil {
		t.Fatal("repository outside the roots accepted")
	}
	if _, err := r.Resolve(inside); err == nil {
		t.Fatal("SAFE mode resolved a raw path")
	}
	if p, err := r.Resolve(rp.ID); err != nil || p != rp.Path {
		t.Fatalf("id resolution: %v %s", err, p)
	}
	// A registered path replaced by a symlink to outside is re-validated.
	os.RemoveAll(inside)
	os.Symlink(outside, inside)
	if _, err := r.Resolve(rp.ID); err == nil {
		t.Fatal("symlinked-away repository resolved")
	}
	// Repositories inside the workspace are refused in every mode.
	ws := filepath.Join(pol.WorkspaceRoot, "evil")
	gitInit(t, ws)
	if _, err := Open(filepath.Join(tmp, "data"), pol).Add(ws, ""); err == nil {
		t.Fatal("repository inside the workspace accepted")
	}
}
