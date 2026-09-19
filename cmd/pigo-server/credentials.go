// This file stores provider API keys for the server: the public pool an
// administrator configures, and the personal keys individual users bring.
//
// Threat model and the shape it forces:
//
//   - Keys are encrypted at rest with AES-256-GCM. The key-encryption key is
//     derived with HKDF-SHA256 from PIGO_SERVER_SECRET, which arrives through
//     the environment and therefore does not live next to the ciphertext in the
//     data directory. Losing the secret means losing the stored keys; leaking it
//     means losing all of them. That is the accepted trade of choosing
//     server-side storage over per-browser storage.
//   - Each record is sealed with additional data binding it to its owner and
//     provider, so a ciphertext copied from one slot to another fails to open
//     rather than silently authenticating as someone else.
//   - Plaintext leaves this file in exactly one direction: resolve() hands it to
//     the provider credential store, which puts it in an outgoing request
//     header. No read API returns it, no log line prints it, and the HTTP
//     responses carry only "configured" plus the last four characters.
//
// Degradation: when PIGO_SERVER_SECRET is unset the store starts in a disabled
// mode. Writes are refused with a clear reason, reads report nothing
// configured, and an existing credentials.json is never read, rewritten or
// removed — so setting the secret later restores the data untouched. Everything
// else about the server keeps working.
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// credentialSecretEnv names the environment variable holding the master secret.
const credentialSecretEnv = "PIGO_SERVER_SECRET"

// credentialKeyInfo is the HKDF info string. It scopes the derived key to this
// use, so a future feature deriving from the same secret cannot produce the
// same key.
const credentialKeyInfo = "pigo-server/credentials/v1"

// errCredentialsDisabled is returned by every write path when the master secret
// is missing. The message is user-facing: it names the variable to set.
var errCredentialsDisabled = errors.New("服务端未配置 " + credentialSecretEnv + "，无法保存 API Key")

// maxAPIKeyLength bounds a stored key. Real keys are well under this; the limit
// keeps a malformed request from writing megabytes into the store.
const maxAPIKeyLength = 4096

// credentialState is the on-disk document. Public holds the administrator's
// shared pool keyed by provider name; Users maps a user id to that user's own
// keys. Values are sealed records, never plaintext.
type credentialState struct {
	Public map[string]string            `json:"public,omitempty"`
	Users  map[string]map[string]string `json:"users,omitempty"`
}

// credentialStore keeps provider keys, sealed, in the credentials table (one row
// per owner and provider) with an in-memory copy for lookups. A store whose
// cipher is nil is disabled: it answers "nothing configured", refuses writes,
// and never reads or touches the rows.
type credentialStore struct {
	mu     sync.RWMutex
	db     *sqlDB
	cipher cipher.AEAD
	state  credentialState
}

// newCredentialStore opens (or creates) the store under dataDir. secret is the
// raw PIGO_SERVER_SECRET value; empty starts the store disabled, which is not
// an error. A decode failure is an error: silently continuing would present an
// existing deployment with an empty store and invite overwriting it.
func newCredentialStore(db *sqlDB, secret string) (*credentialStore, error) {
	store := &credentialStore{
		db:    db,
		state: credentialState{Public: map[string]string{}, Users: map[string]map[string]string{}},
	}
	if strings.TrimSpace(secret) == "" {
		// Disabled: deliberately do not read the rows, so a deployment that
		// starts without its secret cannot go on to overwrite real data.
		return store, nil
	}
	aead, err := newCredentialCipher(secret)
	if err != nil {
		return nil, err
	}
	store.cipher = aead
	if err := db.query("SELECT owner, provider, record FROM credentials", func(rows *sql.Rows) error {
		var owner, providerName, record string
		if err := rows.Scan(&owner, &providerName, &record); err != nil {
			return err
		}
		store.slotLocked(owner, true)[providerName] = record
		return nil
	}); err != nil {
		return nil, fmt.Errorf("load credentials: %w", err)
	}
	return store, nil
}

// slotLocked is an owner's map, created when create is set. Caller holds mu
// (or, at construction, owns the store).
func (s *credentialStore) slotLocked(owner string, create bool) map[string]string {
	if owner == "" {
		return s.state.Public
	}
	if s.state.Users[owner] == nil && create {
		s.state.Users[owner] = map[string]string{}
	}
	return s.state.Users[owner]
}

// newCredentialCipher derives the record key from the master secret.
func newCredentialCipher(secret string) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, []byte(secret), nil, credentialKeyInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("derive credential key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("credential cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("credential cipher: %w", err)
	}
	return aead, nil
}

// enabled reports whether keys can be stored and read back.
func (s *credentialStore) enabled() bool {
	return s != nil && s.cipher != nil
}

// slotAAD binds a sealed record to its slot. An empty owner is the public pool.
func slotAAD(owner, providerName string) []byte {
	return []byte("pigo-credential\x00" + owner + "\x00" + strings.ToLower(providerName))
}

// seal encrypts key for the given slot.
func (s *credentialStore) seal(owner, providerName, key string) (string, error) {
	nonce := make([]byte, s.cipher.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("credential nonce: %w", err)
	}
	sealed := s.cipher.Seal(nonce, nonce, []byte(key), slotAAD(owner, providerName))
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// open decrypts a stored record. A record that fails to open (wrong secret,
// tampered file, moved between slots) yields "" rather than an error: the
// caller's next fallback applies, which keeps one damaged entry from taking the
// whole request down.
func (s *credentialStore) open(owner, providerName, record string) string {
	raw, err := base64.StdEncoding.DecodeString(record)
	if err != nil || len(raw) < s.cipher.NonceSize() {
		return ""
	}
	nonce, body := raw[:s.cipher.NonceSize()], raw[s.cipher.NonceSize():]
	plain, err := s.cipher.Open(nil, nonce, body, slotAAD(owner, providerName))
	if err != nil {
		return ""
	}
	return string(plain)
}

// setUserKey stores one user's key for a provider.
func (s *credentialStore) setUserKey(userID, providerName, key string) error {
	return s.set(userID, providerName, key)
}

// setPublicKey stores the shared pool's key for a provider.
func (s *credentialStore) setPublicKey(providerName, key string) error {
	return s.set("", providerName, key)
}

func (s *credentialStore) set(owner, providerName, key string) error {
	if !s.enabled() {
		return errCredentialsDisabled
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("API Key 不能为空")
	}
	if len(key) > maxAPIKeyLength {
		return errors.New("API Key 过长")
	}
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	if providerName == "" {
		return errors.New("缺少 provider")
	}
	sealed, err := s.seal(owner, providerName, key)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.exec(`INSERT INTO credentials (owner, provider, record) VALUES (?, ?, ?)
ON CONFLICT (owner, provider) DO UPDATE SET record = excluded.record`, owner, providerName, sealed); err != nil {
		return fmt.Errorf("save credential: %w", err)
	}
	s.slotLocked(owner, true)[providerName] = sealed
	return nil
}

// deleteUserKey removes one user's key, reporting whether anything was removed.
func (s *credentialStore) deleteUserKey(userID, providerName string) (bool, error) {
	return s.delete(userID, providerName)
}

// deletePublicKey removes a shared-pool key.
func (s *credentialStore) deletePublicKey(providerName string) (bool, error) {
	return s.delete("", providerName)
}

func (s *credentialStore) delete(owner, providerName string) (bool, error) {
	if !s.enabled() {
		return false, errCredentialsDisabled
	}
	providerName = strings.ToLower(strings.TrimSpace(providerName))

	s.mu.Lock()
	defer s.mu.Unlock()
	slot := s.state.Public
	if owner != "" {
		slot = s.state.Users[owner]
	}
	if _, ok := slot[providerName]; !ok {
		return false, nil
	}
	if _, err := s.db.exec("DELETE FROM credentials WHERE owner = ? AND provider = ?", owner, providerName); err != nil {
		return false, err
	}
	delete(slot, providerName)
	if owner != "" && len(slot) == 0 {
		delete(s.state.Users, owner)
	}
	return true, nil
}

// dropUser removes every key belonging to a user, for account deletion.
func (s *credentialStore) dropUser(userID string) error {
	if !s.enabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Users[userID]; !ok || userID == "" {
		return nil
	}
	if _, err := s.db.exec("DELETE FROM credentials WHERE owner = ?", userID); err != nil {
		return err
	}
	delete(s.state.Users, userID)
	return nil
}

// resolve returns the key to use for (user, provider), applying the precedence
// the spec fixes: the user's own key, then the shared pool, then "" — which
// leaves the provider credential store to fall back to the process environment.
//
// allowUserKeys is the administrator's switch: when personal keys are turned
// off, stored entries stop being used but are not deleted.
func (s *credentialStore) resolve(userID, providerName string, allowUserKeys bool) string {
	if !s.enabled() {
		return ""
	}
	providerName = strings.ToLower(strings.TrimSpace(providerName))

	s.mu.RLock()
	defer s.mu.RUnlock()
	if allowUserKeys && userID != "" {
		if record, ok := s.state.Users[userID][providerName]; ok {
			if key := s.open(userID, providerName, record); key != "" {
				return key
			}
		}
	}
	if record, ok := s.state.Public[providerName]; ok {
		if key := s.open("", providerName, record); key != "" {
			return key
		}
	}
	return ""
}

// configuredProviders lists the provider names an owner has keys for, sorted.
func (s *credentialStore) configuredProviders(owner string) []string {
	if !s.enabled() {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	slot := s.state.Public
	if owner != "" {
		slot = s.state.Users[owner]
	}
	out := make([]string, 0, len(slot))
	for name := range slot {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// has reports whether a slot holds a record, without decrypting it.
func (s *credentialStore) has(owner, providerName string) bool {
	if !s.enabled() {
		return false
	}
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	s.mu.RLock()
	defer s.mu.RUnlock()
	slot := s.state.Public
	if owner != "" {
		slot = s.state.Users[owner]
	}
	_, ok := slot[providerName]
	return ok
}

// hint returns the masked form shown in the UI, or "" when nothing is stored.
// It is the only thing derived from a key that ever leaves the server.
func (s *credentialStore) hint(owner, providerName string) string {
	if !s.enabled() {
		return ""
	}
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	s.mu.RLock()
	slot := s.state.Public
	if owner != "" {
		slot = s.state.Users[owner]
	}
	record, ok := slot[providerName]
	s.mu.RUnlock()
	if !ok {
		return ""
	}
	return maskAPIKey(s.open(owner, providerName, record))
}

// maskAPIKey shows a key's last four characters, enough to tell two keys apart
// without being enough to use one.
func maskAPIKey(key string) string {
	if key == "" {
		return ""
	}
	runes := []rune(key)
	if len(runes) <= 8 {
		return "••••"
	}
	return "••••" + string(runes[len(runes)-4:])
}
