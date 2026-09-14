package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const maxFileBytes = 10 << 20

var errPathEscape = errors.New("path is outside the session workspace")

type fileEntry struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size"`
}

func resolveWorkspaceFile(root, rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" || rel == "." {
		rel = "."
	}
	if filepath.IsAbs(rel) {
		return "", errPathEscape
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	full := filepath.Join(absRoot, rel)
	if err := withinRoot(absRoot, full); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		if os.IsNotExist(err) {
			return full, nil
		}
		return "", err
	}
	if err := withinRoot(absRoot, resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

func withinRoot(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errPathEscape
	}
	return nil
}

func listWorkspace(root, rel string) ([]fileEntry, error) {
	dir, err := resolveWorkspaceFile(root, rel)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]fileEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, fileEntry{Name: e.Name(), Dir: e.IsDir() || info.Mode()&fs.ModeDir != 0, Size: info.Size()})
	}
	return out, nil
}

func readWorkspaceFile(root, rel string) ([]byte, error) {
	path, err := resolveWorkspaceFile(root, rel)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmtDirError(rel)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxFileBytes {
		return nil, fmt.Errorf("file exceeds %d byte limit", maxFileBytes)
	}
	return data, nil
}

func fmtDirError(rel string) error {
	if rel == "" {
		rel = "."
	}
	return errors.New(rel + " is a directory")
}
