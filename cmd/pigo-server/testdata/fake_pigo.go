// Fake headless pigo used by pigo-server tests. It speaks the same stream-json
// envelope the real CLI emits, without contacting a model.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func main() {
	if os.Getenv("FAKE_PIGO_FAIL") == "1" {
		fmt.Fprintln(os.Stderr, "missing API key")
		os.Exit(1)
	}
	resume, prompt := "", ""
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--resume", "-r":
			i++
			if i < len(args) {
				resume = args[i]
			}
		case "-p", "--print":
			i++
			if i < len(args) {
				prompt = args[i]
			}
		}
	}
	sid := resume
	if sid == "" {
		sid = "fake-sid-1"
	}
	text := "hello:" + prompt
	if resume != "" {
		text = "resumed:" + prompt
	}
	if prompt == "dump-env" {
		text = "HOME=" + os.Getenv("HOME")
	}
	if strings.HasPrefix(prompt, "stat:") {
		path := strings.TrimPrefix(prompt, "stat:")
		if _, err := os.Stat(path); err == nil {
			text = "visible"
		} else {
			text = "hidden"
		}
	}
	emit := func(v map[string]any) {
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
	}
	emit(map[string]any{"type": "agent_start", "sessionId": sid})
	emit(map[string]any{"type": "tool_execution_start", "toolName": "read", "toolCallId": "t1"})
	emit(map[string]any{"type": "tool_execution_end", "toolName": "read", "toolCallId": "t1", "isError": false})
	emit(map[string]any{"type": "message_update", "text": text})
	emit(map[string]any{"type": "turn_end", "stopReason": "end_turn", "text": text})
	emit(map[string]any{"type": "agent_end", "messageCount": 1})
}
