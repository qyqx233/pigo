package main

import (
	"encoding/json"
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
	MetaFile  string
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
		MetaFile:  filepath.Join(root, "meta.json"),
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

func loadSessions(dataDir string) map[string]*managedSession {
	out := make(map[string]*managedSession)
	root := filepath.Join(dataDir, "sessions")
	entries, err := os.ReadDir(root)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		paths := newSessionPaths(dataDir, e.Name())
		meta, err := paths.loadMeta()
		if err != nil || meta.ID == "" {
			continue
		}
		_ = os.MkdirAll(paths.Run, 0o700)
		if recoverInterruptedTurn(&meta) {
			_ = paths.saveMeta(meta)
		}
		out[meta.ID] = &managedSession{paths: paths, meta: meta}
	}
	return out
}

func (p sessionPaths) saveMeta(meta sessionMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := p.MetaFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.MetaFile)
}

func (p sessionPaths) loadMeta() (sessionMeta, error) {
	data, err := os.ReadFile(p.MetaFile)
	if err != nil {
		return sessionMeta{}, err
	}
	var meta sessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return sessionMeta{}, err
	}
	return meta, nil
}

func (p sessionPaths) remove() error {
	return os.RemoveAll(p.Root)
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
