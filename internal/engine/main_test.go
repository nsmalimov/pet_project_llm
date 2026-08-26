package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// One shared Go build cache for the whole package run: each test still gets
// its own workspace, but compiled objects are reused (minutes → seconds).
func TestMain(m *testing.M) {
	if os.Getenv("PROOFLINE_SHARED_CACHE") == "" {
		dir := filepath.Join(os.TempDir(), "proofline-test-cache")
		_ = os.MkdirAll(dir, 0o755)
		os.Setenv("PROOFLINE_SHARED_CACHE", dir)
	}
	os.Exit(m.Run())
}
