package executor

import (
	"strings"
	"testing"
)

func TestAgentEnvAllowlist(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "x")
	t.Setenv("GITHUB_TOKEN", "x")
	t.Setenv("DATABASE_URL", "x")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("ANTHROPIC_API_KEY", "k")
	t.Setenv("CLAUDE_CONFIG_DIR", "/c")
	env := strings.Join(agentEnv(), "\n")
	for _, bad := range []string{"AWS_SECRET_ACCESS_KEY=", "GITHUB_TOKEN=", "DATABASE_URL=", "CLAUDECODE="} {
		if strings.Contains(env, bad) {
			t.Errorf("host variable leaked to the agent: %s", bad)
		}
	}
	for _, good := range []string{"PATH=", "HOME=", "ANTHROPIC_API_KEY=k", "CLAUDE_CONFIG_DIR=/c", "PROOFLINE_AGENT=1"} {
		if !strings.Contains(env, good) {
			t.Errorf("expected %s in agent env", good)
		}
	}
}
