package github

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"orchestrator/internal/github/fakegh"
)

func TestWebhookSignatureAndParsing(t *testing.T) {
	f := fakegh.NewFakeGitHub()
	defer f.Close()
	f.SetPR("acme", "svc", 7, "base111", "head222", "Fix dedup")
	req := f.WebhookRequest("s3cret", "d-1", "synchronize", "acme", "svc", 7, "/github/webhook")
	d, err := ParseDelivery("s3cret", req)
	if err != nil || d.ID != "d-1" || !d.Relevant() || d.PR.HeadSHA != "head222" || d.PR.BaseSHA != "base111" || d.PR.Owner != "acme" {
		t.Fatalf("%v %+v", err, d)
	}
	// Wrong secret, missing signature, missing delivery id.
	req = f.WebhookRequest("wrong", "d-2", "synchronize", "acme", "svc", 7, "/github/webhook")
	if _, err := ParseDelivery("s3cret", req); err != ErrBadSignature {
		t.Fatalf("bad signature accepted: %v", err)
	}
	req = httptest.NewRequest("POST", "/github/webhook", strings.NewReader("{}"))
	if _, err := ParseDelivery("s3cret", req); err == nil {
		t.Fatal("unsigned webhook accepted")
	}
	req = f.WebhookRequest("s3cret", "", "synchronize", "acme", "svc", 7, "/github/webhook")
	if _, err := ParseDelivery("s3cret", req); err == nil {
		t.Fatal("delivery without id accepted")
	}
	req = f.WebhookRequest("s3cret", "d-3", "labeled", "acme", "svc", 7, "/github/webhook")
	if d, _ := ParseDelivery("s3cret", req); d.Relevant() {
		t.Fatal("irrelevant action treated as relevant")
	}
}

func TestFetchPRAndRevocation(t *testing.T) {
	f := fakegh.NewFakeGitHub()
	defer f.Close()
	f.SetPR("acme", "svc", 7, "base111", "head222", "Fix dedup")
	c := &Client{Token: "t", BaseURL: f.Server.URL}
	pr, err := c.FetchPR("acme", "svc", 7)
	if err != nil || pr.HeadSHA != "head222" {
		t.Fatal(err)
	}
	if _, err := c.FetchPR("acme", "svc", 8); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("404 must read as revoked/not visible: %v", err)
	}
	f.Revoked = true
	if _, err := c.FetchPR("acme", "svc", 7); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("403 must read as revoked: %v", err)
	}
	if err := c.PostStatus(context.Background(), "acme", "svc", "head222", CommitStatus{State: "success"}); err == nil {
		t.Fatal("post under revocation succeeded")
	}
}
