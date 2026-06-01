package session

import (
	"strings"
	"testing"
)

// TestRedactPrompt_StripsCommonSecrets verifies the prompt-mode wrapper
// redacts the full DefaultPatterns catalog so cloud-query payloads
// (ox-8wmo) never carry secrets off-machine.
func TestRedactPrompt_StripsCommonSecrets(t *testing.T) {
	r := NewRedactor()

	cases := []struct {
		name     string
		prompt   string
		wantOut  string // substring expected in output
		wantGone string // substring expected NOT in output (the raw secret)
		wantHit  bool   // expect at least one pattern hit
	}{
		{
			name:     "github_personal_access_token",
			prompt:   "please debug this: ghp_1234567890abcdefghijklmnopqrstuvwxyz",
			wantOut:  "[REDACTED_GITHUB_TOKEN]",
			wantGone: "ghp_1234567890abcdefghijklmnopqrstuvwxyz",
			wantHit:  true,
		},
		{
			name:     "aws_access_key",
			prompt:   "creds AKIAIOSFODNN7EXAMPLE leaked",
			wantOut:  "[REDACTED_AWS_KEY]",
			wantGone: "AKIAIOSFODNN7EXAMPLE",
			wantHit:  true,
		},
		{
			name:     "dot_env_block",
			prompt:   "from .env:\nGITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123456789",
			wantOut:  "[REDACTED_GITHUB_TOKEN]",
			wantGone: "ghp_abcdefghijklmnopqrstuvwxyz0123456789",
			wantHit:  true,
		},
		{
			name:    "no_secrets",
			prompt:  "where is the function that handles login?",
			wantOut: "where is the function that handles login?",
			wantHit: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, count := r.RedactPrompt(tc.prompt)
			if tc.wantHit && count == 0 {
				t.Fatalf("expected redaction hit, got count=0; out=%q", out)
			}
			if !tc.wantHit && count != 0 {
				t.Fatalf("expected no redaction, got count=%d; out=%q", count, out)
			}
			if tc.wantOut != "" && !strings.Contains(out, tc.wantOut) {
				t.Fatalf("expected %q in output, got %q", tc.wantOut, out)
			}
			if tc.wantGone != "" && strings.Contains(out, tc.wantGone) {
				t.Fatalf("raw secret leaked through redactor: %q still in %q", tc.wantGone, out)
			}
		})
	}
}

// TestRedactPrompt_EmptyInput verifies the wrapper handles the trivial
// empty case without misbehaving.
func TestRedactPrompt_EmptyInput(t *testing.T) {
	r := NewRedactor()
	out, count := r.RedactPrompt("")
	if out != "" {
		t.Fatalf("expected empty output, got %q", out)
	}
	if count != 0 {
		t.Fatalf("expected count=0, got %d", count)
	}
}
