package verify

import (
	"context"
	"testing"
	"time"
)

func TestVerboseInjection(t *testing.T) {
	cases := map[string]string{
		"go test ./...":            "go test -v ./...",
		"go test -run TestX ./...": "go test -v -run TestX ./...",
		"go test -v ./...":         "go test -v ./...",
		"python3 -m pytest -q":     "python3 -m pytest -q -v",
		"pytest tests/ -v":         "pytest tests/ -v",
		"npm test --silent":        "npm test --silent",
		"make test":                "make test",
	}
	for in, want := range cases {
		if got := Verbose(in); got != want {
			t.Errorf("Verbose(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseGoTests(t *testing.T) {
	out := "=== RUN   TestA\n--- PASS: TestA (0.00s)\n=== RUN   TestB\n    x_test.go:9: boom\n--- FAIL: TestB (0.00s)\n--- SKIP: TestC (0.00s)\nFAIL\n"
	tests := ParseTests("go test -v ./...", out)
	if len(tests) != 3 || tests[0].Status != "pass" || tests[1].Name != "TestB" || tests[1].Status != "fail" || tests[2].Status != "skip" {
		t.Fatalf("%+v", tests)
	}
	if got := ParseTests("go test -v ./...", "ok  \treservations\t0.1s [no tests to run]"); got == nil || len(got) != 0 {
		t.Fatalf("known runner with no tests must yield empty non-nil, got %#v", got)
	}
	if ParseTests("npm test", "anything") != nil {
		t.Fatal("unknown runner must yield nil")
	}
}

func TestParsePytest(t *testing.T) {
	out := "tests/test_a.py::test_ok PASSED\ntests/test_a.py::test_bad FAILED\ntests/test_a.py::test_skip SKIPPED\n"
	tests := ParseTests("python3 -m pytest -q -v", out)
	if len(tests) != 3 || tests[1].Status != "fail" || tests[2].Status != "skip" {
		t.Fatalf("%+v", tests)
	}
}

func TestTimeoutIsNotAPass(t *testing.T) {
	res := Run(context.Background(), "r", t.TempDir(), "sleep 5", 200*time.Millisecond)
	if res.Passed || !res.TimedOut {
		t.Fatalf("%+v", res)
	}
}
