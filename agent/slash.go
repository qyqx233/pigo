package agent

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/smallnest/pigo/internal/cli"
	"github.com/smallnest/pigo/internal/cli/prompts"
	"github.com/smallnest/pigo/internal/cli/run"
	"github.com/smallnest/pigo/internal/provider"
	"github.com/smallnest/pigo/internal/runtime"
)

// SlashCommand describes one command available to an embedded Session. The
// list includes built-ins, prompt templates, skills, and plugin commands.
type SlashCommand struct {
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	ArgumentHint string `json:"argumentHint,omitempty"`
	Source       string `json:"source"`
}

// SlashResult is the result of resolving a slash command. Prompt is non-empty
// when the resolved command should continue through the normal agent loop;
// Message is status text that should be shown without treating it as a model
// response. Action is true when no agent run is required.
type SlashResult struct {
	Name    string
	Message string
	Prompt  string
	Action  bool
}

// SlashCommands returns the current command catalog. It is rebuilt on demand
// so newly installed prompt templates and skills appear without restarting the
// embedding process.
func (s *Session) SlashCommands() ([]SlashCommand, error) {
	registry, _, err := s.slashRegistry()
	if err != nil {
		return nil, err
	}
	commands := registry.List()
	result := make([]SlashCommand, 0, len(commands))
	for _, command := range commands {
		result = append(result, SlashCommand{
			Name:         command.Name,
			Description:  command.Description,
			ArgumentHint: command.ArgumentHint,
			Source:       command.Source.String(),
		})
	}
	return result, nil
}

// ResolveSlash resolves input when it starts with a slash. Non-command input
// returns handled=false. The Session must not be used concurrently while this
// method runs, matching the concurrency contract of Prompt and Stream.
func (s *Session) ResolveSlash(input string) (result SlashResult, handled bool, err error) {
	trimmed := strings.TrimLeft(input, " \t")
	if !strings.HasPrefix(trimmed, "/") {
		return SlashResult{}, false, nil
	}
	name := ""
	if fields := strings.Fields(trimmed); len(fields) > 0 {
		name = strings.TrimPrefix(fields[0], "/")
	}
	registry, live, err := s.slashRegistry()
	if err != nil {
		return SlashResult{}, true, err
	}
	outcome, err := registry.ResolveOutcome(input)
	if err != nil {
		return SlashResult{Name: name}, true, err
	}
	s.applyLiveConfig(live)
	return SlashResult{
		Name:    name,
		Message: outcome.Message,
		Prompt:  outcome.Prompt,
		Action:  outcome.Kind == runtime.SlashAction,
	}, true, nil
}

// ThinkingLevel returns the reasoning effort used for the next turn.
func (s *Session) ThinkingLevel() string {
	return string(s.runCfg.ThinkingLevel)
}

// Configure updates the model and/or reasoning effort used by subsequent
// turns. Empty values leave that setting unchanged. Both values are validated
// before either is applied, so a failed update never leaves a partial change.
func (s *Session) Configure(model, thinking string) error {
	live := &cli.LiveConfig{
		Model:         s.model,
		ProviderName:  s.env.ProviderName,
		Provider:      s.env.Provider,
		ThinkingLevel: s.runCfg.ThinkingLevel,
	}
	if model = strings.TrimSpace(model); model != "" && model != live.Model {
		resolved, providerName, err := provider.ResolveProvider(model, "", "", "", os.Getenv)
		if err != nil {
			return err
		}
		live.Model = model
		live.ProviderName = providerName
		live.Provider = resolved
	}
	if thinking = strings.TrimSpace(thinking); thinking != "" {
		level, err := run.ResolveThinkingLevel(thinking)
		if err != nil {
			return err
		}
		live.ThinkingLevel = level
	}
	s.applyLiveConfig(live)
	return nil
}

func (s *Session) slashRegistry() (*runtime.SlashRegistry, *cli.LiveConfig, error) {
	live := &cli.LiveConfig{
		Model:         s.model,
		ProviderName:  s.env.ProviderName,
		Provider:      s.env.Provider,
		ThinkingLevel: s.runCfg.ThinkingLevel,
	}
	registry, err := prompts.BuildSlashRegistry(live, s.creds, s.env.Skills, s.env.Plugins, prompts.PromptTemplateSources{
		ProjectDir:     filepath.Join(s.env.Cwd, ".pigo", "prompts"),
		ProjectTrusted: run.Trusted(s.env.Cwd),
	})
	return registry, live, err
}

func (s *Session) applyLiveConfig(live *cli.LiveConfig) {
	if live.Model != s.model || live.ProviderName != s.env.ProviderName {
		s.model = live.Model
		s.env.Provider = live.Provider
		s.env.ProviderName = live.ProviderName
		s.runCfg.Model = live.Model
		s.runCfg.Provider = live.ProviderName
		s.runCfg.Stream = provider.StreamFnFromProvider(live.Provider)
	}
	s.runCfg.ThinkingLevel = live.ThinkingLevel
}
