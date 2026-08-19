// Package router decides which executor and model handle a given step.
// The interface takes the signals an intelligent router would need (role,
// uncertainty, attempt history, independence requirements); the shipped
// implementation is deliberately simple rules. Replacing it with a learned /
// LLM-driven router later does not touch the engine.
package router

import "fmt"

// Request carries the routing signals for one step.
type Request struct {
	Role             string // researcher | developer | reviewer | tester
	Uncertainty      string // low | medium | high (from research)
	Attempt          int    // prior attempts of this role on this task
	NeedIndependence bool   // reviewer must be independent from the author
	AuthorModel      string // model that produced the change under review
	Deep             bool   // deep investigation (post-blocked) vs initial scan
}

// Route is the routing outcome. Reason is recorded as an event so escalations
// are observable.
type Route struct {
	Executor string `json:"executor"`
	Model    string `json:"model,omitempty"`
	Reason   string `json:"reason"`
}

type Router interface {
	Route(req Request) Route
}

// Rules is the v0 rule-based router.
//
//   - researcher: cheap model; strong model for deep investigations
//   - developer:  cheap model first, escalate to strong after a failed attempt
//   - reviewer:   a model different from the author's, for independence
//   - tester:     the command executor (real test runs, no LLM)
type Rules struct {
	Executor    string // executor for LLM roles: "claude" or "mock"
	CheapModel  string // e.g. "sonnet"
	StrongModel string // e.g. "opus"
}

func (r Rules) Route(req Request) Route {
	switch req.Role {
	case "tester":
		return Route{Executor: "command", Reason: "verification runs real test commands, no LLM needed"}
	case "researcher":
		if req.Deep || req.Attempt > 0 {
			return Route{Executor: r.Executor, Model: r.StrongModel,
				Reason: "deep investigation after blocked/uncertain outcome → strong model"}
		}
		return Route{Executor: r.Executor, Model: r.CheapModel, Reason: "initial codebase scan → cheap model"}
	case "developer":
		if req.Attempt > 0 {
			return Route{Executor: r.Executor, Model: r.StrongModel,
				Reason: fmt.Sprintf("attempt %d after failure → escalate to strong model", req.Attempt+1)}
		}
		if req.Uncertainty == "high" {
			return Route{Executor: r.Executor, Model: r.StrongModel, Reason: "high uncertainty → strong model"}
		}
		return Route{Executor: r.Executor, Model: r.CheapModel, Reason: "first attempt, low/medium uncertainty → cheap model"}
	case "reviewer":
		if req.NeedIndependence && req.AuthorModel == r.CheapModel {
			return Route{Executor: r.Executor, Model: r.StrongModel,
				Reason: "independent review → model different from author (" + req.AuthorModel + ")"}
		}
		return Route{Executor: r.Executor, Model: r.CheapModel,
			Reason: "independent review → model different from author (" + req.AuthorModel + ")"}
	default:
		return Route{Executor: r.Executor, Model: r.CheapModel, Reason: "default route"}
	}
}
