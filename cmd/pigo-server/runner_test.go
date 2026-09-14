package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseStreamJSONDeltasAndTools(t *testing.T) {
	input := strings.NewReader(strings.Join([]string{
		`{"type":"agent_start","sessionId":"sid-9"}`,
		`{"type":"tool_execution_start","toolName":"bash","toolCallId":"c1"}`,
		`{"type":"tool_execution_end","toolName":"bash","toolCallId":"c1","isError":false}`,
		`{"type":"message_update","text":"Hel"}`,
		`{"type":"message_update","text":"Hello"}`,
		`{"type":"turn_end","stopReason":"end_turn","text":"Hello"}`,
		`{"type":"agent_end","messageCount":1}`,
	}, "\n") + "\n")

	var events []streamEvent
	got, err := parseStreamJSON(input, func(ev streamEvent) { events = append(events, ev) })
	if err != nil {
		t.Fatal(err)
	}
	if got.PigoSessionID != "sid-9" || got.Text != "Hello" {
		t.Fatalf("result = %+v", got)
	}
	var deltas []string
	var tools []string
	for _, ev := range events {
		if ev.Type == "delta" {
			deltas = append(deltas, ev.Text)
		}
		if ev.Type == "tool" {
			tools = append(tools, ev.Phase+":"+ev.Tool)
		}
	}
	if strings.Join(deltas, "") != "Hello" {
		t.Fatalf("deltas = %v", deltas)
	}
	if strings.Join(tools, ",") != "start:bash,end:bash" {
		t.Fatalf("tools = %v", tools)
	}
}

func TestRunTurnFakePigo(t *testing.T) {
	bin := buildFakePigo(t)
	dir := t.TempDir()
	ws := filepath.Join(dir, "workspace")
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".pigo"), 0o700); err != nil {
		t.Fatal(err)
	}
	build := func(ctx context.Context, spec RunSpec) (*exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, bin, pigoArgs(spec)...)
		cmd.Dir = spec.Workspace
		cmd.Env = []string{"HOME=" + spec.Home, "PIGO_HOME=" + filepath.Join(spec.Home, ".pigo")}
		return cmd, nil
	}
	got, err := runTurn(context.Background(), RunSpec{Workspace: ws, Home: home, Prompt: "world", NoTools: true, NoSkills: true}, build, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.PigoSessionID != "fake-sid-1" || got.Text != "hello:world" {
		t.Fatalf("got %+v", got)
	}

	got, err = runTurn(context.Background(), RunSpec{Workspace: ws, Home: home, Prompt: "again", ResumeID: got.PigoSessionID, NoTools: true, NoSkills: true}, build, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "resumed:again" {
		t.Fatalf("resume text = %q", got.Text)
	}
}

func buildFakePigo(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "pigo")
	cmd := exec.Command("go", "build", "-o", out, "./testdata/fake_pigo.go")
	cmd.Dir = "."
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake pigo: %v\n%s", err, data)
	}
	return out
}
