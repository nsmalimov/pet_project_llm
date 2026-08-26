package api

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"orchestrator/internal/domain"
)

// Browser e2e: the real SPA rendered by headless Chrome against the real
// API and a real engine run (Local Pilot example). Asserts the decision
// screen answers the product questions. Skips when Chrome is absent.
func chromeBin() string {
	for _, c := range []string{os.Getenv("PROOFLINE_CHROME"), "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/usr/bin/google-chrome", "/usr/bin/chromium", "/usr/bin/chromium-browser"} {
		if c == "" {
			continue
		}
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

func dumpDOM(t *testing.T, chrome, url string) string {
	t.Helper()
	cmd := exec.Command(chrome, "--headless=new", "--disable-gpu", "--no-sandbox", "--virtual-time-budget=8000", "--dump-dom", url)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("chrome: %v", err)
	}
	return string(out)
}

func TestBrowserDecisionScreen(t *testing.T) {
	chrome := chromeBin()
	if chrome == "" {
		t.Skip("no Chrome available for browser e2e")
	}
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	srv, _ := newLocalServer(t)
	// Empty state: overview offers examples, no fake cases.
	dom := dumpDOM(t, chrome, srv.URL+"/")
	for _, want := range []string{"No cases yet", "Load example", "LOCAL_UNSAFE", "GitHub not connected"} {
		if !strings.Contains(dom, want) {
			t.Errorf("overview (empty) lacks %q", want)
		}
	}
	// New case form states what is and is not configured.
	dom = dumpDOM(t, chrome, srv.URL+"/new")
	for _, want := range []string{"GitHub integration not connected", "NOT CONFIGURED", "Start verification"} {
		if !strings.Contains(dom, want) {
			t.Errorf("new case form lacks %q", want)
		}
	}
	// Run the fake-fix example through the real engine.
	var task domain.Task
	if code := postJSON(t, srv.URL+"/examples/B-fake-fix", "", &task); code != 201 {
		t.Fatalf("%d", code)
	}
	waitDone(t, srv, task.ID)
	time.Sleep(200 * time.Millisecond)
	dom = dumpDOM(t, chrome, srv.URL+"/cases/"+task.ID+"/packet")
	for _, want := range []string{"BLOCKED", "Contradictions", "ORIGINAL tests fail", "Problem reproduced", "Change verified", "Not verified", "Human decision", "Local Pilot example", "packet v"} {
		if !strings.Contains(dom, want) {
			t.Errorf("packet screen lacks %q", want)
		}
	}
	if strings.Contains(dom, `class="band s-supported"`) {
		t.Error("fake fix rendered as supported")
	}
	dom = dumpDOM(t, chrome, srv.URL+"/cases/"+task.ID+"/timeline")
	for _, want := range []string{"baseline FAILED", "replaying ORIGINAL tests", "proof packet v"} {
		if !strings.Contains(dom, want) {
			t.Errorf("timeline lacks %q", want)
		}
	}
	// Overview lists the real case with its verdict; no hardcoded counts.
	dom = dumpDOM(t, chrome, srv.URL+"/")
	if !strings.Contains(dom, task.ID) || !strings.Contains(dom, "example: B-fake-fix") {
		t.Error("overview does not list the created case")
	}
	dom = dumpDOM(t, chrome, srv.URL+"/cases/task_doesnotexist")
	if !strings.Contains(dom, "Case not found") {
		t.Error("missing case not reported honestly")
	}
}
