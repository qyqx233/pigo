// Environment variables an administrator injects into every session's sandbox
// container. The container starts with --clearenv, so without this nothing of
// the host's environment reaches a command run there — which is what keeps
// provider keys out, and also what leaves tools with no HTTPS_PROXY on a
// network that needs one.
//
// These are not secrets: anything here is readable with `env` by any user who
// can run a command in their own session. The panel says so.
//
// See spec/sandbox-toolchains.md.
package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// sandboxEnvVar is one variable as the administrator sets it.
type sandboxEnvVar struct {
	Name      string    `json:"name"`
	Value     string    `json:"value"`
	UpdatedBy string    `json:"updatedBy,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

const maxSandboxEnvValue = 4096

var sandboxEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

// reservedSandboxEnv are the variables the sandbox sets itself. PATH carries
// the tool mounts, HOME and PIGO_HOME root the session, and the loader
// variables would change what every command in the container executes.
var reservedSandboxEnv = map[string]bool{
	"HOME": true, "PIGO_HOME": true, "PATH": true,
	"LD_PRELOAD": true, "LD_LIBRARY_PATH": true, "LD_AUDIT": true,
}

func (v sandboxEnvVar) validate() (sandboxEnvVar, error) {
	v.Name = strings.TrimSpace(v.Name)
	v.Value = strings.TrimSpace(v.Value)
	switch {
	case !sandboxEnvNamePattern.MatchString(v.Name):
		return v, errors.New("变量名只能是字母、数字和下划线，且不以数字开头")
	case reservedSandboxEnv[strings.ToUpper(v.Name)]:
		return v, fmt.Errorf("%s 由沙箱自己设置，不能覆盖", strings.ToUpper(v.Name))
	case v.Value == "":
		return v, errors.New("值不能为空；不需要就删除这一条")
	case len(v.Value) > maxSandboxEnvValue:
		return v, fmt.Errorf("值超过 %d 字符", maxSandboxEnvValue)
	case strings.ContainsAny(v.Value, "\x00\n\r"):
		return v, errors.New("值不能包含换行或空字符")
	}
	return v, nil
}

// sandboxEnv returns the configured variables, by name.
func (s *settingsStore) sandboxEnv() []sandboxEnvVar {
	// An empty list, never nil: it is JSON for a page that iterates it.
	out := append([]sandboxEnvVar{}, s.get().SandboxEnv...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// sandboxEnvPairs is the same list in the "K=V" form RunSpec takes.
func (s *settingsStore) sandboxEnvPairs() []string {
	vars := s.sandboxEnv()
	out := make([]string, 0, len(vars))
	for _, v := range vars {
		out = append(out, v.Name+"="+v.Value)
	}
	return out
}

// putSandboxEnv adds or replaces one variable.
func (s *settingsStore) putSandboxEnv(next sandboxEnvVar) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := append([]sandboxEnvVar(nil), s.settings.SandboxEnv...)
	replaced := false
	for i, v := range s.settings.SandboxEnv {
		if v.Name == next.Name {
			s.settings.SandboxEnv[i] = next
			replaced = true
			break
		}
	}
	if !replaced {
		s.settings.SandboxEnv = append(s.settings.SandboxEnv, next)
	}
	if err := s.saveLocked(); err != nil {
		s.settings.SandboxEnv = previous
		return err
	}
	return nil
}

// removeSandboxEnv drops one variable, reporting whether it existed.
func (s *settingsStore) removeSandboxEnv(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]sandboxEnvVar, 0, len(s.settings.SandboxEnv))
	for _, v := range s.settings.SandboxEnv {
		if v.Name != name {
			kept = append(kept, v)
		}
	}
	if len(kept) == len(s.settings.SandboxEnv) {
		return false, nil
	}
	previous := s.settings.SandboxEnv
	s.settings.SandboxEnv = kept
	if err := s.saveLocked(); err != nil {
		s.settings.SandboxEnv = previous
		return false, err
	}
	return true, nil
}

// --- HTTP -----------------------------------------------------------------------

// sandboxEnvResponse carries the list and how many sessions still run on the
// previous one: a container keeps the environment it started with.
type sandboxEnvResponse struct {
	Vars        []sandboxEnvVar `json:"vars"`
	RunningOld  int             `json:"runningOld"`
	ProcessHint []string        `json:"processHint,omitempty"`
}

// proxyHint offers the server's own proxy setting as a starting value: the
// deployment that needs one in the sandbox usually needs the same one.
func proxyHint() []string {
	var out []string
	for _, name := range []string{"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY"} {
		if v := strings.TrimSpace(lookupProxyEnv(name)); v != "" {
			out = append(out, name+"="+v)
		}
	}
	return out
}

// lookupProxyEnv reads the server's own setting, in either spelling.
func lookupProxyEnv(name string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return os.Getenv(strings.ToLower(name))
}

func (s *apiServer) sandboxEnvResponse() sandboxEnvResponse {
	return sandboxEnvResponse{Vars: s.settings.sandboxEnv(), RunningOld: s.liveSandboxCount(), ProcessHint: proxyHint()}
}

// liveSandboxCount is how many session containers are running. They keep the
// environment they started with, so a change reaches them when they restart.
func (s *apiServer) liveSandboxCount() int {
	s.mu.RLock()
	all := make([]*managedSession, 0, len(s.sessions))
	for _, managed := range s.sessions {
		all = append(all, managed)
	}
	s.mu.RUnlock()
	n := 0
	for _, managed := range all {
		if managed.liveAlive() {
			n++
		}
	}
	return n
}

func (s *apiServer) handleSandboxEnv(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.sandboxEnvResponse())
}

func (s *apiServer) handlePutSandboxEnv(w http.ResponseWriter, r *http.Request) {
	var request sandboxEnvVar
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	next, err := request.validate()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	next.UpdatedBy = principalForRequest(r).Username
	next.UpdatedAt = time.Now().UTC()
	if err := s.settings.putSandboxEnv(next); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The name only: a value may be a token, and admin actions are logged.
	s.logAdminAction(r, "put sandbox env", next.Name)
	writeJSON(w, http.StatusOK, s.sandboxEnvResponse())
}

func (s *apiServer) handleDeleteSandboxEnv(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	removed, err := s.settings.removeSandboxEnv(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "没有这个变量")
		return
	}
	s.logAdminAction(r, "delete sandbox env", name)
	writeJSON(w, http.StatusOK, s.sandboxEnvResponse())
}
