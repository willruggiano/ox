package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sageox/ox/internal/auth"
	"github.com/sageox/ox/internal/config"
	"github.com/sageox/ox/internal/endpoint"
	"gopkg.in/yaml.v3"
)

// withIsolatedConfig points OX_USER_CONFIG, XDG_*_HOME, and the auth client
// at a fresh temp directory so the test never reads or writes the developer's
// real ~/.sageox state.
func withIsolatedConfig(t *testing.T) (configDir string, userCfgPath string) {
	t.Helper()
	tmp := t.TempDir()
	configDir = filepath.Join(tmp, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	userCfgPath = filepath.Join(configDir, "config.yaml")

	t.Setenv("OX_USER_CONFIG", userCfgPath)
	// auth package + config package both resolve via paths.ConfigDir(),
	// which honors XDG_CONFIG_HOME → $XDG_CONFIG_HOME/sageox. Point each
	// of the XDG dirs at tmp so any sageox/* subdir lands inside this test.
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))
	t.Setenv("HOME", tmp)
	// the actual auth file path becomes <tmp>/sageox/auth.json; ensure the
	// parent exists so SaveTokenForEndpoint succeeds.
	if err := os.MkdirAll(filepath.Join(tmp, "sageox"), 0o755); err != nil {
		t.Fatalf("mkdir sageox: %v", err)
	}

	return configDir, userCfgPath
}

func writeUserConfigYAML(t *testing.T, path string, cfg *config.UserConfig) error {
	t.Helper()
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// writeFakeToken drops a non-expired StoredToken for the current endpoint so
// the auth gate in PrepareCloudQuery passes WITHOUT a network call. The
// values are intentionally garbage — IsAuthCredentialValidForEndpoint only
// checks expiry, never server-side validity.
func writeFakeToken(t *testing.T, _ string) {
	t.Helper()
	tok := &auth.StoredToken{
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		ExpiresAt:    time.Now().Add(1 * time.Hour),
	}
	if err := auth.SaveTokenForEndpoint(endpoint.Get(), tok); err != nil {
		t.Fatalf("save fake token: %v", err)
	}
}

// TestPrepareCloudQuery_DefaultConfigNeverQueries is the load-bearing
// integration test for the "default config makes zero network calls"
// invariant of ox-8wmo. With no config at all, ShouldQuery MUST be false
// — verifiably so by the absence of any network call attempt.
func TestPrepareCloudQuery_DefaultConfigNeverQueries(t *testing.T) {
	withIsolatedConfig(t)

	decision := PrepareCloudQuery(context.Background(), "", "find the login handler")

	if decision.ShouldQuery {
		t.Fatalf("default config must not opt into cloud query; got ShouldQuery=true")
	}
	if decision.RedactedPrompt != "" {
		t.Fatalf("default-off path should not invoke redactor; got RedactedPrompt=%q", decision.RedactedPrompt)
	}
}

// TestPrepareCloudQuery_OptInWithoutTokenDegradesSilently covers the
// no-auth degradation path: cloud_query=true but `ox login` has not run.
// MUST return ShouldQuery=false WITHOUT raising an error — the prompt
// path can never break because of auth state.
func TestPrepareCloudQuery_OptInWithoutTokenDegradesSilently(t *testing.T) {
	_, userCfgPath := withIsolatedConfig(t)

	// opt in via user config — but write no auth.json
	cfg := &config.UserConfig{}
	enabled := true
	cfg.Hooks = &config.HooksConfig{
		UserPromptSubmit: &config.UserPromptSubmitHookConfig{
			CloudQuery: &enabled,
		},
	}
	if err := writeUserConfigYAML(t, userCfgPath, cfg); err != nil {
		t.Fatalf("write user config: %v", err)
	}

	decision := PrepareCloudQuery(context.Background(), "", "find the login handler")

	if decision.ShouldQuery {
		t.Fatalf("opt-in WITHOUT token must degrade silently; got ShouldQuery=true")
	}
}

// TestPrepareCloudQuery_RedactsBeforeTransmit verifies the redaction
// pipeline runs before any byte would leave the machine. Pasted secrets
// MUST be scrubbed from RedactedPrompt, and the raw secret must NEVER
// appear in the decision payload.
func TestPrepareCloudQuery_RedactsBeforeTransmit(t *testing.T) {
	configDir, userCfgPath := withIsolatedConfig(t)

	cfg := &config.UserConfig{}
	enabled := true
	cfg.Hooks = &config.HooksConfig{
		UserPromptSubmit: &config.UserPromptSubmitHookConfig{
			CloudQuery: &enabled,
		},
	}
	if err := writeUserConfigYAML(t, userCfgPath, cfg); err != nil {
		t.Fatalf("write user config: %v", err)
	}

	// drop a fake non-expired token so the auth gate passes
	writeFakeToken(t, configDir)

	const pastedSecret = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	prompt := "help me debug, here is my token: " + pastedSecret

	decision := PrepareCloudQuery(context.Background(), "", prompt)

	if !decision.ShouldQuery {
		t.Fatalf("expected ShouldQuery=true with opt-in + token; got false")
	}
	if decision.RedactedTokens == 0 {
		t.Fatalf("expected redactor to report at least 1 hit; got 0")
	}
	if strings.Contains(decision.RedactedPrompt, pastedSecret) {
		t.Fatalf("raw secret leaked into RedactedPrompt: %q", decision.RedactedPrompt)
	}
	if !strings.Contains(decision.RedactedPrompt, "[REDACTED_GITHUB_TOKEN]") {
		t.Fatalf("expected redaction marker in output, got %q", decision.RedactedPrompt)
	}
}

// TestPrepareCloudQuery_TimeoutBudget verifies the 100ms inheritance from
// the local-query budget so a slow remote can never delay the prompt path.
func TestPrepareCloudQuery_TimeoutBudget(t *testing.T) {
	withIsolatedConfig(t)
	decision := PrepareCloudQuery(context.Background(), "", "anything")
	if decision.CloudQueryTimeout.Milliseconds() > 100 {
		t.Fatalf("cloud query timeout must be <=100ms; got %v", decision.CloudQueryTimeout)
	}
}
