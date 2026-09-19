package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type sessionPaths struct {
	Root      string
	Workspace string
	Home      string
	PigoHome  string
	Run       string
}

func newSessionPaths(dataDir, id string) sessionPaths {
	root := filepath.Join(dataDir, "sessions", id)
	home := filepath.Join(root, "home")
	return sessionPaths{
		Root:      root,
		Workspace: filepath.Join(root, "workspace"),
		Home:      home,
		PigoHome:  filepath.Join(home, ".pigo"),
		Run:       filepath.Join(root, "run"),
	}
}

type sessionMeta struct {
	ID            string    `json:"id"`
	UserID        string    `json:"userId,omitempty"`
	Title         string    `json:"title,omitempty"`
	PigoSessionID string    `json:"pigoSessionId,omitempty"`
	Model         string    `json:"model"`
	Provider      string    `json:"provider"`
	Thinking      string    `json:"thinking"`
	Tools         []string  `json:"tools"`
	CreatedAt     time.Time `json:"createdAt"`
	LastUsed      time.Time `json:"lastUsed"`
	// ActiveTurn is set while a turn runs and cleared when it ends; one found
	// at startup is a turn the previous process never finished.
	ActiveTurn *turnRecord `json:"activeTurn,omitempty"`
	// LastTurn is how the most recent turn ended.
	LastTurn *turnRecord `json:"lastTurn,omitempty"`
}

func (p sessionPaths) create() error {
	for _, dir := range []string{p.Workspace, p.PigoHome, p.Run} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create session dir %s: %w", dir, err)
		}
	}
	return nil
}

func defaultDataDir() string {
	if dir := os.Getenv("PIGO_SERVER_DATA"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".pigo-server"
	}
	return filepath.Join(home, ".pigo-server")
}

// recoverInterruptedTurn turns a turn left running by a previous process into
// its record as interrupted. The steps it completed are in the transcript,
// which it saved after each one.
func recoverInterruptedTurn(meta *sessionMeta) bool {
	active := meta.ActiveTurn
	if active == nil {
		return false
	}
	now := time.Now().UTC()
	meta.LastTurn = &turnRecord{
		ID:        active.ID,
		StartedAt: active.StartedAt,
		EndedAt:   &now,
		Status:    "stopped",
		Reason:    turnInterrupted,
		Steps:     active.Steps,
		Message:   turnMessage(turnInterrupted, active.Steps, turnLimits{}),
	}
	meta.ActiveTurn = nil
	return true
}
