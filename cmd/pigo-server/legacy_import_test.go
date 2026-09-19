package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/session"
)

// legacyFixture writes a data directory in the pre-database layout, with one
// of everything — and a few things that cannot be read.
type legacyFixture struct {
	dir        string
	aliceID    string
	liveToken  string
	sessionID  string
	entryIDs   []string
	workspaced string
}

func writeLegacyFixture(t *testing.T) legacyFixture {
	t.Helper()
	dir := t.TempDir()
	f := legacyFixture{dir: dir, aliceID: "user-alice", liveToken: "raw-live-token", sessionID: "sess-1"}
	write := func(rel string, v any) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		var data []byte
		switch v := v.(type) {
		case string:
			data = []byte(v)
		default:
			var err error
			if data, err = json.MarshalIndent(v, "", "  "); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	disabled := now.Add(-time.Hour)
	write("auth/auth.json", authState{
		Users: []userRecord{
			{ID: f.aliceID, Username: "alice", UsernameKey: "alice", PasswordHash: "hash-a", CreatedAt: now.Add(-72 * time.Hour)},
			{ID: "user-bob", Username: "bob", UsernameKey: "bob", PasswordHash: "hash-b", CreatedAt: now.Add(-48 * time.Hour), DisabledAt: &disabled},
		},
		Sessions: []authSession{
			{TokenHash: hashAuthToken(f.liveToken), UserID: f.aliceID, CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)},
			{TokenHash: "expired", UserID: f.aliceID, CreatedAt: now.Add(-60 * 24 * time.Hour), ExpiresAt: now.Add(-time.Hour)},
		},
	})
	write("settings.json", serverSettings{
		DefaultModel: "deepseek-chat", DefaultThinking: "high", AllowUserKeys: true,
		CustomProviders: []customProvider{{Name: "gw", Protocol: "openai", BaseURL: "http://gw/v1"}},
		ModelPrices:     []modelPrice{{Provider: "deepseek", Model: "deepseek-chat", Input: 2, Output: 8}},
	})
	aead, err := newCredentialCipher(testSecret)
	if err != nil {
		t.Fatal(err)
	}
	sealer := &credentialStore{cipher: aead}
	userRecord, _ := sealer.seal(f.aliceID, "openai", "sk-alice")
	publicRecord, _ := sealer.seal("", "deepseek", "sk-public")
	write("credentials.json", credentialState{
		Public: map[string]string{"deepseek": publicRecord},
		Users:  map[string]map[string]string{f.aliceID: {"openai": userRecord}},
	})
	// One entry written before scopes existed, one public.
	write("models/models.json", `[
  {"id":"old/model","label":"Old","provider":"openrouter","userId":"user-alice","expiresAt":"2099-01-01","createdAt":"2026-01-01T00:00:00Z"},
  {"id":"shared/model","label":"Shared","provider":"deepseek","userId":"","expiresAt":"","createdAt":"2026-01-02T00:00:00Z","scope":"public"}
]`)

	// A session with a transcript and a workspace file.
	root := filepath.Join(dir, "sessions", f.sessionID)
	write(filepath.Join("sessions", f.sessionID, "meta.json"), sessionMeta{ID: f.sessionID, UserID: f.aliceID, Title: "你好", Model: "deepseek-chat", CreatedAt: now, LastUsed: now})
	store, err := session.NewStore(filepath.Join(root, "transcript"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(session.SessionHeader{ID: transcriptID, CreatedAt: now, UpdatedAt: now}, agentcore.MessageList{
		textMessage(agentcore.RoleUser, "你好"), textMessage(agentcore.RoleAssistant, "你好！"),
	}); err != nil {
		t.Fatal(err)
	}
	_, entries, err := store.LoadEntries(transcriptID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		f.entryIDs = append(f.entryIDs, e.ID)
	}
	f.workspaced = filepath.Join(root, "workspace", "notes.txt")
	write(filepath.Join("sessions", f.sessionID, "workspace", "notes.txt"), "keep me")
	// A session whose metadata is unreadable.
	write("sessions/broken/meta.json", "{not json")

	// The ledger: two good lines, one corrupt, one duplicate.
	good1, _ := json.Marshal(ledgerEntry{ID: "l1", At: now.Add(-time.Hour), UserID: f.aliceID, SessionID: f.sessionID, Provider: "deepseek", Model: "deepseek-chat", Input: 100, Cost: 1000, Priced: true, Price: &priceSnapshot{Input: 2, Output: 8}})
	good2, _ := json.Marshal(ledgerEntry{ID: "l2", At: now, UserID: f.aliceID, SessionID: f.sessionID, Provider: "deepseek", Model: "deepseek-chat", Output: 5})
	write("ledger/2026-09.jsonl", string(good1)+"\n{broken\n"+string(good2)+"\n"+string(good1)+"\n")
	return f
}

func legacyServerConfig(dir string) serverConfig {
	return serverConfig{dataDir: dir, model: "openrouter/free", thinking: "medium", maxSessions: 8}
}

// TestMigrateLegacyDataDir runs the whole path: the server refuses to start on
// old files, "migrate" imports them, moves them to legacy/, and the server then
// starts with everything in place.
func TestMigrateLegacyDataDir(t *testing.T) {
	t.Setenv(credentialSecretEnv, testSecret)
	t.Setenv("PIGO_DB", "")
	f := writeLegacyFixture(t)

	files := findLegacyFiles(f.dir)
	for _, want := range []string{"auth/auth.json", "settings.json", "credentials.json", "models/models.json",
		"ledger/2026-09.jsonl", "sessions/sess-1/meta.json", "sessions/broken/meta.json"} {
		found := false
		for _, got := range files {
			found = found || got == want
		}
		if !found {
			t.Errorf("legacy file %s not detected (got %v)", want, files)
		}
	}
	if _, err := newAPIServer(legacyServerConfig(f.dir)); err == nil || !strings.Contains(err.Error(), "pigo-server migrate") {
		t.Fatalf("the server must refuse to start on old files, got %v", err)
	}

	if code := runMigrate([]string{"-data", f.dir}); code != 0 {
		t.Fatalf("migrate exit = %d", code)
	}
	if got := findLegacyFiles(f.dir); len(got) != 0 {
		t.Fatalf("files left behind: %v", got)
	}
	for _, rel := range files {
		if _, err := os.Stat(filepath.Join(f.dir, "legacy", rel)); err != nil {
			t.Errorf("%s not moved to legacy/: %v", rel, err)
		}
	}
	if data, err := os.ReadFile(f.workspaced); err != nil || string(data) != "keep me" {
		t.Errorf("the workspace was touched: %q %v", data, err)
	}
	for _, rel := range files {
		if strings.Contains(rel, "transcript") {
			t.Errorf("a transcript was treated as a legacy file: %s", rel)
		}
	}

	server, err := newAPIServer(legacyServerConfig(f.dir))
	if err != nil {
		t.Fatal(err)
	}
	defer server.close()

	if user, ok := server.auth.authenticate(f.liveToken); !ok || user.ID != f.aliceID {
		t.Errorf("a live login was lost: %+v %v", user, ok)
	}
	if n, _ := server.db.count("SELECT COUNT(*) FROM auth_sessions"); n != 1 {
		t.Errorf("logins = %d, want only the live one", n)
	}
	if bob, ok := server.auth.findUser("user-bob"); !ok || bob.DisabledAt == nil {
		t.Errorf("bob = %+v %v", bob, ok)
	}
	settings := server.settings.get()
	if settings.DefaultModel != "deepseek-chat" || len(settings.CustomProviders) != 1 || len(settings.ModelPrices) != 1 {
		t.Errorf("settings = %+v", settings)
	}
	if got := server.credentials.resolve(f.aliceID, "openai", true); got != "sk-alice" {
		t.Errorf("user key = %q", got)
	}
	if got := server.credentials.resolve("someone", "deepseek", true); got != "sk-public" {
		t.Errorf("public key = %q", got)
	}
	if own := server.customModels.list(f.aliceID); len(own) != 1 || own[0].Scope != modelScopeUser {
		t.Errorf("own models = %+v", own)
	}
	if shared := server.customModels.listPublic(); len(shared) != 1 {
		t.Errorf("public models = %+v", shared)
	}
	managed, ok := server.getSession(f.sessionID)
	if !ok || managed.meta.Title != "你好" {
		t.Fatalf("session = %+v %v", managed, ok)
	}
	if len(server.sessions) != 1 {
		t.Errorf("sessions = %d; the unreadable one must be skipped", len(server.sessions))
	}
	// The transcript stayed in place, and is read as it was.
	entries, err := readTranscript(managed.paths)
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries = %d %v", len(entries), err)
	}
	for i, e := range entries {
		if e.ID != f.entryIDs[i] {
			t.Errorf("entry %d id = %s, want the original %s", i, e.ID, f.entryIDs[i])
		}
	}
	ledger, err := server.ledger.scan(ledgerQuery{})
	if err != nil || len(ledger) != 2 || ledger[0].Price == nil || ledger[0].Price.Input != 2 {
		t.Errorf("ledger = %+v %v", ledger, err)
	}

	// Nothing left to migrate; and a populated database refuses an import.
	if code := runMigrate([]string{"-data", f.dir}); code != 0 {
		t.Errorf("second migrate exit = %d", code)
	}
	if _, err := importLegacy(filepath.Join(f.dir, "legacy"), server.db); !errors.Is(err, errDBNotEmpty) {
		t.Errorf("import into a populated database = %v", err)
	}
}

// TestImportLegacyReport: what could not be read is reported, not fatal.
func TestImportLegacyReport(t *testing.T) {
	f := writeLegacyFixture(t)
	report, err := importLegacy(f.dir, openTestDB(t))
	if err != nil {
		t.Fatal(err)
	}
	if report.Users != 2 || report.Logins != 1 || report.Credentials != 2 || report.CustomModels != 2 ||
		report.Sessions != 1 || report.LedgerEntries != 2 || !report.Settings {
		t.Errorf("report = %+v", report)
	}
	text := strings.Join(report.Skipped, "\n")
	if !strings.Contains(text, "sessions/broken/meta.json") || !strings.Contains(text, "2 行") {
		t.Errorf("skipped = %v", report.Skipped)
	}
}
