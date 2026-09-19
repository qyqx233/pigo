// This file introduces the administrator role.
//
// Administrators are named by the PIGO_ADMIN_USERS environment variable rather
// than by a field in auth.json, and the role is evaluated per request. Two
// consequences are deliberate: write access to the data directory cannot grant
// anyone the role, and there is no schema migration for existing deployments.
// The cost is that changing the roster requires a restart.
//
// An empty roster means the deployment has no administrators at all — the
// server behaves exactly as it did before this feature, with public
// configuration coming only from the process environment. Startup logging says
// so explicitly, because a silent "no admin console" is indistinguishable from
// a broken build.
package main

import (
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"
)

// adminRoster is the set of usernames granted the administrator role, keyed the
// same way authStore keys usernames (lowercased) so the comparison matches how
// accounts are deduplicated at registration.
type adminRoster map[string]struct{}

// parseAdminRoster reads a comma-separated PIGO_ADMIN_USERS value. Blank
// entries are skipped so trailing commas and "a, ,b" are harmless.
func parseAdminRoster(value string) adminRoster {
	roster := adminRoster{}
	for _, name := range strings.Split(value, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "" {
			roster[name] = struct{}{}
		}
	}
	return roster
}

// has reports whether username is an administrator. A name that has not been
// registered yet still matches: the roster pre-authorizes, so whoever claims
// that username later holds the role from their first request.
func (r adminRoster) has(username string) bool {
	if len(r) == 0 {
		return false
	}
	_, ok := r[strings.ToLower(strings.TrimSpace(username))]
	return ok
}

// names returns the roster sorted, for the startup log.
func (r adminRoster) names() []string {
	out := make([]string, 0, len(r))
	for name := range r {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// describe renders the roster for the startup banner.
func (r adminRoster) describe() string {
	if len(r) == 0 {
		return "none (set PIGO_ADMIN_USERS to enable the admin console)"
	}
	return strings.Join(r.names(), ", ")
}

// requireAdmin wraps a handler so only administrators reach it. It runs inside
// requirePrincipal, which has already rejected anonymous requests.
//
// The service token (PIGO_SERVER_TOKEN) counts as an administrator: it is the
// deployment's own credential, held by whoever runs the process, and is already
// trusted to create accounts.
func (s *apiServer) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if principal := principalForRequest(r); principal.Admin || principal.Service {
			next.ServeHTTP(w, r)
			return
		}
		writeError(w, http.StatusForbidden, "需要管理员权限")
	})
}

// logAdminAction records one administrative write: who did what, to which
// target. It is a log line rather than a stored audit trail — enough to answer
// "who changed the public key" from the server's own output, without adding a
// file whose retention nobody has decided. Targets are provider names,
// usernames and model ids; secrets never reach here.
func (s *apiServer) logAdminAction(r *http.Request, action, target string) {
	principal := principalForRequest(r)
	actor := principal.Username
	if actor == "" {
		actor = "service-token"
	}
	log.Printf("admin: %s %s %s", actor, action, target)
}

// handleGetSettings returns the deployment-wide settings.
func (s *apiServer) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.settings.get())
}

// handleUpdateSettings replaces them.
func (s *apiServer) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var request serverSettings
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := s.settings.update(request)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logAdminAction(r, "update settings", "")
	writeJSON(w, http.StatusOK, updated)
}

// handleListCustomProviders returns the administrator-defined endpoints.
func (s *apiServer) handleListCustomProviders(w http.ResponseWriter, _ *http.Request) {
	out := s.settings.customProviders()
	if out == nil {
		out = []customProvider{}
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePutCustomProvider adds or replaces one. Replacing keeps any stored key,
// which is what makes this the "edit the address" path as well as the "add" one.
func (s *apiServer) handlePutCustomProvider(w http.ResponseWriter, r *http.Request) {
	var request customProvider
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	next, err := request.validate()
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.settings.putCustomProvider(next); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logAdminAction(r, "put custom provider", next.Name)
	writeJSON(w, http.StatusOK, s.settings.customProviders())
}

// handleDeleteCustomProvider drops an endpoint and the keys filed under it —
// leaving the credentials behind would strand ciphertext nothing can reach.
func (s *apiServer) handleDeleteCustomProvider(w http.ResponseWriter, r *http.Request) {
	name := strings.ToLower(strings.TrimSpace(r.PathValue("name")))
	removed, err := s.settings.removeCustomProvider(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "该自定义 provider 不存在")
		return
	}
	if _, err := s.credentials.deletePublicKey(name); err != nil && !errors.Is(err, errCredentialsDisabled) {
		log.Printf("admin: dropping public key for removed provider %s: %v", name, err)
	}
	s.logAdminAction(r, "delete custom provider", name)
	writeJSON(w, http.StatusOK, s.settings.customProviders())
}
