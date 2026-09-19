// HTTP surface for provider API keys: a user's own keys, and the shared pool an
// administrator maintains.
//
// Every response here is derived from a key, never the key itself — see
// credentialInfoResponse. The write handlers return 503 when the server has no
// master secret, naming the variable to set, because "your key vanished" with
// no explanation is the worst possible failure for this feature.
package main

import (
	"errors"
	"net/http"
	"strings"

	"github.com/smallnest/pigo/internal/provider"
)

// credentialInfoResponse describes one provider slot for the UI. Hint is a
// masked form ("••••abcd") so a user can tell which key is stored without the
// server ever handing it back.
type credentialInfoResponse struct {
	Provider   string `json:"provider"`
	Configured bool   `json:"configured"`
	Hint       string `json:"hint,omitempty"`
}

type credentialWriteRequest struct {
	Key string `json:"key"`
}

// credentialsEnabledResponse wraps the list so a client can distinguish "no
// keys yet" from "this server cannot store keys at all".
type credentialsEnabledResponse struct {
	Enabled     bool                     `json:"enabled"`
	Reason      string                   `json:"reason,omitempty"`
	Credentials []credentialInfoResponse `json:"credentials"`
}

// knownProvider reports whether name is a provider this server can route to:
// a built-in, or an endpoint the administrator defined. Keys are only accepted
// for those; a typo would otherwise sit in the store looking configured and
// never be used.
func (s *apiServer) knownProvider(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, spec := range provider.ProviderSpecs() {
		if strings.EqualFold(spec.Name, name) {
			return true
		}
	}
	_, ok := s.settings.findCustomProvider(name)
	return ok
}

// listCredentials builds the response for one owner ("" is the shared pool).
func (s *apiServer) listCredentials(owner string) credentialsEnabledResponse {
	out := credentialsEnabledResponse{Enabled: s.credentials.enabled(), Credentials: []credentialInfoResponse{}}
	if !out.Enabled {
		out.Reason = errCredentialsDisabled.Error()
		return out
	}
	for _, name := range s.credentials.configuredProviders(owner) {
		out.Credentials = append(out.Credentials, credentialInfoResponse{
			Provider:   name,
			Configured: true,
			Hint:       s.credentials.hint(owner, name),
		})
	}
	return out
}

// writeCredentialError maps a store error onto a status code: a missing master
// secret is a server-configuration problem (503), anything else is a bad
// request.
func writeCredentialError(w http.ResponseWriter, err error) {
	if errors.Is(err, errCredentialsDisabled) {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}

// --- a user's own keys ------------------------------------------------------

func (s *apiServer) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	principal := principalForRequest(r)
	if principal.UserID == "" {
		writeJSON(w, http.StatusOK, s.listCredentials(""))
		return
	}
	writeJSON(w, http.StatusOK, s.listCredentials(principal.UserID))
}

func (s *apiServer) handleSetCredential(w http.ResponseWriter, r *http.Request) {
	principal := principalForRequest(r)
	if principal.UserID == "" {
		writeError(w, http.StatusForbidden, "服务令牌无法保存个人 API Key")
		return
	}
	if !s.settings.get().AllowUserKeys {
		writeError(w, http.StatusForbidden, "管理员已关闭用户自带 API Key")
		return
	}
	name := r.PathValue("provider")
	if !s.knownProvider(name) {
		writeError(w, http.StatusBadRequest, "未知的 provider: "+name)
		return
	}
	var request credentialWriteRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.credentials.setUserKey(principal.UserID, name, request.Key); err != nil {
		writeCredentialError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.listCredentials(principal.UserID))
}

func (s *apiServer) handleDeleteCredential(w http.ResponseWriter, r *http.Request) {
	principal := principalForRequest(r)
	if principal.UserID == "" {
		writeError(w, http.StatusForbidden, "服务令牌无个人 API Key")
		return
	}
	removed, err := s.credentials.deleteUserKey(principal.UserID, r.PathValue("provider"))
	if err != nil {
		writeCredentialError(w, err)
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "该 provider 未配置 API Key")
		return
	}
	writeJSON(w, http.StatusOK, s.listCredentials(principal.UserID))
}

// --- the shared pool (administrators) ---------------------------------------

func (s *apiServer) handleListPublicCredentials(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.listCredentials(""))
}

func (s *apiServer) handleSetPublicCredential(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	if !s.knownProvider(name) {
		writeError(w, http.StatusBadRequest, "未知的 provider: "+name)
		return
	}
	var request credentialWriteRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.credentials.setPublicKey(name, request.Key); err != nil {
		writeCredentialError(w, err)
		return
	}
	s.logAdminAction(r, "set public credential", name)
	writeJSON(w, http.StatusOK, s.listCredentials(""))
}

func (s *apiServer) handleDeletePublicCredential(w http.ResponseWriter, r *http.Request) {
	removed, err := s.credentials.deletePublicKey(r.PathValue("provider"))
	if err != nil {
		writeCredentialError(w, err)
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "该 provider 未配置公共 API Key")
		return
	}
	s.logAdminAction(r, "delete public credential", r.PathValue("provider"))
	writeJSON(w, http.StatusOK, s.listCredentials(""))
}
