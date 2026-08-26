package sandbox

import (
	"regexp"
	"strings"
)

// Secret patterns redacted before persistence. Conservative on purpose:
// false positives cost a few masked characters, false negatives leak a key.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)AKIA[0-9A-Z]{16}`),                                                         // AWS access key id
	regexp.MustCompile(`(?i)aws_secret_access_key\s*[=:]\s*\S+`),                                       // AWS secret
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),                                                   // GitHub tokens
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),                                                 // GitHub fine-grained
	regexp.MustCompile(`sk-[A-Za-z0-9_\-]{16,}`),                                                       // OpenAI/Anthropic style
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9\-]{10,}`),                                                // Slack
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{16,}`),                                            // bearer tokens
	regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password|passwd|pwd)\s*[=:]\s*["']?[^\s"']{8,}`), // key=value
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`), // JWT
}

// Redact masks secret-like substrings. Returns the text and the number of
// replacements so artifacts can state "redacted: N".
func Redact(s string) (string, int) {
	n := 0
	for _, re := range secretPatterns {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			n++
			// Keep a short prefix for recognisability (e.g. "AKIA…", "ghp_…").
			keep := 4
			if idx := strings.IndexAny(m, "=:"); idx >= 0 && idx < 40 {
				keep = idx + 1 // keep "password=" style label
			}
			if keep > len(m) {
				keep = len(m)
			}
			return m[:keep] + "[REDACTED]"
		})
	}
	return s, n
}
