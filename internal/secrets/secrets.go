package secrets

import (
	"bytes"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

const AllowMarker = "biga:allow-secret"

const (
	maxFindings    = 10
	minEntropy     = 3.5
	minLiteralSize = 16
)

type Finding struct {
	Rule string
	Line int
}

func (f Finding) String() string {
	return fmt.Sprintf("%s on line %d", f.Rule, f.Line)
}

type rule struct {
	name   string
	hints  [][]byte
	quick  func(line []byte) bool
	re     *regexp.Regexp
	accept func(groups [][]byte) bool
}

func token(name, pattern string, hints ...string) rule {
	return rule{name: name, hints: toBytes(hints), re: regexp.MustCompile(pattern)}
}

func toBytes(values []string) [][]byte {
	out := make([][]byte, len(values))
	for i, v := range values {
		out[i] = []byte(v)
	}
	return out
}

var placeholders = []string{"example", "placeholder", "changeme", "your_", "your-", "xxxx", "redacted", "dummy"}

var rules = []rule{
	token("private key", `-----BEGIN (?:[A-Z]+ )*PRIVATE KEY(?: BLOCK)?-----`, "PRIVATE KEY"),
	token("AWS access key", `\b(?:AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16}\b`, "AKIA", "ASIA", "ABIA", "ACCA"),
	token("GitHub token", `\b(?:gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{22,255})\b`,
		"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_"),
	token("GitLab token", `\bglpat-[A-Za-z0-9_-]{20,}`, "glpat-"),
	token("Slack token", `\bxox[abeprs]-[A-Za-z0-9-]{10,}`, "xox"),
	token("Stripe live key", `\b[rs]k_live_[A-Za-z0-9]{16,}`, "_live_"),
	token("Google API key", `\bAIza[0-9A-Za-z_-]{35}`, "AIza"),
	token("Anthropic API key", `\bsk-ant-[A-Za-z0-9_-]{20,}`, "sk-ant-"),
	token("OpenAI API key", `\bsk-(?:proj-)?[A-Za-z0-9_-]{32,}`, "sk-"),
	token("npm token", `\bnpm_[A-Za-z0-9]{36}\b`, "npm_"),
	{
		name:  "hard-coded secret",
		quick: mayAssignSecret,
		re: regexp.MustCompile(`(?i)(?:secret|token|passw(?:or)?d|api[_-]?key|private[_-]?key|access[_-]?key)` +
			`[a-z0-9_.-]*["']?\s*[:=]\s*["']([^"'\s$<{][^"'\s]{` + strconv.Itoa(minLiteralSize-1) + `,})["']`),
		accept: acceptLiteral,
	},
}

var secretWords = toBytes([]string{
	"secret", "token", "passw", "api_key", "api-key", "apikey",
	"private_key", "private-key", "privatekey", "access_key", "access-key", "accesskey",
})

func mayAssignSecret(line []byte) bool {
	if bytes.IndexByte(line, '=') < 0 && bytes.IndexByte(line, ':') < 0 {
		return false
	}
	if bytes.IndexByte(line, '"') < 0 && bytes.IndexByte(line, '\'') < 0 {
		return false
	}
	lower := bytes.ToLower(line)
	for _, w := range secretWords {
		if bytes.Contains(lower, w) {
			return true
		}
	}
	return false
}

func acceptLiteral(groups [][]byte) bool {
	value := groups[1]
	if bytes.Contains(value, []byte("://")) || bytes.HasPrefix(value, []byte("/")) || bytes.HasPrefix(value, []byte("./")) {
		return false
	}
	lower := strings.ToLower(string(value))
	for _, p := range placeholders {
		if strings.Contains(lower, p) {
			return false
		}
	}
	return entropy(value) >= minEntropy
}

func entropy(b []byte) float64 {
	var counts [256]int
	for _, c := range b {
		counts[c]++
	}
	n := float64(len(b))
	h := 0.0
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}

func (r rule) possible(data []byte) bool {
	if len(r.hints) == 0 {
		return true
	}
	for _, h := range r.hints {
		if bytes.Contains(data, h) {
			return true
		}
	}
	return false
}

func (r rule) matches(line []byte) bool {
	if r.quick != nil && !r.quick(line) {
		return false
	}
	for _, m := range r.re.FindAllSubmatch(line, -1) {
		if r.accept == nil || r.accept(m) {
			return true
		}
	}
	return false
}

func Scan(data []byte) []Finding {
	var active []rule
	for _, r := range rules {
		if r.possible(data) {
			active = append(active, r)
		}
	}

	var findings []Finding
	for i, line := range bytes.Split(data, []byte("\n")) {
		if bytes.Contains(line, []byte(AllowMarker)) {
			continue
		}
		for _, r := range active {
			if r.matches(line) {
				findings = append(findings, Finding{Rule: r.name, Line: i + 1})
				break
			}
		}
		if len(findings) == maxFindings {
			break
		}
	}
	return findings
}
