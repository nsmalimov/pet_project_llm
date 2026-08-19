package router

import "testing"

var r = Rules{Executor: "claude", CheapModel: "sonnet", StrongModel: "opus"}

func TestDeveloperEscalatesAfterFailure(t *testing.T) {
	first := r.Route(Request{Role: "developer", Attempt: 0, Uncertainty: "low"})
	if first.Model != "sonnet" {
		t.Fatalf("first attempt should be cheap, got %s", first.Model)
	}
	retry := r.Route(Request{Role: "developer", Attempt: 1})
	if retry.Model != "opus" {
		t.Fatalf("retry should escalate, got %s", retry.Model)
	}
	if retry.Reason == "" {
		t.Fatal("escalation must carry a reason")
	}
}

func TestDeveloperHighUncertaintyGetsStrongModel(t *testing.T) {
	rt := r.Route(Request{Role: "developer", Uncertainty: "high"})
	if rt.Model != "opus" {
		t.Fatalf("got %s", rt.Model)
	}
}

func TestReviewerIndependentFromAuthor(t *testing.T) {
	rt := r.Route(Request{Role: "reviewer", NeedIndependence: true, AuthorModel: "sonnet"})
	if rt.Model == "sonnet" {
		t.Fatal("reviewer must not use the author's model")
	}
	rt = r.Route(Request{Role: "reviewer", NeedIndependence: true, AuthorModel: "opus"})
	if rt.Model == "opus" {
		t.Fatal("reviewer must not use the author's model")
	}
}

func TestTesterUsesCommandExecutor(t *testing.T) {
	rt := r.Route(Request{Role: "tester"})
	if rt.Executor != "command" {
		t.Fatalf("got %s", rt.Executor)
	}
}

func TestDeepInvestigationUsesStrongModel(t *testing.T) {
	rt := r.Route(Request{Role: "researcher", Deep: true})
	if rt.Model != "opus" {
		t.Fatalf("got %s", rt.Model)
	}
}
