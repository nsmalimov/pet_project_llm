package github

import (
	"strings"
	"testing"

	"orchestrator/internal/domain"
)

func packet(verdict domain.ClaimStatus, head string) *domain.Packet {
	return &domain.Packet{Version: 2, Verdict: verdict,
		Source: domain.SourceState{HeadSHAs: map[string]string{"svc": head}},
		Claims: []domain.Claim{{Core: true, Status: domain.ClaimSupported, Title: "Change verified"}, {Core: true, Status: domain.ClaimInsufficient, Title: "Independent challenge"}},
		Gaps:   []string{"handler"}}
}

func TestNeverFakeGreen(t *testing.T) {
	if st := BuildStatus(packet(domain.ClaimSupported, "aaa"), "svc", "aaa", "u"); st.State != "success" || !strings.Contains(st.Description, "SUPPORTED") {
		t.Fatalf("%+v", st)
	}
	for _, v := range []domain.ClaimStatus{domain.ClaimInsufficient, domain.ClaimBlocked, domain.ClaimStale} {
		if st := BuildStatus(packet(v, "aaa"), "svc", "aaa", "u"); st.State == "success" {
			t.Fatalf("%s must not be success", v)
		}
	}
	// A newer push: the packet is for commit A, the status is asked for B.
	st := BuildStatus(packet(domain.ClaimSupported, "aaa"), "svc", "bbb", "u")
	if st.State == "success" || !strings.Contains(st.Description, "NOT VERIFIED") {
		t.Fatalf("stale packet reported as success: %+v", st)
	}
	if st := BuildStatus(nil, "svc", "bbb", "u"); st.State == "success" {
		t.Fatal("no packet must not be success")
	}
	p := packet(domain.ClaimSupported, "aaa")
	p.Source.Dirty = true
	if st := BuildStatus(p, "svc", "aaa", "u"); st.State == "success" {
		t.Fatal("dirty source must not be success")
	}
}

func TestCommentNeverApproves(t *testing.T) {
	c := BuildComment(packet(domain.ClaimSupported, "aaa"), "svc", "aaa", "http://x/cases/1")
	if strings.Contains(strings.ToLower(c), "approved") || !strings.Contains(c, "not an approval") || !strings.Contains(c, "Not verified") {
		t.Fatalf("%s", c)
	}
}
