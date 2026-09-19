// Toolchains mounted into every sandbox: self-contained directories (a Python
// environment built by sandbox-python/build.sh, a node or go release …) that
// are bind-mounted read-only at /opt/<name>, with their bin/ put first on PATH.
//
// A toolchain must be relocatable — it is mounted at a path other than its
// own — so a directory with a symlink that is absolute or points outside it
// (a venv, which links to its base interpreter) is refused at startup rather
// than failing inside the sandbox. So is a directory too broad to expose: the
// root, or a home directory.
//
// Configured with -sandbox-tool name=dir (repeatable) or PIGO_SANDBOX_TOOLS
// (comma-separated). See spec/sandbox-toolchains.md.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// sandboxTool is one validated toolchain.
type sandboxTool struct {
	Name    string `json:"name"`
	HostDir string `json:"-"`
	// Mount is where the sandbox sees it: /opt/<name>.
	Mount string `json:"mount"`
	// Python is set when the toolchain has bin/python3.
	Python bool `json:"python,omitempty"`
	// Packages lists a Python toolchain's installed packages, from the
	// PIGO-PACKAGES.txt the build script writes; empty when there is none.
	Packages []string `json:"-"`
}

var toolNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// toolFlag collects repeated -sandbox-tool values.
type toolFlag []string

func (f *toolFlag) String() string { return strings.Join(*f, ",") }

func (f *toolFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

// toolSpecs gathers the configured entries: PIGO_SANDBOX_TOOLS first, then the
// flags.
func toolSpecs(env string, flags []string) []string {
	var out []string
	for _, item := range strings.Split(env, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return append(out, flags...)
}

// resolveSandboxTools parses and checks every entry. Any problem is an error:
// a toolchain the administrator configured and silently did not get is harder
// to diagnose than a server that refuses to start.
func resolveSandboxTools(specs []string) ([]sandboxTool, error) {
	var out []sandboxTool
	seen := map[string]bool{}
	for _, spec := range specs {
		name, dir, ok := strings.Cut(spec, "=")
		name, dir = strings.TrimSpace(name), strings.TrimSpace(dir)
		if !ok || name == "" || dir == "" {
			return nil, fmt.Errorf("sandbox tool %q: expected name=dir", spec)
		}
		if !toolNamePattern.MatchString(name) {
			return nil, fmt.Errorf("sandbox tool %q: the name must be lowercase letters, digits and dashes", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("sandbox tool %q is configured twice", name)
		}
		seen[name] = true
		tool, err := checkToolDir(name, dir)
		if err != nil {
			return nil, err
		}
		out = append(out, tool)
	}
	return out, nil
}

// checkToolDir validates one toolchain directory.
func checkToolDir(name, dir string) (sandboxTool, error) {
	fail := func(format string, args ...any) (sandboxTool, error) {
		return sandboxTool{}, fmt.Errorf("sandbox tool %s (%s): %s", name, dir, fmt.Sprintf(format, args...))
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fail("%v", err)
	}
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fail("%v", err)
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return fail("not a directory")
	}
	if err := refuseBroadDir(root); err != nil {
		return fail("%v", err)
	}
	if st, err := os.Stat(filepath.Join(root, "bin")); err != nil || !st.IsDir() {
		return fail("no bin/ directory")
	}
	if bad, err := firstNonPortableLink(root); err != nil {
		return fail("%v", err)
	} else if bad != "" {
		return fail("%s; the directory must be self-contained to be mounted at /opt/%s (a venv is not: build a standalone environment with cmd/pigo-server/sandbox-python/build.sh)", bad, name)
	}
	tool := sandboxTool{Name: name, HostDir: root, Mount: "/opt/" + name}
	if st, err := os.Stat(filepath.Join(root, "bin", "python3")); err == nil && !st.IsDir() {
		tool.Python = true
		tool.Packages = readPackageList(filepath.Join(root, "PIGO-PACKAGES.txt"))
	}
	return tool, nil
}

// refuseBroadDir rejects directories whose exposure would defeat the sandbox:
// the root, and a home directory (where keys and credentials live).
func refuseBroadDir(dir string) error {
	if dir == "/" {
		return errors.New("refusing to mount /")
	}
	if filepath.Dir(dir) == "/home" || dir == "/root" {
		return fmt.Errorf("refusing to mount the home directory %s", dir)
	}
	if home, err := os.UserHomeDir(); err == nil {
		if real, err := filepath.EvalSymlinks(home); err == nil && (dir == real || dir == home) {
			return fmt.Errorf("refusing to mount the home directory %s", dir)
		}
	}
	return nil
}

// firstNonPortableLink finds a symlink that would break once the directory is
// mounted elsewhere: an absolute one, or one resolving outside the directory.
// It returns "" when there is none.
func firstNonPortableLink(root string) (string, error) {
	var bad string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if filepath.IsAbs(target) {
			bad = fmt.Sprintf("%s is an absolute symlink (-> %s)", rel, target)
			return fs.SkipAll
		}
		resolved := filepath.Clean(filepath.Join(filepath.Dir(path), target))
		if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
			bad = fmt.Sprintf("%s points outside the directory (-> %s)", rel, target)
			return fs.SkipAll
		}
		return nil
	})
	return bad, err
}

// readPackageList reads package names from a PIGO-PACKAGES.txt: pip-freeze
// lines, "name==version" or, for a package installed from a file,
// "name @ url"; "#" starts a comment.
func readPackageList(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, _, _ := strings.Cut(line, "==")
		name, _, _ = strings.Cut(name, " @ ")
		out = append(out, strings.TrimSpace(name))
	}
	return out
}

// toolMountArgs are the bwrap arguments that mount the toolchains.
func toolMountArgs(tools []sandboxTool) []string {
	var args []string
	for _, t := range tools {
		args = append(args, "--ro-bind", t.HostDir, t.Mount)
	}
	for _, t := range tools {
		if t.Python {
			// The mount is read-only: without this every import tries, and
			// fails, to write a .pyc next to its source.
			args = append(args, "--setenv", "PYTHONDONTWRITEBYTECODE", "1")
			break
		}
	}
	return args
}

// toolPath is the sandbox PATH with each toolchain's bin/ first, in the
// configured order.
func toolPath(tools []sandboxTool, base string) string {
	parts := make([]string, 0, len(tools)+1)
	for _, t := range tools {
		parts = append(parts, t.Mount+"/bin")
	}
	return strings.Join(append(parts, base), ":")
}

// toolPromptNote tells the model what the sandbox offers beyond the system,
// for the system prompt. Empty when no toolchain is mounted.
func toolPromptNote(tools []sandboxTool) string {
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n# Sandbox toolchains\n\n")
	for _, t := range tools {
		fmt.Fprintf(&b, "- %s: %s/bin (first on PATH)", t.Name, t.Mount)
		if t.Python {
			b.WriteString(". `python3` is this interpreter")
			if len(t.Packages) > 0 {
				fmt.Fprintf(&b, ", with these packages preinstalled: %s", strings.Join(t.Packages, ", "))
			}
			b.WriteString(". Prefer them over writing your own parsers. To add a package, run `pip install <package>`: the environment is read-only, so it installs into this session's home and lasts for this session only")
		}
		b.WriteString(".\n")
	}
	return b.String()
}

// sandboxToolsResponse is the toolchain list as reported (never nil, so it
// encodes as []).
func sandboxToolsResponse(tools []sandboxTool) []sandboxTool {
	if tools == nil {
		return []sandboxTool{}
	}
	return tools
}
