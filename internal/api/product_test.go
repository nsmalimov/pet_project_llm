package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"orchestrator/examples"
	"orchestrator/internal/domain"
	"orchestrator/internal/engine"
	"orchestrator/internal/executor"
	"orchestrator/internal/gitws"
	"orchestrator/internal/repos"
	"orchestrator/internal/router"
	"orchestrator/internal/store"
)

// Product path over HTTP in local single-user mode: examples → real engine
// run → packet → verdict → cancel. No auth configured = local workspace.
func newLocalServer(t *testing.T) (*httptest.Server, *engine.Engine) {
	t.Helper()
	tmp := t.TempDir()
	data := filepath.Join(tmp, "data")
	st, _ := store.NewFileStore(data)
	ws := gitws.NewManager(filepath.Join(data, "worktrees"))
	execs := map[string]executor.Executor{
		"scenario": &executor.ScenarioExecutor{Lookup: func(n string) (*executor.ScriptExecutor, error) { sc, _, err := examples.Load(n); return sc, err }},
	}
	eng := engine.New(st, ws, execs, router.Rules{Executor: "claude", CheapModel: "sonnet", StrongModel: "opus"}, nil, engine.DefaultConfig())
	eng.Repos = repos.Open(data, eng.Policy)
	s := New(eng)
	s.ExampleRoot = filepath.Join(tmp, "examples")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, eng
}

func getJSON(t *testing.T, url string, v any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if v != nil {
		_ = json.NewDecoder(resp.Body).Decode(v)
	}
	return resp.StatusCode
}

func postJSON(t *testing.T, url, body string, v any) int {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if v != nil {
		_ = json.NewDecoder(resp.Body).Decode(v)
	}
	return resp.StatusCode
}

func waitDone(t *testing.T, srv *httptest.Server, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		var v map[string]any
		getJSON(t, srv.URL+"/tasks/"+id+"/packet", &v)
		task := v["task"].(map[string]any)
		st := domain.TaskStatus(task["status"].(string))
		if st.Terminal() || st == domain.StatusAwaitingDecision {
			return v
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("task did not finish")
	return nil
}

func TestExampleCaseEndToEndOverHTTP(t *testing.T) {
	srv, _ := newLocalServer(t)
	var sys map[string]any
	if code := getJSON(t, srv.URL+"/system", &sys); code != 200 || sys["exec_mode"] != "LOCAL_UNSAFE" || sys["examples_enabled"] != true || sys["github_connected"] != false {
		t.Fatalf("system: %d %v", code, sys)
	}
	var ex map[string]any
	getJSON(t, srv.URL+"/examples", &ex)
	if len(ex["scenarios"].([]any)) < 4 {
		t.Fatal("examples missing")
	}
	// Fake fix must end BLOCKED and say why.
	var task domain.Task
	if code := postJSON(t, srv.URL+"/examples/B-fake-fix", "", &task); code != 201 || task.Scenario != "B-fake-fix" {
		t.Fatalf("create example: %d %+v", code, task)
	}
	v := waitDone(t, srv, task.ID)
	packet := v["packet"].(map[string]any)
	if packet["verdict"] != "blocked" || !strings.Contains(strings.Join(anyStrings(packet["contradictions"]), " "), "ORIGINAL tests fail") {
		t.Fatalf("fake fix packet: %v", packet["verdict"])
	}
	if packet["live"] == true {
		t.Fatal("finished packet reported as live")
	}
	// Verdict pinned to the viewed version; any other version is refused.
	cur := int(packet["version"].(float64))
	if code := postJSON(t, srv.URL+"/tasks/"+task.ID+"/verdict", fmt.Sprintf(`{"decision":"reject","note":"fake","packet_version":%d}`, cur+1), nil); code != 409 {
		t.Fatalf("stale verdict → %d", code)
	}
	if code := postJSON(t, srv.URL+"/tasks/"+task.ID+"/verdict", fmt.Sprintf(`{"decision":"reject","note":"fake","packet_version":%d}`, cur), nil); code != 201 {
		t.Fatalf("verdict → %d", code)
	}
	// Listing shows the case with its scenario label; unknown example 404.
	var list []domain.Task
	getJSON(t, srv.URL+"/tasks", &list)
	if len(list) != 1 || list[0].Scenario != "B-fake-fix" {
		t.Fatalf("list: %+v", list)
	}
	if code := postJSON(t, srv.URL+"/examples/nope", "", nil); code != 404 {
		t.Fatalf("unknown example → %d", code)
	}
}

func TestCancelOverHTTPLeavesInterrupted(t *testing.T) {
	srv, _ := newLocalServer(t)
	var task domain.Task
	if code := postJSON(t, srv.URL+"/examples/A-valid-fix", "", &task); code != 201 {
		t.Fatalf("%d", code)
	}
	// Cancel as soon as it is running.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		code := postJSON(t, srv.URL+"/tasks/"+task.ID+"/cancel", "", nil)
		if code == 202 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	deadline = time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var v map[string]any
		getJSON(t, srv.URL+"/tasks/"+task.ID+"/packet", &v)
		st := domain.TaskStatus(v["task"].(map[string]any)["status"].(string))
		if st == domain.StatusInterrupted {
			if v["packet"].(map[string]any)["verdict"] == "supported" {
				t.Fatal("cancelled task shows supported")
			}
			return
		}
		if st == domain.StatusDone {
			t.Skip("task finished before cancel could land (fast machine)")
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("task not interrupted after cancel")
}

func anyStrings(v any) []string {
	var out []string
	if xs, ok := v.([]any); ok {
		for _, x := range xs {
			out = append(out, x.(string))
		}
	}
	return out
}

var _ = context.Background
