// Package fixtures ships the reservations demo repository (a real timezone
// duplicate-booking bug with one failing test) so the app can instantiate a
// Local Pilot case without any external checkout.
package fixtures

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

//go:embed reservations/*.fixture reservations/cmd/server/*.fixture
var Reservations embed.FS

// Materialize writes the reservations fixture into dir and commits it as a
// standalone git repository. The bug is on the initial commit.
func Materialize(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Files carry a .fixture suffix so the buggy code is neither a nested
	// module nor part of ./... in this repository.
	err := fs.WalkDir(Reservations, "reservations", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := Reservations.ReadFile(p)
		if err != nil {
			return err
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(p, "reservations/"), ".fixture")
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		return err
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.name=fixture", "-c", "user.email=fixture@localhost", "commit", "-q", "-m", "reservations service with timezone duplicate bug"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1"}
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %w\n%s", args, err, out)
		}
	}
	return nil
}
