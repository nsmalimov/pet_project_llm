// Package examples ships the Local Pilot scenarios: scripted researcher/
// developer/reviewer replies that drive the REAL engine (worktrees, baseline,
// go test, original-test replay, packet builder) on the reservations fixture.
// Agents are scripted; nothing else is.
package examples

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"orchestrator/internal/executor"
)

//go:embed scenarios/*.json
var scenarios embed.FS

// Scenario describes one Local Pilot example.
type Scenario struct {
	Name     string `json:"name"`
	Title    string `json:"title"`
	Expected string `json:"expected"` // verdict a correct engine must derive
	Summary  string `json:"summary"`
}

var catalogue = []Scenario{
	{"A-valid-fix", "Valid fix: baseline fails, UTC normalisation, suite passes, reviewer finds nothing", "SUPPORTED", "The happy path. Still leaves integration and cross-service unchecked."},
	{"B-fake-fix", "Fake fix: the agent inverts the failing assertion instead of fixing the code", "BLOCKED", "The original tests are replayed against the change and fail."},
	{"C-regression", "Regression: the fix collapses every day into one slot; another test breaks", "BLOCKED", "A real test failure after the change contradicts the claim."},
	{"E-counterexample", "Reviewer counterexample: fix and tests look right, the independent challenger finds a breaking scenario", "BLOCKED", "A reviewer counterexample outranks green tests."},
}

func List() []Scenario {
	out := append([]Scenario(nil), catalogue...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Load returns the scripted executor for a scenario name.
func Load(name string) (*executor.ScriptExecutor, *Scenario, error) {
	var meta *Scenario
	for i := range catalogue {
		if catalogue[i].Name == name {
			meta = &catalogue[i]
		}
	}
	if meta == nil || strings.ContainsAny(name, "/\\.") {
		return nil, nil, fmt.Errorf("unknown example %q", name)
	}
	b, err := scenarios.ReadFile("scenarios/" + name + ".json")
	if err != nil {
		return nil, nil, err
	}
	var sc executor.ScriptExecutor
	if err := json.Unmarshal(b, &sc); err != nil {
		return nil, nil, err
	}
	return &sc, meta, nil
}

// Goal is the task text for an example case.
func Goal(sc *Scenario) string {
	return "Fix duplicate reservation across timezones (example " + sc.Name + ")"
}

// Context is the intent / acceptance criteria shown on the case.
func Context(sc *Scenario) []string {
	return []string{
		"Bug: the same room can be booked twice for the same UTC calendar day when clients send the day in different timezones.",
		"Acceptance: TestReserveRejectsSameUTCDayAcrossTimezones passes; existing tests keep passing.",
		"Local Pilot example — agents are scripted; engine, worktree, commands and packet are real. Expected verdict for a correct engine: " + sc.Expected + ".",
	}
}

const ReproCommand = "go test -run TestReserveRejectsSameUTCDayAcrossTimezones ./..."
