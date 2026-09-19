package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// builtinToolNames is the default pigo tool set advertised when -tools all.
// It matches internal/cli/run.BuiltinTools (task/memory are added inside the
// sandbox by pigo itself and are not listed here).
var builtinToolNames = []string{
	"read", "write", "edit", "grep", "find",
	"bash", "bash_output", "kill_bash",
	"todo", "webfetch", "websearch",
}

// Sandbox launches one headless pigo turn inside bubblewrap.
type Sandbox struct {
	Bwrap string
	Pigo  string
}

// Ready reports whether bwrap and pigo binaries exist. Missing either is
// fail-closed: the orchestrator must not exec pigo on the host cwd.
func (s Sandbox) Ready() error {
	if strings.TrimSpace(s.Bwrap) == "" {
		return fmt.Errorf("bwrap binary is not configured")
	}
	if _, err := os.Stat(s.Bwrap); err != nil {
		return fmt.Errorf("bwrap: %w", err)
	}
	return nil
}

// RunSpec is one headless pigo invocation, already rooted at a session directory.
type RunSpec struct {
	Workspace    string
	Home         string
	Prompt       string
	ResumeID     string
	Model        string
	Provider     string
	Thinking     string
	NoTools      bool
	AllowedTools []string
	NoSkills     bool
	SkillsHost   string
	Env          []string
	// PigoBin overrides the in-sandbox binary path (tests). Empty means /tmp/pigo.
	PigoBin string
}

// Command builds `bwrap … -- pigo …` for spec. It never bind-mounts the host
// home directory. Env is cleared inside the sandbox and only spec.Env plus a
// small fixed set (HOME, PATH, PIGO_HOME) are re-injected.
func (s Sandbox) Command(ctx context.Context, spec RunSpec) (*exec.Cmd, error) {
	if err := s.Ready(); err != nil {
		return nil, err
	}
	argv, err := s.argv(spec)
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, argv[0], argv[1:]...), nil
}

func (s Sandbox) mountArgs(spec RunSpec) ([]string, error) {
	if spec.Workspace == "" || spec.Home == "" {
		return nil, fmt.Errorf("sandbox: workspace and home are required")
	}
	args := []string{
		s.Bwrap,
		"--unshare-pid", "--unshare-ipc", "--unshare-uts",
		"--new-session",
		"--die-with-parent",
		"--clearenv",
		"--tmpfs", "/tmp",
		"--proc", "/proc",
	}
	args = appendRoBinds(args, []string{
		"/usr", "/bin", "/lib", "/lib64",
		"/etc/ssl", "/etc/resolv.conf", "/etc/nsswitch.conf", "/etc/hosts", "/etc/passwd",
	})
	args = appendDevBinds(args, []string{"/dev/null", "/dev/zero", "/dev/urandom", "/dev/random"})
	args = append(args,
		"--bind", spec.Workspace, "/workspace",
		"--bind", spec.Home, "/home/pigo",
		"--setenv", "HOME", "/home/pigo",
		"--setenv", "PIGO_HOME", "/home/pigo/.pigo",
		"--setenv", "PATH", "/tmp:/usr/local/bin:/usr/bin:/bin",
		"--setenv", "USER", "pigo",
		"--setenv", "TERM", "dumb",
		"--chdir", "/workspace",
	)
	if !spec.NoSkills {
		if dir := strings.TrimSpace(spec.SkillsHost); dir != "" {
			if st, err := os.Stat(dir); err == nil && st.IsDir() {
				args = append(args, "--dir", "/home/pigo/.agents", "--ro-bind", dir, "/home/pigo/.agents/skills")
			}
		}
	}
	for _, kv := range spec.Env {
		key, val, ok := strings.Cut(kv, "=")
		if !ok || key == "" || key == "HOME" || key == "PIGO_HOME" {
			continue
		}
		args = append(args, "--setenv", key, val)
	}
	return args, nil
}

func (s Sandbox) argv(spec RunSpec) ([]string, error) {
	args, err := s.mountArgs(spec)
	if err != nil {
		return nil, err
	}
	args = append(args, "--", "/tmp/pigo")
	args = append(args, pigoArgs(spec)...)
	return args, nil
}

func (s Sandbox) liveArgv(spec RunSpec) ([]string, error) {
	args, err := s.mountArgs(spec)
	if err != nil {
		return nil, err
	}
	args = append(args, "--", "/bin/sh")
	return args, nil
}

func pigoArgs(spec RunSpec) []string {
	args := []string{
		"--approve",
		"-C", "/workspace",
		"--output-format", "stream-json",
		"--no-prompt-templates",
	}
	if spec.NoSkills {
		args = append(args, "--no-skills")
	}
	if spec.Model != "" {
		args = append(args, "-m", spec.Model)
	}
	if spec.Provider != "" {
		args = append(args, "--provider", spec.Provider)
	}
	if spec.Thinking != "" {
		args = append(args, "--thinking-level", spec.Thinking)
	}
	if spec.ResumeID != "" {
		args = append(args, "--resume", spec.ResumeID)
	}
	if spec.NoTools {
		args = append(args, "--no-tools")
	} else if len(spec.AllowedTools) > 0 {
		args = append(args, "--allowed-tools", strings.Join(spec.AllowedTools, ","))
	}
	args = append(args, "-p", spec.Prompt)
	return args
}

func appendRoBinds(args, hosts []string) []string {
	for _, host := range hosts {
		if _, err := os.Stat(host); err != nil {
			continue
		}
		args = append(args, "--ro-bind", host, host)
	}
	return args
}

func appendDevBinds(args, devices []string) []string {
	for _, dev := range devices {
		if _, err := os.Stat(dev); err != nil {
			continue
		}
		args = append(args, "--dev-bind", dev, dev)
	}
	return args
}

func skillsDir() string {
	if dir := strings.TrimSpace(os.Getenv("PIGO_SKILLS_DIR")); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".agents", "skills")
}

func findBwrap() (string, error) {
	if p := strings.TrimSpace(os.Getenv("PIGO_SERVER_BWRAP")); p != "" {
		return filepath.Abs(p)
	}
	p, err := exec.LookPath("bwrap")
	if err != nil {
		return "", fmt.Errorf("bwrap not found on PATH (install bubblewrap)")
	}
	return p, nil
}

func configuredTools(toolsFlag string) (noTools bool, names []string) {
	items := splitList(toolsFlag)
	switch {
	case len(items) == 0:
		return true, nil
	case len(items) == 1 && strings.EqualFold(items[0], "all"):
		return false, append([]string(nil), builtinToolNames...)
	default:
		return false, items
	}
}
