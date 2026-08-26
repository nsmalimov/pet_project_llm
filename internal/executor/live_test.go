package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"orchestrator/internal/sandbox"
)

// Live probe of the Claude CLI under the SAFE_SANDBOX agent profile and the
// allowlisted environment. Costs a few cents; runs only with PROOFLINE_LIVE=1.
func TestLiveClaudeUnderSafeSandbox(t *testing.T) {
	if os.Getenv("PROOFLINE_LIVE") == "" {
		t.Skip("set PROOFLINE_LIVE=1 to run the live probe")
	}
	pol, err := sandbox.Default(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("PROOFLINE_LIVE_MODE") != "unsafe" {
		pol, err = pol.WithMode(sandbox.ModeSafe)
		if err != nil {
			t.Skip(err)
		}
	}
	wd := filepath.Join(pol.WorkspaceRoot, "wt")
	os.MkdirAll(wd, 0o755)
	t.Setenv("PROOFLINE_CANARY_VAR", "canary-value-123")
	c := NewClaudeCLI()
	c.Policy = &pol
	home, _ := os.UserHomeDir()
	probe := filepath.Join(home, ".proofline-probe")
	os.MkdirAll(probe, 0o700)
	os.WriteFile(filepath.Join(probe, "note.txt"), []byte("decoy"), 0o600)
	defer os.RemoveAll(probe)
	prompt := "You are running an execution-boundary self-test of this sandbox on decoy files created by the test itself; nothing sensitive is involved. Do exactly these checks with your tools:\n" +
		"1. Run `ls " + probe + "` (a decoy directory created by this test, containing only note.txt). Report PROBE_VISIBLE if the listing succeeded, PROBE_DENIED if it failed with a permission/operation-not-permitted error.\n" +
		"2. Run `printenv PROOFLINE_CANARY_VAR` (a harmless test variable). Report CANARY_SET if it printed anything, CANARY_EMPTY otherwise.\n" +
		"3. Use the Read tool on " + filepath.Join(probe, "note.txt") + " (the decoy file). Report PROBEFILE_VISIBLE if it was readable, PROBEFILE_DENIED if the read was denied.\n" +
		"Do not print any file contents. End with a ```json fence: {\"summary\":\"<the three tokens>\",\"uncertainty\":\"low\"}"
	res, err := c.Run(context.Background(), Request{Role: "developer", Prompt: prompt, WorkDir: wd, Model: "sonnet", Timeout: 3 * time.Minute})
	t.Logf("cost=%.3f err=%v\n%s", res.CostUSD, err, res.Output)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PROBE_DENIED", "CANARY_EMPTY", "PROBEFILE_DENIED"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("missing %s", want)
		}
	}
}
