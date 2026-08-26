// Package integration runs the one real integration provider: start the
// service from the task worktree (approved command, sandboxed, localhost
// only), probe it with approved HTTP checks, capture sanitized responses,
// and hand back a result the engine binds to the exact source SHA.
package integration

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"orchestrator/internal/domain"
	"orchestrator/internal/repos"
	"orchestrator/internal/sandbox"
)

type Result struct {
	Passed   bool
	Checks   []domain.TestCase
	Output   string // check log + service output tail (redacted)
	Redacted int
	Err      string // runner-level problem (could not start, port never opened)
}

// Run executes chk against the service started in dir.
func Run(ctx context.Context, pol sandbox.Policy, dir string, chk *repos.IntegrationCheck) Result {
	var log strings.Builder
	res := Result{}
	argv, err := pol.ValidateCommand(chk.Start)
	if err != nil {
		res.Err = "start command rejected: " + err.Error()
		res.Output = res.Err
		return res
	}
	port := chk.Port
	proc, err := pol.Start(ctx, sandbox.Spec{Dir: dir, Argv: argv, LocalNetwork: true, ExtraEnv: []string{"PORT=" + strconv.Itoa(port)}})
	if err != nil {
		res.Err = "could not start service: " + err.Error()
		res.Output = res.Err
		return res
	}
	defer func() {
		out, n := proc.Stop()
		res.Redacted += n
		res.Output = log.String() + "\n--- service output ---\n" + tail(out, 4000)
	}()
	startup := time.Duration(chk.StartupSeconds) * time.Second
	if startup == 0 {
		startup = 60 * time.Second
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(startup)
	up := false
	for time.Now().Before(deadline) {
		if proc.Exited() {
			break
		}
		c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			c.Close()
			up = true
			break
		}
		select {
		case <-ctx.Done():
			res.Err = "cancelled"
			return res
		case <-time.After(250 * time.Millisecond):
		}
	}
	if !up {
		res.Err = fmt.Sprintf("service did not listen on %s within %s", addr, startup)
		fmt.Fprintln(&log, res.Err)
		return res
	}
	fmt.Fprintf(&log, "service up on %s (%s)\n", addr, chk.Start)
	client := &http.Client{Timeout: 15 * time.Second}
	res.Passed = true
	for _, c := range chk.Checks {
		method := c.Method
		if method == "" {
			method = "GET"
		}
		req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+c.Path, strings.NewReader(c.Body))
		if err != nil {
			res.Checks = append(res.Checks, domain.TestCase{Name: c.Name, Status: "fail"})
			res.Passed = false
			fmt.Fprintf(&log, "[FAIL] %s: bad request: %v\n", c.Name, err)
			continue
		}
		if c.Body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			res.Checks = append(res.Checks, domain.TestCase{Name: c.Name, Status: "fail"})
			res.Passed = false
			fmt.Fprintf(&log, "[FAIL] %s: %s %s → %v\n", c.Name, method, c.Path, err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		bodyText, n := sandbox.Redact(strings.TrimSpace(string(body)))
		res.Redacted += n
		ok := resp.StatusCode == c.ExpectStatus && (c.ExpectBody == "" || strings.Contains(bodyText, c.ExpectBody))
		st := "pass"
		if !ok {
			st = "fail"
			res.Passed = false
		}
		res.Checks = append(res.Checks, domain.TestCase{Name: c.Name, Status: st})
		fmt.Fprintf(&log, "[%s] %s: %s %s %s → %d (expected %d%s) body: %s\n", strings.ToUpper(st), c.Name, method, c.Path, c.Body, resp.StatusCode, c.ExpectStatus, expectBody(c), tail(bodyText, 300))
	}
	return res
}

func expectBody(c repos.HTTPCheck) string {
	if c.ExpectBody == "" {
		return ""
	}
	return ", body ∋ " + strconv.Quote(c.ExpectBody)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
