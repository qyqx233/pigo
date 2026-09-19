// Editing a custom provider in place: its address, protocol and name.
//
// The name is a key other state is filed under, so a rename carries it along
// in one transaction: the settings document (the endpoint itself and the
// price-table rows), the custom models that name it, the stored keys (re-sealed,
// since a record's encryption is bound to its provider name) and the sessions
// that use it. The billing ledger is history and keeps the name it was billed
// under.
package main

import (
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
)

// handlePatchCustomProvider edits the custom provider named in the path. The
// body is the whole new definition; a different name renames it.
func (s *apiServer) handlePatchCustomProvider(w http.ResponseWriter, r *http.Request) {
	old := strings.ToLower(strings.TrimSpace(r.PathValue("name")))
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
	status, err := s.editCustomProvider(old, next)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	if next.Name != old {
		s.logAdminAction(r, "rename custom provider", old+" -> "+next.Name)
	} else {
		s.logAdminAction(r, "edit custom provider", next.Name)
	}
	writeJSON(w, http.StatusOK, s.settings.customProviders())
}

// editCustomProvider replaces the provider old with next, returning the HTTP
// status to report on failure.
func (s *apiServer) editCustomProvider(old string, next customProvider) (int, error) {
	if _, ok := s.settings.findCustomProvider(old); !ok {
		return http.StatusNotFound, fmt.Errorf("该自定义 provider 不存在")
	}
	rename := next.Name != old
	if _, taken := s.settings.findCustomProvider(next.Name); rename && taken {
		return http.StatusConflict, fmt.Errorf("已有名为 %s 的 provider", next.Name)
	}

	// The sessions on this provider stay locked until they point at the new
	// definition, so no turn starts in between. Locked in id order, before the
	// stores' own locks — the order the session handlers already use.
	sessions := s.sessionsOnProvider(old)
	for _, managed := range sessions {
		managed.mu.Lock()
		defer managed.mu.Unlock()
	}
	for _, managed := range sessions {
		if !managed.closed && managed.activeTurn() != nil {
			return http.StatusConflict, fmt.Errorf("会话 %s 正在用 %s 运行，等它结束后再修改", shortID(managed.meta.ID), old)
		}
	}

	if rename {
		if err := s.renameProviderState(old, next, sessions); err != nil {
			return http.StatusInternalServerError, err
		}
	} else if err := s.settings.putCustomProvider(next); err != nil {
		return http.StatusInternalServerError, err
	}

	// Sessions whose agent is loaded resolved the endpoint when it loaded;
	// point them at the new definition now rather than at their next restart.
	for _, managed := range sessions {
		if managed.closed || managed.agentCtx == nil {
			continue
		}
		if err := s.applyHostConfig(managed); err != nil {
			log.Printf("pigo-server: session %s: apply edited provider %s: %v", shortID(managed.meta.ID), next.Name, err)
		}
	}
	return http.StatusOK, nil
}

// sessionsOnProvider lists the sessions whose metadata names the provider,
// sorted by id.
func (s *apiServer) sessionsOnProvider(name string) []*managedSession {
	s.mu.RLock()
	all := make([]*managedSession, 0, len(s.sessions))
	for _, managed := range s.sessions {
		all = append(all, managed)
	}
	s.mu.RUnlock()
	var out []*managedSession
	for _, managed := range all {
		managed.mu.Lock()
		match := !managed.closed && strings.EqualFold(managed.meta.Provider, name)
		managed.mu.Unlock()
		if match {
			out = append(out, managed)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].meta.ID < out[j].meta.ID })
	return out
}

// renameProviderState moves everything filed under old to next.Name in one
// transaction, then updates the in-memory copies. The caller holds the
// sessions' locks. Every store lock is taken before the transaction begins:
// with SQLite's single connection, a store waiting on the database while
// holding its lock would otherwise deadlock against us.
func (s *apiServer) renameProviderState(old string, next customProvider, sessions []*managedSession) error {
	s.settings.mu.Lock()
	defer s.settings.mu.Unlock()
	s.customModels.mu.Lock()
	defer s.customModels.mu.Unlock()
	creds := s.credentials
	if creds != nil {
		creds.mu.Lock()
		defer creds.mu.Unlock()
	}

	settings := s.settings.settings
	settings.CustomProviders = append([]customProvider(nil), settings.CustomProviders...)
	for i, item := range settings.CustomProviders {
		if item.Name == old {
			settings.CustomProviders[i] = next
		}
	}
	settings.ModelPrices = append([]modelPrice(nil), settings.ModelPrices...)
	for i, row := range settings.ModelPrices {
		if row.Provider == old {
			settings.ModelPrices[i].Provider = next.Name
		}
	}

	// Keys: open under the old name, seal under the new one. A record that no
	// longer opens (a changed secret) is useless under either name and is left
	// where it is.
	type movedKey struct{ owner, record string }
	var moved []movedKey
	if creds.enabled() {
		for owner, slot := range creds.allSlotsLocked() {
			record, ok := slot[old]
			if !ok {
				continue
			}
			key := creds.open(owner, old, record)
			if key == "" {
				log.Printf("pigo-server: rename provider %s: a stored key for %q does not open; left under the old name", old, owner)
				continue
			}
			sealed, err := creds.seal(owner, next.Name, key)
			if err != nil {
				return err
			}
			moved = append(moved, movedKey{owner, sealed})
		}
	}

	metas := make([]sessionMeta, len(sessions))
	for i, managed := range sessions {
		metas[i] = managed.meta
		metas[i].Provider = next.Name
	}

	err := s.db.inTx(func(tx *sqlTx) error {
		if err := saveSettingsDoc(tx, settings); err != nil {
			return err
		}
		if _, err := tx.exec("UPDATE custom_models SET provider = ? WHERE provider = ?", next.Name, old); err != nil {
			return fmt.Errorf("rename provider in custom models: %w", err)
		}
		for _, key := range moved {
			if _, err := tx.exec("DELETE FROM credentials WHERE owner = ? AND provider = ?", key.owner, old); err != nil {
				return fmt.Errorf("move credential: %w", err)
			}
			if _, err := tx.exec("INSERT INTO credentials (owner, provider, record) VALUES (?, ?, ?)", key.owner, next.Name, key.record); err != nil {
				return fmt.Errorf("move credential: %w", err)
			}
		}
		for _, meta := range metas {
			if err := upsertSession(tx, meta); err != nil {
				return fmt.Errorf("session %s: %w", shortID(meta.ID), err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("rename provider %s: %w", old, err)
	}

	s.settings.settings = settings
	for i := range s.customModels.models {
		if s.customModels.models[i].Provider == old {
			s.customModels.models[i].Provider = next.Name
		}
	}
	for _, key := range moved {
		slot := creds.slotLocked(key.owner, true)
		delete(slot, old)
		slot[next.Name] = key.record
	}
	for i, managed := range sessions {
		managed.meta = metas[i]
	}
	return nil
}

// allSlotsLocked maps every owner ("" is the shared pool) to its records.
// Caller holds mu.
func (s *credentialStore) allSlotsLocked() map[string]map[string]string {
	out := map[string]map[string]string{"": s.state.Public}
	for owner, slot := range s.state.Users {
		out[owner] = slot
	}
	return out
}
