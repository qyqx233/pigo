// The administrator's user console: listing accounts, suspending them,
// resetting a password, and deleting an account outright.
//
// Two rules shape every handler here:
//
//   - An administrator may not suspend, delete or reset themselves. The roster
//     lives in the environment, so locking yourself out is recoverable, but a
//     console that lets you delete the account you are signed in as is a footgun
//     with no upside.
//   - Deleting an account touches five stores. The cascade below keeps going
//     after a failure and reports what it could not clean up, because a
//     half-deleted account that says so is easier to finish by hand than one
//     that aborted halfway and claimed nothing happened.
package main

import (
	"net/http"
	"strings"
)

// adminUserResponse is one row of the user console. It carries no password
// material and no API keys — only which providers the account has keys for, so
// an administrator can see who would be affected by deleting it.
type adminUserResponse struct {
	publicUser
	Admin       bool     `json:"admin"`
	Disabled    bool     `json:"disabled"`
	Sessions    int      `json:"sessions"`
	Credentials []string `json:"credentials,omitempty"`
}

// disableRequest is the body of the suspend/restore call.
type disableRequest struct {
	Disabled bool `json:"disabled"`
}

// passwordResetResponse carries the generated password. It is the only response
// in the server that contains a usable secret, and the value exists nowhere
// else: it is hashed on the way into the store and never recoverable after.
type passwordResetResponse struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// deleteUserResponse reports the outcome of a cascade. Problems is empty on a
// clean delete; otherwise it names what is left behind.
type deleteUserResponse struct {
	Username string   `json:"username"`
	Sessions int      `json:"sessions"`
	Problems []string `json:"problems,omitempty"`
}

// sessionCount returns how many live sessions an account owns.
func (s *apiServer) sessionCount(userID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, managed := range s.sessions {
		managed.mu.Lock()
		if !managed.closed && managed.meta.UserID == userID {
			n++
		}
		managed.mu.Unlock()
	}
	return n
}

func (s *apiServer) handleListUsers(w http.ResponseWriter, _ *http.Request) {
	users := s.auth.users()
	out := make([]adminUserResponse, 0, len(users))
	for _, user := range users {
		out = append(out, adminUserResponse{
			publicUser:  user,
			Admin:       s.config.admins.has(user.Username),
			Disabled:    user.DisabledAt != nil,
			Sessions:    s.sessionCount(user.ID),
			Credentials: s.credentials.configuredProviders(user.ID),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// targetUser resolves the {id} path value, refusing the caller's own account.
func (s *apiServer) targetUser(w http.ResponseWriter, r *http.Request) (publicUser, bool) {
	id := strings.TrimSpace(r.PathValue("id"))
	user, ok := s.auth.findUser(id)
	if !ok {
		writeError(w, http.StatusNotFound, "用户不存在")
		return publicUser{}, false
	}
	if principal := principalForRequest(r); principal.UserID != "" && principal.UserID == user.ID {
		writeError(w, http.StatusBadRequest, "不能对自己的账号执行该操作")
		return publicUser{}, false
	}
	return user, true
}

func (s *apiServer) handleSetUserDisabled(w http.ResponseWriter, r *http.Request) {
	user, ok := s.targetUser(w, r)
	if !ok {
		return
	}
	var request disableRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := s.auth.setDisabled(user.ID, request.Disabled)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	action := "enable user"
	if request.Disabled {
		action = "disable user"
		// A suspended account keeps its data but must stop consuming resources.
		s.closeUserSessions(user.ID, false)
	}
	s.logAdminAction(r, action, user.Username)
	writeJSON(w, http.StatusOK, updated)
}

func (s *apiServer) handleResetUserPassword(w http.ResponseWriter, r *http.Request) {
	user, ok := s.targetUser(w, r)
	if !ok {
		return
	}
	password, err := s.auth.resetPassword(user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The password itself is deliberately absent from this log line.
	s.logAdminAction(r, "reset password", user.Username)
	writeJSON(w, http.StatusOK, passwordResetResponse{Username: user.Username, Password: password})
}

func (s *apiServer) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	user, ok := s.targetUser(w, r)
	if !ok {
		return
	}
	var problems []string

	// 1. Stop anything running, and remove the session directories with it. A
	//    sandbox left alive would keep writing into a workspace we are deleting.
	sessions, sessionProblems := s.closeUserSessions(user.ID, true)
	problems = append(problems, sessionProblems...)

	// 2. The account itself, with its sessions.
	if err := s.auth.deleteUser(user.ID); err != nil {
		problems = append(problems, "删除账号: "+err.Error())
	}

	// 3. Stored provider keys.
	if err := s.credentials.dropUser(user.ID); err != nil {
		problems = append(problems, "删除 API Key: "+err.Error())
	}

	// 4. Personal model entries. The shared catalog is untouched.
	if s.customModels != nil {
		if err := s.customModels.dropUser(user.ID); err != nil {
			problems = append(problems, "删除自定义模型: "+err.Error())
		}
	}

	// The usage ledger is deliberately left alone: it is the record of what
	// was spent, and reports show the calls under the deleted user's name.

	s.logAdminAction(r, "delete user", user.Username)
	writeJSON(w, http.StatusOK, deleteUserResponse{
		Username: user.Username,
		Sessions: sessions,
		Problems: problems,
	})
}

// closeUserSessions stops a user's live sandboxes. With removeData set it also
// deletes each session's directory and drops it from the map; otherwise the
// sessions are only parked and can be resumed if the account is restored.
//
// It returns how many sessions it acted on and any problems, and never stops
// early: one workspace that refuses to delete must not strand the rest.
func (s *apiServer) closeUserSessions(userID string, removeData bool) (int, []string) {
	s.mu.Lock()
	var targets []*managedSession
	for id, managed := range s.sessions {
		managed.mu.Lock()
		owned := managed.meta.UserID == userID
		managed.mu.Unlock()
		if !owned {
			continue
		}
		targets = append(targets, managed)
		if removeData {
			delete(s.sessions, id)
		}
	}
	s.mu.Unlock()

	var problems []string
	for _, managed := range targets {
		managed.mu.Lock()
		if removeData {
			managed.closed = true
		}
		managed.stopLive()
		paths := managed.paths
		managed.mu.Unlock()
		if removeData {
			if err := paths.remove(); err != nil {
				problems = append(problems, "删除工作区 "+paths.Root+": "+err.Error())
			}
		}
	}
	return len(targets), problems
}
