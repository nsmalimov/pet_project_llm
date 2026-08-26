package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"orchestrator/internal/domain"
)

// Phase 1 product acceptance over HTTP: every scenario the product must
// show honestly — valid, fake fix, regression, reviewer counterexample,
// stale after an edit + re-verify, interrupted/resumed — plus policy
// enforcement, token management and backup/restore.

func runExample(t *testing.T, srv *httptest.Server, name string) (domain.Task, map[string]any) {
	t.Helper()
	var task domain.Task
	if code := postJSON(t, srv.URL+"/examples/"+name, "", &task); code != 201 {
		t.Fatalf("example %s → %d", name, code)
	}
	return task, waitDone(t, srv, task.ID)
}

func verdictOf(v map[string]any) string { return v["packet"].(map[string]any)["verdict"].(string) }
func statusOf(v map[string]any) string  { return v["task"].(map[string]any)["status"].(string) }

func TestAcceptanceScenariosOverHTTP(t *testing.T) {
	srv, eng := newLocalServer(t)
	// A: valid fix → SUPPORTED with integration SUPPORTED (policy from the example).
	a, va := runExample(t, srv, "A-valid-fix")
	if verdictOf(va) != "supported" {
		t.Fatalf("A: %s", verdictOf(va))
	}
	claims := va["packet"].(map[string]any)["claims"].([]any)
	integ := ""
	for _, c := range claims {
		m := c.(map[string]any)
		if m["type"] == "integration_checked" {
			integ = m["status"].(string)
		}
	}
	if integ != "supported" {
		t.Fatalf("A integration: %s", integ)
	}
	// Stale after a later edit; re-verify makes it fresh again (new version).
	wt := filepath.Join(eng.WS.Root, a.ID, "reservations", "store.go")
	b, _ := os.ReadFile(wt)
	os.WriteFile(wt, append(b, []byte("\n// later edit\n")...), 0o644)
	var vs map[string]any
	getJSON(t, srv.URL+"/tasks/"+a.ID+"/packet", &vs)
	if verdictOf(vs) != "stale" {
		t.Fatalf("after edit: %s", verdictOf(vs))
	}
	if code := postJSON(t, srv.URL+"/tasks/"+a.ID+"/reverify", "", nil); code != 202 {
		t.Fatalf("reverify → %d", code)
	}
	va2 := waitDone(t, srv, a.ID)
	if verdictOf(va2) != "supported" || int(va2["packet"].(map[string]any)["version"].(float64)) < 3 {
		t.Fatalf("after reverify: %s v%v", verdictOf(va2), va2["packet"].(map[string]any)["version"])
	}
	// B/C/E: never green.
	for _, name := range []string{"B-fake-fix", "C-regression", "E-counterexample"} {
		_, v := runExample(t, srv, name)
		if verdictOf(v) != "blocked" {
			t.Errorf("%s: %s", name, verdictOf(v))
		}
	}
	// Interrupted then resumed: cancel A-valid-fix early, resume, finish.
	var task domain.Task
	postJSON(t, srv.URL+"/examples/A-valid-fix", "", &task)
	for i := 0; i < 200; i++ {
		if postJSON(t, srv.URL+"/tasks/"+task.ID+"/cancel", "", nil) == 202 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for i := 0; i < 200; i++ {
		var v map[string]any
		getJSON(t, srv.URL+"/tasks/"+task.ID+"/packet", &v)
		if statusOf(v) == "interrupted" {
			break
		}
		if statusOf(v) == "done" {
			t.Skip("finished before cancel")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if code := postJSON(t, srv.URL+"/tasks/"+task.ID+"/resume", "", nil); code != 200 {
		t.Fatalf("resume → %d", code)
	}
	v := waitDone(t, srv, task.ID)
	if statusOf(v) != "done" || verdictOf(v) != "supported" {
		t.Fatalf("resumed: %s %s", statusOf(v), verdictOf(v))
	}
	// Metrics reflect persisted state only.
	var m map[string]any
	if code := getJSON(t, srv.URL+"/metrics", &m); code != 200 || m["packets_by_verdict"].(map[string]any)["blocked"].(float64) < 3 {
		t.Fatalf("metrics: %d %v", code, m)
	}
	if code := getJSON(t, srv.URL+"/ready", nil); code != 200 {
		t.Fatalf("ready → %d", code)
	}
}

func TestRepositoryPolicyEnforcedOverHTTP(t *testing.T) {
	srv, eng := newLocalServer(t)
	dir := filepath.Join(t.TempDir(), "svc")
	mkRepo(t, dir)
	var rp map[string]any
	if code := postJSON(t, srv.URL+"/repos", fmt.Sprintf(`{"path":%q}`, dir), &rp); code != 201 {
		t.Fatalf("add repo → %d", code)
	}
	id := rp["id"].(string)
	req, _ := http.NewRequest("PUT", srv.URL+"/repos/"+id+"/policy", strings.NewReader(`{"test_command":"go test ./...","allowed_runners":["go"],"agent_write":false}`))
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("policy → %d", resp.StatusCode)
	}
	// Runner outside the repo allowlist is refused; verify-only enforced.
	if code := postJSON(t, srv.URL+"/tasks", fmt.Sprintf(`{"goal":"x","repos":[%q],"test_command":"make test"}`, id), nil); code != 400 {
		t.Fatalf("disallowed runner → %d", code)
	}
	if code := postJSON(t, srv.URL+"/tasks", fmt.Sprintf(`{"goal":"x","repos":[%q]}`, id), nil); code != 400 {
		t.Fatalf("agent_write=false without head → %d", code)
	}
	// A hostile policy is rejected at validation.
	req, _ = http.NewRequest("PUT", srv.URL+"/repos/"+id+"/policy", strings.NewReader(`{"test_command":"sh -c id"}`))
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 400 {
		t.Fatalf("hostile policy → %d", resp.StatusCode)
	}
	_ = eng
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	if _, err := os.Stat("../../bin/orc"); err != nil {
		t.Skip("bin/orc not built (make build)")
	}
	srv, eng := newLocalServer(t)
	task, _ := runExample(t, srv, "B-fake-fix")
	data := filepath.Dir(eng.WS.Root)
	archive := filepath.Join(t.TempDir(), "b.tar")
	if out, err := exec.Command("../../bin/orc", "backup", archive, "--data", data).CombinedOutput(); err != nil {
		t.Fatalf("backup: %v %s", err, out)
	}
	target := filepath.Join(t.TempDir(), "restored")
	if out, err := exec.Command("../../bin/orc", "restore", archive, "--data", target).CombinedOutput(); err != nil {
		t.Fatalf("restore: %v %s", err, out)
	}
	out, err := exec.Command("../../bin/orc", "packet", task.ID, "--data", target).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "BLOCKED") && !strings.Contains(string(out), "STALE") {
		t.Fatalf("restored packet unreadable: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(target, "worktrees")); err == nil {
		t.Fatal("worktrees must not be part of the backup")
	}
}
