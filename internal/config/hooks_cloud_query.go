package config

// HooksConfig holds per-hook policy switches that affect what hooks may do.
// Today this covers only the UserPromptSubmit cloud-query opt-in, but the
// shape is reserved for future per-event policy (e.g. an opt-in for
// PreToolUse remote analysis, etc.). Keeping it nested under a HooksConfig
// stops the user/project config schema from growing a flat list of
// hook-event-specific top-level keys.
type HooksConfig struct {
	UserPromptSubmit *UserPromptSubmitHookConfig `yaml:"userpromptsubmit,omitempty" json:"userpromptsubmit,omitempty"`
}

// UserPromptSubmitHookConfig holds policy switches for the UserPromptSubmit
// hook (ox-tshf). Today only carries the cloud_query opt-in; future fields
// (e.g. injection budget, fail-open vs fail-closed) live here.
type UserPromptSubmitHookConfig struct {
	// CloudQuery opts the UserPromptSubmit handler into issuing a remote
	// SageOx query in PARALLEL with the always-on local query. Disabled by
	// default — when false, zero network calls happen on the prompt path
	// regardless of any other setting. Prompt content always passes through
	// the session.Redactor BEFORE any byte leaves the machine; the redaction
	// pipeline is non-negotiable, not a separate toggle.
	//
	// Tradeoff: enabling improves recall (server-side index sees more than
	// the local cached ledger), at the cost of sending redacted prompt
	// text to the configured SageOx endpoint. Default off preserves the
	// strictest privacy posture.
	CloudQuery *bool `yaml:"cloud_query,omitempty" json:"cloud_query,omitempty"`
}

// IsCloudQueryEnabled returns true when the user has explicitly opted in.
// Default false — never silently flips to true.
func (c *UserPromptSubmitHookConfig) IsCloudQueryEnabled() bool {
	if c == nil || c.CloudQuery == nil {
		return false
	}
	return *c.CloudQuery
}

// IsUserPromptSubmitCloudQueryEnabled is the convenience getter on HooksConfig.
func (c *HooksConfig) IsUserPromptSubmitCloudQueryEnabled() bool {
	if c == nil {
		return false
	}
	return c.UserPromptSubmit.IsCloudQueryEnabled()
}

// SetUserPromptSubmitCloudQuery sets the cloud_query opt-in flag, allocating
// nested structs as needed. Used by `ox config set`.
func (c *HooksConfig) SetUserPromptSubmitCloudQuery(enabled bool) {
	if c.UserPromptSubmit == nil {
		c.UserPromptSubmit = &UserPromptSubmitHookConfig{}
	}
	c.UserPromptSubmit.CloudQuery = &enabled
}

// ResolveUserPromptSubmitCloudQuery determines the effective cloud_query mode.
// Priority: User config > Project config > Default (false). This mirrors the
// resolution pattern used by ResolveRecordingReminder / ResolveSessionRecording.
//
// Returns false if config cannot be loaded — fail-closed is the only safe
// default for a privacy-sensitive switch.
func ResolveUserPromptSubmitCloudQuery(projectRoot string) bool {
	// Fail-closed on any config-load error. For a privacy switch this is
	// the only safe behavior — proceeding with partial state (e.g.,
	// missing user config but valid project config saying true) would
	// silently send prompt content to the cloud against the user's
	// intent. Default is the strictest privacy posture.
	userCfg, err := LoadUserConfig()
	if err != nil {
		return false
	}
	if userCfg != nil && userCfg.Hooks != nil && userCfg.Hooks.UserPromptSubmit != nil && userCfg.Hooks.UserPromptSubmit.CloudQuery != nil {
		return *userCfg.Hooks.UserPromptSubmit.CloudQuery
	}
	if projectRoot != "" {
		cfg, err := LoadProjectConfig(projectRoot)
		if err != nil {
			return false
		}
		if cfg != nil && cfg.Hooks != nil && cfg.Hooks.UserPromptSubmit != nil && cfg.Hooks.UserPromptSubmit.CloudQuery != nil {
			return *cfg.Hooks.UserPromptSubmit.CloudQuery
		}
	}
	return false
}
