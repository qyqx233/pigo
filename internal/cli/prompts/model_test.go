package prompts

import (
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/cli"
	"github.com/smallnest/pigo/internal/runtime"
)

// TestModelCommandSwitchesToBareProviderName verifies /model zai selects the
// zai provider's default model (issue #564): live.Model carries the concrete
// preset id so the next turn's wire request targets a real model instead of
// sending the literal provider name to OpenRouter.
func TestModelCommandSwitchesToBareProviderName(t *testing.T) {
	live := &cli.LiveConfig{Model: "openrouter/free", ProviderName: "openrouter"}
	reg := runtime.NewSlashRegistry()
	RegisterLiveCommands(reg, live)

	out, err := reg.ResolveOutcome("/model zai")
	if err != nil {
		t.Fatalf("ResolveOutcome /model zai: %v", err)
	}
	if live.Model != "glm-4.7" {
		t.Errorf("live.Model = %q, want glm-4.7", live.Model)
	}
	if live.ProviderName != "zai" {
		t.Errorf("live.ProviderName = %q, want zai", live.ProviderName)
	}
	if !strings.Contains(out.Message, "glm-4.7") || !strings.Contains(out.Message, "zai") {
		t.Errorf("message = %q, want it to mention glm-4.7 (zai)", out.Message)
	}
}

// TestModelCommandConcreteIdUnchanged verifies a concrete model id switches
// verbatim, and a bare provider name without preset models reports the
// mismatch instead of silently falling back to OpenRouter.
func TestModelCommandConcreteIdUnchanged(t *testing.T) {
	live := &cli.LiveConfig{Model: "openrouter/free", ProviderName: "openrouter"}
	reg := runtime.NewSlashRegistry()
	RegisterLiveCommands(reg, live)

	out, err := reg.ResolveOutcome("/model glm-5.2")
	if err != nil {
		t.Fatalf("ResolveOutcome /model glm-5.2: %v", err)
	}
	if live.Model != "glm-5.2" || live.ProviderName != "zai" {
		t.Errorf("live = (%q, %q), want (glm-5.2, zai)", live.Model, live.ProviderName)
	}
	if !strings.Contains(out.Message, "glm-5.2") {
		t.Errorf("message = %q, want it to mention glm-5.2", out.Message)
	}

	// A concrete id for another provider switches verbatim as before.
	out, err = reg.ResolveOutcome("/model deepseek-v4-pro")
	if err != nil {
		t.Fatalf("ResolveOutcome /model deepseek-v4-pro: %v", err)
	}
	if live.Model != "deepseek-v4-pro" || live.ProviderName != "deepseek" {
		t.Errorf("live = (%q, %q), want (deepseek-v4-pro, deepseek)", live.Model, live.ProviderName)
	}
}
