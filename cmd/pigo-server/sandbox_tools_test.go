package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeToolchain builds a small self-contained toolchain: bin/ with an
// executable, an internal relative symlink, and optionally a python3 and the
// package list the build script writes.
func fakeToolchain(t *testing.T, python bool) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tool")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "hello"), []byte("#!/bin/sh\necho hello-from-toolchain\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hello", filepath.Join(root, "bin", "hi")); err != nil {
		t.Fatal(err)
	}
	if python {
		if err := os.WriteFile(filepath.Join(root, "bin", "python3"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "PIGO-PACKAGES.txt"), []byte("# built\npandas==3.0.1\npip @ file:///build/pip-26.1.2-py3-none-any.whl#sha256=38\npdfplumber==0.11.7\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestResolveSandboxTools(t *testing.T) {
	py := fakeToolchain(t, true)
	node := fakeToolchain(t, false)
	tools, err := resolveSandboxTools(toolSpecs(" python="+py+" ", []string{"node=" + node}))
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[0].Name != "python" || tools[0].Mount != "/opt/python" || !tools[0].Python ||
		strings.Join(tools[0].Packages, ",") != "pandas,pip,pdfplumber" || tools[1].Python {
		t.Fatalf("tools = %+v", tools)
	}
}

func TestResolveSandboxToolsRefuses(t *testing.T) {
	good := fakeToolchain(t, false)

	absolute := fakeToolchain(t, false)
	if err := os.Symlink("/usr/bin/env", filepath.Join(absolute, "bin", "env")); err != nil {
		t.Fatal(err)
	}
	outside := fakeToolchain(t, false)
	if err := os.Symlink("../../..", filepath.Join(outside, "bin", "up")); err != nil {
		t.Fatal(err)
	}
	noBin := t.TempDir()
	home, _ := os.UserHomeDir()

	cases := []struct {
		name  string
		specs []string
		want  string
	}{
		{"no equals", []string{"python"}, "name=dir"},
		{"bad name", []string{"Py thon=" + good}, "lowercase"},
		{"twice", []string{"a=" + good, "a=" + good}, "twice"},
		{"missing dir", []string{"a=" + filepath.Join(good, "nope")}, "no such file"},
		{"no bin", []string{"a=" + noBin}, "no bin/"},
		{"root", []string{"a=/"}, "refusing to mount /"},
		{"home", []string{"a=" + home}, "home directory"},
		{"absolute symlink", []string{"a=" + absolute}, "absolute symlink"},
		{"symlink outside", []string{"a=" + outside}, "points outside"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveSandboxTools(tc.specs)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestSandboxToolArgs(t *testing.T) {
	tools := []sandboxTool{
		{Name: "python", HostDir: "/srv/py", Mount: "/opt/python", Python: true, Packages: []string{"pandas"}},
		{Name: "node", HostDir: "/srv/node", Mount: "/opt/node"},
	}
	argv, err := (Sandbox{Bwrap: "/usr/bin/bwrap", Tools: tools}).liveArgv(RunSpec{Workspace: "/ws", Home: "/h", NoSkills: true})
	if err != nil {
		t.Fatal(err)
	}
	if !containsTriple(argv, "--ro-bind", "/srv/py", "/opt/python") || !containsTriple(argv, "--ro-bind", "/srv/node", "/opt/node") {
		t.Errorf("mounts missing: %v", argv)
	}
	if !containsTriple(argv, "--setenv", "PATH", "/opt/python/bin:/opt/node/bin:/tmp:/usr/local/bin:/usr/bin:/bin") {
		t.Errorf("PATH wrong: %v", argv)
	}
	if !containsTriple(argv, "--setenv", "PYTHONDONTWRITEBYTECODE", "1") {
		t.Error("PYTHONDONTWRITEBYTECODE not set with a Python toolchain")
	}
	plain, _ := (Sandbox{Bwrap: "/usr/bin/bwrap", Tools: tools[1:]}).liveArgv(RunSpec{Workspace: "/ws", Home: "/h", NoSkills: true})
	if containsPair(plain, "--setenv", "PYTHONDONTWRITEBYTECODE") {
		t.Error("PYTHONDONTWRITEBYTECODE set without a Python toolchain")
	}
	if none, _ := (Sandbox{Bwrap: "/usr/bin/bwrap"}).liveArgv(RunSpec{Workspace: "/ws", Home: "/h", NoSkills: true}); !containsTriple(none, "--setenv", "PATH", "/tmp:/usr/local/bin:/usr/bin:/bin") {
		t.Error("PATH changed with no toolchain")
	}

	note := toolPromptNote(tools)
	for _, want := range []string{"/opt/python/bin", "pandas", "pip install", "/opt/node/bin"} {
		if !strings.Contains(note, want) {
			t.Errorf("prompt note lacks %q:\n%s", want, note)
		}
	}
	if toolPromptNote(nil) != "" {
		t.Error("a note without toolchains")
	}
}

// TestSandboxToolInBwrap mounts a toolchain into a real sandbox: its commands
// are on PATH, and the mount cannot be written to.
func TestSandboxToolInBwrap(t *testing.T) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bwrap not available")
	}
	tools, err := resolveSandboxTools([]string{"fake=" + fakeToolchain(t, false)})
	if err != nil {
		t.Fatal(err)
	}
	box := Sandbox{Bwrap: bwrap, Tools: tools}
	args, err := box.mountArgs(RunSpec{Workspace: t.TempDir(), Home: t.TempDir(), NoSkills: true})
	if err != nil {
		t.Fatal(err)
	}
	args = append(args, "--", "/bin/sh", "-c", `hello; hi; touch /opt/fake/x 2>/dev/null && echo WRITABLE || echo read-only`)
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got := string(out); got != "hello-from-toolchain\nhello-from-toolchain\nread-only\n" {
		t.Errorf("output = %q", got)
	}
}
