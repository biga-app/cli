package secrets

import (
	"slices"
	"strings"
	"testing"
)

const (
	awsKey      = "AKIA" + "IOSFODNN7EXAMPLE"
	pemHeader   = "-----BEGIN RSA PRIVATE" + " KEY-----"
	randomValue = "q8Zr3Lm0Xv7Tn2Hs9Bw4"
)

func TestScan(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []Finding
	}{
		{"nothing", "package main\n\nfunc main() {}\n", nil},
		{"aws access key", "id = " + awsKey + "\n", []Finding{{"AWS access key", 1}}},
		{"private key on a later line", "x\ny\n" + pemHeader + "\n", []Finding{{"private key", 3}}},
		{"github token", "t: ghp_" + strings.Repeat("a1B2", 9), []Finding{{"GitHub token", 1}}},
		{"slack token", "xoxb-" + "123456789012-abcdefghij", []Finding{{"Slack token", 1}}},
		{"stripe live key", "sk_live_" + "abcdefghijklmnop1234", []Finding{{"Stripe live key", 1}}},
		{"anthropic key", "sk-ant-" + "api03-abcdefghijklmnopqrstu", []Finding{{"Anthropic API key", 1}}},
		{"google key", "AIza" + strings.Repeat("a1_-B", 7), []Finding{{"Google API key", 1}}},
		{"npm token", "npm_" + strings.Repeat("aB3dE", 7) + "x", []Finding{{"npm token", 1}}},
		{"quoted secret", `api_key = "` + randomValue + `"`, []Finding{{"hard-coded secret", 1}}},
		{"json secret", `{"clientSecret": "` + randomValue + `"}`, []Finding{{"hard-coded secret", 1}}},
		{"several lines", awsKey + "\nok\n" + awsKey, []Finding{{"AWS access key", 1}, {"AWS access key", 3}}},
		{"allow marker", "id = " + awsKey + " // " + AllowMarker + "\n", nil},
		{"marker covers only its line", awsKey + " " + AllowMarker + "\n" + awsKey, []Finding{{"AWS access key", 2}}},
		{"placeholder", `api_key = "your-api-key-goes-here"`, nil},
		{"low entropy", `password = "aaaaaaaaaaaaaaaaaaaa"`, nil},
		{"short value", `password = "hunter2"`, nil},
		{"variable reference", `token = "${GITHUB_TOKEN_FROM_ENV}"`, nil},
		{"url value", `token = "https://example.com/some/long/path"`, nil},
		{"path value", `secret = "/run/secrets/database-password"`, nil},
		{"unquoted value", "token = " + randomValue, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Scan([]byte(tt.content)); !slices.Equal(got, tt.want) {
				t.Errorf("Scan = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScanCapsFindings(t *testing.T) {
	content := strings.Repeat(awsKey+"\n", maxFindings*3)
	if got := Scan([]byte(content)); len(got) != maxFindings {
		t.Errorf("got %d findings, want %d", len(got), maxFindings)
	}
}

func TestFindingString(t *testing.T) {
	if got := (Finding{Rule: "private key", Line: 4}).String(); got != "private key on line 4" {
		t.Errorf("String = %q", got)
	}
}

func BenchmarkScanLargeSource(b *testing.B) {
	block := strings.Join([]string{
		`func token(next string) string { return next + "value" }`,
		`key := cache["user"]; ok := token != ""`,
		`// a plain comment line without anything interesting in it`,
		`type Config struct { Name string; Retries int }`,
		``,
	}, "\n")
	data := []byte(strings.Repeat(block, 8000))
	b.SetBytes(int64(len(data)))
	for range b.N {
		Scan(data)
	}
}
