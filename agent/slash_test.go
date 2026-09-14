package agent

import "testing"

func TestResolveSlashChangesThinkingLevel(t *testing.T) {
	session, err := New(WithoutTools())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer session.Close()

	result, handled, err := session.ResolveSlash("/think low")
	if err != nil {
		t.Fatalf("ResolveSlash: %v", err)
	}
	if !handled || !result.Action {
		t.Fatalf("result = %+v, handled = %v; want handled action", result, handled)
	}
	if got := session.ThinkingLevel(); got != "low" {
		t.Fatalf("ThinkingLevel = %q, want low", got)
	}
}

func TestResolveSlashRejectsEmptyCommand(t *testing.T) {
	session, err := New(WithoutTools())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer session.Close()

	if _, handled, err := session.ResolveSlash("/"); !handled || err == nil {
		t.Fatalf("handled = %v, err = %v; want handled error", handled, err)
	}
}

func TestSlashCommandsIncludeLiveBuiltins(t *testing.T) {
	session, err := New(WithoutTools())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer session.Close()

	commands, err := session.SlashCommands()
	if err != nil {
		t.Fatalf("SlashCommands: %v", err)
	}
	found := false
	for _, command := range commands {
		if command.Name == "model" && command.Source == "builtin" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("SlashCommands does not include /model")
	}
}

func TestConfigureChangesModelAndThinking(t *testing.T) {
	session, err := New(WithoutTools())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer session.Close()

	if err := session.Configure("ollama/llama3.2", "high"); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if got := session.Model(); got != "ollama/llama3.2" {
		t.Fatalf("Model = %q", got)
	}
	if got := session.Provider(); got != "ollama" {
		t.Fatalf("Provider = %q", got)
	}
	if got := session.ThinkingLevel(); got != "high" {
		t.Fatalf("ThinkingLevel = %q", got)
	}
}

func TestConfigureIsAtomic(t *testing.T) {
	session, err := New(WithoutTools())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer session.Close()
	originalModel := session.Model()

	if err := session.Configure("ollama/llama3.2", "impossible"); err == nil {
		t.Fatal("Configure accepted an invalid thinking level")
	}
	if got := session.Model(); got != originalModel {
		t.Fatalf("Model changed after failed update: got %q, want %q", got, originalModel)
	}
}
