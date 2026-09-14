package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveWorkspaceRejectsEscape(t *testing.T) {
	root := t.TempDir()
	if _, err := resolveWorkspaceFile(root, "../secret"); err == nil {
		t.Fatal("expected escape error")
	}
	if _, err := resolveWorkspaceFile(root, "/etc/passwd"); err == nil {
		t.Fatal("expected abs path error")
	}
}

func TestListAndReadWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := listWorkspace(root, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	data, err := readWorkspaceFile(root, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hi" {
		t.Fatalf("data = %q", data)
	}
}

func TestSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readWorkspaceFile(root, "link"); err == nil {
		t.Fatal("expected symlink escape to fail")
	}
}
