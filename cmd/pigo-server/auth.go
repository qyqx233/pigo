package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	authCookieName = "pigo_auth"
	authLifetime   = 30 * 24 * time.Hour
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{3,32}$`)

// authStore keeps accounts and login tokens in memory for every request's
// authentication, backed by the users and auth_sessions tables. Each change is
// written to the database first and applied in memory only once that
// succeeded.
type authStore struct {
	mu    sync.Mutex
	db    *sqlDB
	state authState
}

type authState struct {
	Users    []userRecord  `json:"users"`
	Sessions []authSession `json:"sessions"`
}

type userRecord struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	UsernameKey  string    `json:"usernameKey"`
	PasswordHash string    `json:"passwordHash"`
	CreatedAt    time.Time `json:"createdAt"`
	// DisabledAt marks a suspended account: login is refused and existing
	// sessions were revoked when it was set. The account's data is kept, so
	// clearing it restores everything.
	DisabledAt *time.Time `json:"disabledAt,omitempty"`
}

type authSession struct {
	TokenHash string    `json:"tokenHash"`
	UserID    string    `json:"userId"`
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type publicUser struct {
	ID         string     `json:"id"`
	Username   string     `json:"username"`
	CreatedAt  time.Time  `json:"createdAt"`
	DisabledAt *time.Time `json:"disabledAt,omitempty"`
	// Admin is filled in by the handlers that know the roster, not by the
	// store: the role lives in the environment, not in auth.json.
	Admin bool `json:"admin,omitempty"`
}

type authRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type requestPrincipal struct {
	UserID    string
	Username  string
	CreatedAt time.Time
	Service   bool
	// Admin is resolved per request from the PIGO_ADMIN_USERS roster, never
	// from stored state (see admin.go).
	Admin bool
}

type principalContextKey struct{}

func newAuthStore(db *sqlDB) (*authStore, error) {
	store := &authStore{db: db}
	if err := db.query("SELECT id, username, username_key, password_hash, created_at, disabled_at FROM users ORDER BY created_at, id",
		func(rows *sql.Rows) error {
			var (
				u        userRecord
				created  int64
				disabled sql.NullInt64
			)
			if err := rows.Scan(&u.ID, &u.Username, &u.UsernameKey, &u.PasswordHash, &created, &disabled); err != nil {
				return err
			}
			u.CreatedAt = fromNanos(created)
			u.DisabledAt = fromNullNanos(disabled)
			store.state.Users = append(store.state.Users, u)
			return nil
		}); err != nil {
		return nil, fmt.Errorf("load users: %w", err)
	}
	now := time.Now().UTC()
	if _, err := db.exec("DELETE FROM auth_sessions WHERE expires_at <= ?", toNanos(now)); err != nil {
		return nil, fmt.Errorf("prune login tokens: %w", err)
	}
	if err := db.query("SELECT token_hash, user_id, created_at, expires_at FROM auth_sessions",
		func(rows *sql.Rows) error {
			var (
				a                authSession
				created, expires int64
			)
			if err := rows.Scan(&a.TokenHash, &a.UserID, &created, &expires); err != nil {
				return err
			}
			a.CreatedAt, a.ExpiresAt = fromNanos(created), fromNanos(expires)
			store.state.Sessions = append(store.state.Sessions, a)
			return nil
		}); err != nil {
		return nil, fmt.Errorf("load login tokens: %w", err)
	}
	return store, nil
}

func insertUser(e sqlExec, u userRecord) error {
	_, err := e.exec("INSERT INTO users (id, username, username_key, password_hash, created_at, disabled_at) VALUES (?, ?, ?, ?, ?, ?)",
		u.ID, u.Username, u.UsernameKey, u.PasswordHash, toNanos(u.CreatedAt), nullNanos(u.DisabledAt))
	return err
}

func insertAuthSession(e sqlExec, a authSession) error {
	_, err := e.exec("INSERT INTO auth_sessions (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)",
		a.TokenHash, a.UserID, toNanos(a.CreatedAt), toNanos(a.ExpiresAt))
	return err
}

func (s *authStore) register(username, password string) (publicUser, string, bool, error) {
	username = strings.TrimSpace(username)
	if !usernamePattern.MatchString(username) {
		return publicUser{}, "", false, errors.New("用户名须为 3-32 位字母、数字、点、下划线或短横线")
	}
	if len(password) < 8 || len(password) > 72 {
		return publicUser{}, "", false, errors.New("密码长度须为 8-72 个字节")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return publicUser{}, "", false, fmt.Errorf("hash password: %w", err)
	}
	id, err := randomID()
	if err != nil {
		return publicUser{}, "", false, err
	}
	rawToken, tokenHash, err := newAuthToken()
	if err != nil {
		return publicUser{}, "", false, err
	}
	now := time.Now().UTC()
	key := strings.ToLower(username)

	s.mu.Lock()
	defer s.mu.Unlock()
	firstUser := len(s.state.Users) == 0
	for _, existing := range s.state.Users {
		if existing.UsernameKey == key {
			return publicUser{}, "", false, errors.New("用户名已存在")
		}
	}
	user := userRecord{ID: id, Username: username, UsernameKey: key, PasswordHash: string(hash), CreatedAt: now}
	login := authSession{TokenHash: tokenHash, UserID: id, CreatedAt: now, ExpiresAt: now.Add(authLifetime)}
	if err := s.db.inTx(func(tx *sqlTx) error {
		if err := insertUser(tx, user); err != nil {
			return err
		}
		return insertAuthSession(tx, login)
	}); err != nil {
		return publicUser{}, "", false, fmt.Errorf("save account: %w", err)
	}
	s.state.Users = append(s.state.Users, user)
	s.state.Sessions = append(s.state.Sessions, login)
	return user.public(), rawToken, firstUser, nil
}

func (s *authStore) login(username, password string) (publicUser, string, error) {
	key := strings.ToLower(strings.TrimSpace(username))
	s.mu.Lock()
	var user userRecord
	for _, candidate := range s.state.Users {
		if candidate.UsernameKey == key {
			user = candidate
			break
		}
	}
	s.mu.Unlock()
	if user.ID == "" || bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) != nil {
		return publicUser{}, "", errors.New("用户名或密码错误")
	}
	// Reported only after the password checks out, so the message cannot be
	// used to enumerate which accounts exist.
	if user.DisabledAt != nil {
		return publicUser{}, "", errors.New("账号已被停用，请联系管理员")
	}
	rawToken, tokenHash, err := newAuthToken()
	if err != nil {
		return publicUser{}, "", err
	}
	now := time.Now().UTC()
	login := authSession{TokenHash: tokenHash, UserID: user.ID, CreatedAt: now, ExpiresAt: now.Add(authLifetime)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.db.inTx(func(tx *sqlTx) error {
		if _, err := tx.exec("DELETE FROM auth_sessions WHERE expires_at <= ?", toNanos(now)); err != nil {
			return err
		}
		return insertAuthSession(tx, login)
	}); err != nil {
		return publicUser{}, "", fmt.Errorf("save login: %w", err)
	}
	s.pruneExpiredLocked(now)
	s.state.Sessions = append(s.state.Sessions, login)
	return user.public(), rawToken, nil
}

func (s *authStore) authenticate(rawToken string) (publicUser, bool) {
	if rawToken == "" {
		return publicUser{}, false
	}
	tokenHash := hashAuthToken(rawToken)
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, login := range s.state.Sessions {
		if login.ExpiresAt.After(now) && subtle.ConstantTimeCompare([]byte(login.TokenHash), []byte(tokenHash)) == 1 {
			for _, user := range s.state.Users {
				if user.ID == login.UserID {
					// Defence in depth: disabling revokes sessions, but a token
					// issued in the same instant must not slip through.
					if user.DisabledAt != nil {
						return publicUser{}, false
					}
					return user.public(), true
				}
			}
		}
	}
	return publicUser{}, false
}

func (s *authStore) logout(rawToken string) error {
	tokenHash := hashAuthToken(rawToken)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.exec("DELETE FROM auth_sessions WHERE token_hash = ?", tokenHash); err != nil {
		return err
	}
	kept := s.state.Sessions[:0]
	for _, login := range s.state.Sessions {
		if subtle.ConstantTimeCompare([]byte(login.TokenHash), []byte(tokenHash)) != 1 {
			kept = append(kept, login)
		}
	}
	s.state.Sessions = kept
	return nil
}

func (s *authStore) pruneExpiredLocked(now time.Time) {
	kept := s.state.Sessions[:0]
	for _, login := range s.state.Sessions {
		if login.ExpiresAt.After(now) {
			kept = append(kept, login)
		}
	}
	s.state.Sessions = kept
}

func (u userRecord) public() publicUser {
	return publicUser{ID: u.ID, Username: u.Username, CreatedAt: u.CreatedAt, DisabledAt: u.DisabledAt}
}

func newAuthToken() (string, string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", err
	}
	token := hex.EncodeToString(raw[:])
	return token, hashAuthToken(token), nil
}

func hashAuthToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func setAuthCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    token,
		Path:     "/",
		Expires:  time.Now().Add(authLifetime).UTC(),
		MaxAge:   int(authLifetime.Seconds()),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearAuthCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Path:     "/",
		Expires:  time.Unix(1, 0).UTC(),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *apiServer) handleRegister(w http.ResponseWriter, r *http.Request) {
	var request authRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.config.token != "" && !s.validServiceToken(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="pigo-server-registration"`)
		writeError(w, http.StatusUnauthorized, "注册需要服务器 Token")
		return
	}
	// Self-service signup can be closed by an administrator. The service token
	// still gets through, so a closed deployment can still add accounts.
	if !s.settings.get().AllowRegistration && !s.validServiceToken(r) {
		writeError(w, http.StatusForbidden, "该部署已关闭注册")
		return
	}
	user, token, firstUser, err := s.auth.register(request.Username, request.Password)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "已存在") {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	if firstUser {
		s.claimLegacySessions(user.ID)
	}
	user.Admin = s.config.admins.has(user.Username)
	setAuthCookie(w, r, token)
	writeJSON(w, http.StatusCreated, user)
}

func (s *apiServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	var request authRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	user, token, err := s.auth.login(request.Username, request.Password)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	user.Admin = s.config.admins.has(user.Username)
	setAuthCookie(w, r, token)
	writeJSON(w, http.StatusOK, user)
}

func (s *apiServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(authCookieName); err == nil {
		_ = s.auth.logout(cookie.Value)
	}
	clearAuthCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *apiServer) handleMe(w http.ResponseWriter, r *http.Request) {
	principal := principalForRequest(r)
	if principal.UserID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, publicUser{
		ID:        principal.UserID,
		Username:  principal.Username,
		CreatedAt: principal.CreatedAt,
		Admin:     principal.Admin,
	})
}

func (s *apiServer) requirePrincipal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie(authCookieName); err == nil {
			if user, ok := s.auth.authenticate(cookie.Value); ok {
				ctx := withPrincipal(r, requestPrincipal{
					UserID:    user.ID,
					Username:  user.Username,
					CreatedAt: user.CreatedAt,
					Admin:     s.config.admins.has(user.Username),
				})
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}
		if s.validServiceToken(r) {
			ctx := withPrincipal(r, requestPrincipal{Service: true})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="pigo-server"`)
		writeError(w, http.StatusUnauthorized, "unauthorized")
	})
}

func (s *apiServer) validServiceToken(r *http.Request) bool {
	if s.config.token == "" {
		return false
	}
	provided, ok := bearerToken(r.Header.Get("Authorization"))
	return ok && subtle.ConstantTimeCompare([]byte(provided), []byte(s.config.token)) == 1
}

// withPrincipal returns r's context carrying p. requirePrincipal uses it on the
// real request path, and handler tests use it to exercise a specific role.
func withPrincipal(r *http.Request, p requestPrincipal) context.Context {
	return context.WithValue(r.Context(), principalContextKey{}, p)
}

func principalForRequest(r *http.Request) requestPrincipal {
	principal, ok := r.Context().Value(principalContextKey{}).(requestPrincipal)
	if !ok {
		// Handlers are also called directly by focused unit tests. Production
		// requests always pass through requirePrincipal.
		return requestPrincipal{Service: true}
	}
	return principal
}

func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// users returns every account, newest registration last. Used by the admin
// console; the records carry no secrets beyond what publicUser exposes.
func (s *authStore) users() []publicUser {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]publicUser, 0, len(s.state.Users))
	for _, user := range s.state.Users {
		out = append(out, user.public())
	}
	return out
}

// findUser looks one account up by id.
func (s *authStore) findUser(id string) (publicUser, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, user := range s.state.Users {
		if user.ID == id {
			return user.public(), true
		}
	}
	return publicUser{}, false
}

// revokeSessionsLocked drops a user's login tokens from memory. The caller
// holds mu and has already deleted them from the database.
func (s *authStore) revokeSessionsLocked(userID string) {
	kept := s.state.Sessions[:0]
	for _, login := range s.state.Sessions {
		if login.UserID != userID {
			kept = append(kept, login)
		}
	}
	s.state.Sessions = kept
}

// setDisabled suspends or restores an account. Suspending also revokes the
// user's sessions, so a browser that is already signed in stops working
// immediately rather than at token expiry.
func (s *authStore) setDisabled(userID string, disabled bool) (publicUser, error) {
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, user := range s.state.Users {
		if user.ID != userID {
			continue
		}
		next := user.DisabledAt
		if disabled && next == nil {
			next = &now
		} else if !disabled {
			next = nil
		}
		if err := s.db.inTx(func(tx *sqlTx) error {
			if _, err := tx.exec("UPDATE users SET disabled_at = ? WHERE id = ?", nullNanos(next), userID); err != nil {
				return err
			}
			if disabled {
				_, err := tx.exec("DELETE FROM auth_sessions WHERE user_id = ?", userID)
				return err
			}
			return nil
		}); err != nil {
			return publicUser{}, err
		}
		s.state.Users[i].DisabledAt = next
		if disabled {
			s.revokeSessionsLocked(userID)
		}
		return s.state.Users[i].public(), nil
	}
	return publicUser{}, errors.New("用户不存在")
}

// resetPassword replaces an account's password with a freshly generated one and
// revokes its sessions. The plaintext is returned to the caller exactly once —
// it is not stored anywhere and cannot be read back.
func (s *authStore) resetPassword(userID string) (string, error) {
	password, err := randomPassword()
	if err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for i, user := range s.state.Users {
		if user.ID != userID {
			continue
		}
		if err := s.db.inTx(func(tx *sqlTx) error {
			if _, err := tx.exec("UPDATE users SET password_hash = ? WHERE id = ?", string(hash), userID); err != nil {
				return err
			}
			_, err := tx.exec("DELETE FROM auth_sessions WHERE user_id = ?", userID)
			return err
		}); err != nil {
			return "", err
		}
		s.state.Users[i].PasswordHash = string(hash)
		s.revokeSessionsLocked(userID)
		return password, nil
	}
	return "", errors.New("用户不存在")
}

// deleteUser removes an account and its sessions. The caller is responsible for
// the data that lives outside this store (workspaces, credentials, models).
func (s *authStore) deleteUser(userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, user := range s.state.Users {
		if user.ID != userID {
			continue
		}
		if err := s.db.inTx(func(tx *sqlTx) error {
			if _, err := tx.exec("DELETE FROM users WHERE id = ?", userID); err != nil {
				return err
			}
			_, err := tx.exec("DELETE FROM auth_sessions WHERE user_id = ?", userID)
			return err
		}); err != nil {
			return err
		}
		s.state.Users = append(s.state.Users[:i], s.state.Users[i+1:]...)
		s.revokeSessionsLocked(userID)
		return nil
	}
	return errors.New("用户不存在")
}

// passwordAlphabet excludes characters that are easy to confuse when a
// temporary password is read off a screen and typed by hand (0/O, 1/l/I).
const passwordAlphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// randomPassword generates a 16-character temporary password with unbiased
// selection (rejection sampling would be needed for an alphabet that does not
// divide 256; 56 does not, so draw from a larger space and reduce with modulo
// over a rejection threshold).
func randomPassword() (string, error) {
	const length = 16
	out := make([]byte, 0, length)
	buf := make([]byte, length*2)
	// 256 % 56 == 32, so values at or above the threshold would bias the
	// distribution; they are discarded and redrawn.
	threshold := byte(256 - (256 % len(passwordAlphabet)))
	for len(out) < length {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if b >= threshold {
				continue
			}
			out = append(out, passwordAlphabet[int(b)%len(passwordAlphabet)])
			if len(out) == length {
				break
			}
		}
	}
	return string(out), nil
}
