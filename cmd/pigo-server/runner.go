package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
)

// CommandBuilder starts the process for one turn. Production uses
// Sandbox.Command; tests inject a fake pigo without bwrap.
type CommandBuilder func(ctx context.Context, spec RunSpec) (*exec.Cmd, error)

type streamJSONEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	Text      string `json:"text"`
	ToolName  string `json:"toolName"`
	ToolCall  string `json:"toolCallId"`
	IsError   bool   `json:"isError"`
	Error     string `json:"error"`
}

type turnResult struct {
	Text          string
	PigoSessionID string
}

func runTurn(ctx context.Context, spec RunSpec, build CommandBuilder, emit func(streamEvent)) (turnResult, error) {
	if build == nil {
		return turnResult{}, fmt.Errorf("sandbox command builder is not configured")
	}
	cmd, err := build(ctx, spec)
	if err != nil {
		return turnResult{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return turnResult{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return turnResult{}, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return turnResult{}, fmt.Errorf("start sandbox: %w", err)
	}

	errCh := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(stderr)
		errCh <- strings.TrimSpace(string(data))
	}()

	// Read stdout to EOF before Wait; Wait closes the pipes.
	stop := context.AfterFunc(ctx, func() { killProcessGroup(cmd) })
	result, parseErr := parseStreamJSON(stdout, emit)
	stop()

	waitErr := cmd.Wait()
	stderrText := <-errCh
	if ctx.Err() != nil {
		waitErr = ctx.Err()
	}

	if parseErr != nil {
		return result, parseErr
	}
	if waitErr != nil {
		if stderrText != "" {
			return result, fmt.Errorf("pigo: %v: %s", waitErr, truncate(stderrText, 2048))
		}
		return result, fmt.Errorf("pigo: %w", waitErr)
	}
	if result.Text == "" && stderrText != "" {
		return result, fmt.Errorf("pigo: %s", truncate(stderrText, 2048))
	}
	return result, nil
}

func parseStreamJSON(r io.Reader, emit func(streamEvent)) (turnResult, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var result turnResult
	var seen string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev streamJSONEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "agent_start":
			if ev.SessionID != "" {
				result.PigoSessionID = ev.SessionID
			}
		case "message_update":
			if ev.Text == "" {
				continue
			}
			delta := ev.Text
			if strings.HasPrefix(ev.Text, seen) {
				delta = ev.Text[len(seen):]
			}
			seen = ev.Text
			result.Text = ev.Text
			if delta != "" && emit != nil {
				emit(streamEvent{Type: "delta", Text: delta})
			}
		case "turn_end":
			if ev.Text != "" {
				result.Text = ev.Text
				if strings.HasPrefix(ev.Text, seen) {
					if delta := ev.Text[len(seen):]; delta != "" && emit != nil {
						emit(streamEvent{Type: "delta", Text: delta})
					}
				}
				seen = ev.Text
			}
		case "tool_execution_start":
			if emit != nil {
				emit(streamEvent{Type: "tool", Tool: ev.ToolName, Phase: "start", ID: ev.ToolCall})
			}
		case "tool_execution_end":
			if emit != nil {
				emit(streamEvent{Type: "tool", Tool: ev.ToolName, Phase: "end", ID: ev.ToolCall, IsError: ev.IsError})
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return result, err
	}
	return result, nil
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
