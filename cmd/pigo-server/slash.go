package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/smallnest/pigo/internal/provider"
)

type slashCommand struct {
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	ArgumentHint string `json:"argumentHint,omitempty"`
	Source       string `json:"source"`
	Available    bool   `json:"available"`
}

func webCommands() []slashCommand {
	available := []slashCommand{
		{Name: "model", Description: "view or switch the active model: /model [model-id]", Source: "builtin", Available: true},
		{Name: "models", Description: "list preset providers and models", Source: "builtin", Available: true},
		{Name: "think", Description: "view or switch reasoning effort", ArgumentHint: "[off|minimal|low|medium|high|xhigh|max]", Source: "builtin", Available: true},
		{Name: "effect", Description: "alias of /think", ArgumentHint: "[off|minimal|low|medium|high|xhigh|max]", Source: "builtin", Available: true},
		{Name: "help", Description: "list available slash commands", Source: "builtin", Available: true},
	}
	unavailable := []string{
		"exit", "quit", "compact", "fork", "clone", "tree", "rewind",
		"export", "import", "copy", "session", "status", "goal", "btw",
		"dream", "remote-control", "memory", "rebuild",
	}
	out := append([]slashCommand(nil), available...)
	for _, name := range unavailable {
		out = append(out, slashCommand{
			Name:        name,
			Description: "TUI/REPL command not implemented in the Web server",
			Source:      "builtin",
			Available:   false,
		})
	}
	return out
}

func resolveWebInput(meta *sessionMeta, input string) (prompt, message string, complete bool, err error) {
	trimmed := strings.TrimSpace(input)
	if !strings.HasPrefix(trimmed, "/") {
		return input, "", false, nil
	}
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return input, "", false, nil
	}
	name := strings.TrimPrefix(fields[0], "/")
	args := strings.TrimSpace(strings.TrimPrefix(trimmed, fields[0]))

	switch name {
	case "help":
		return "", formatHelp(), true, nil
	case "models":
		return "", presetListing(args), true, nil
	case "model":
		msg, applyErr := applyModel(meta, args)
		return "", msg, true, applyErr
	case "think", "effect":
		msg, applyErr := applyThinking(meta, args)
		return "", msg, true, applyErr
	}

	for _, cmd := range webCommands() {
		if cmd.Name == name && !cmd.Available {
			return "", fmt.Sprintf("/%s 暂未在 Web 服务中实现，请在 pigo TUI/REPL 中使用。", name), true, nil
		}
	}
	// Unknown /foo is forwarded to headless pigo as a normal prompt.
	return input, "", false, nil
}

func formatHelp() string {
	var b strings.Builder
	b.WriteString("available commands:")
	for _, cmd := range webCommands() {
		if !cmd.Available {
			continue
		}
		fmt.Fprintf(&b, "\n  /%s", cmd.Name)
		if cmd.ArgumentHint != "" {
			fmt.Fprintf(&b, " %s", cmd.ArgumentHint)
		}
		if cmd.Description != "" {
			fmt.Fprintf(&b, " - %s", cmd.Description)
		}
	}
	return b.String()
}

func applyModel(meta *sessionMeta, id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Sprintf("model: %s (provider: %s)\nrun /models to see presets, or /model <id> to switch", meta.Model, meta.Provider), nil
	}
	_, providerName, err := provider.ResolveProvider(id, "", "", "", os.Getenv)
	if err != nil {
		return "", fmt.Errorf("model: cannot switch to %q: %w", id, err)
	}
	meta.Model = id
	meta.Provider = providerName
	return fmt.Sprintf("model switched to %s (provider: %s)", id, providerName), nil
}

func applyThinking(meta *sessionMeta, level string) (string, error) {
	level = strings.TrimSpace(level)
	if level == "" {
		cur := meta.Thinking
		if cur == "" {
			cur = "off"
		}
		return fmt.Sprintf("think: %s\nswitch with /think <off|minimal|low|medium|high|xhigh|max>", cur), nil
	}
	if !validThinking(level) {
		return "", fmt.Errorf("think: invalid level %q (want off|minimal|low|medium|high|xhigh|max)", level)
	}
	meta.Thinking = level
	return fmt.Sprintf("think level set to %s (applies to the next turn)", level), nil
}

func validThinking(level string) bool {
	switch level {
	case "off", "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}

func presetListing(filter string) string {
	filter = strings.ToLower(strings.TrimSpace(filter))
	var b strings.Builder
	b.WriteString("preset models:")
	count := 0
	for _, model := range provider.PresetCatalog {
		if filter != "" && !strings.EqualFold(model.Provider, filter) {
			continue
		}
		fmt.Fprintf(&b, "\n  %s  %s (%s)", model.ID, model.Label(), model.Provider)
		count++
	}
	if count == 0 {
		return "no presets match " + filter
	}
	b.WriteString("\nswitch with /model <id>")
	return b.String()
}
