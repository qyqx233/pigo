package main

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
)

func TestParseDBTarget(t *testing.T) {
	cases := []struct {
		raw     string
		dialect sqlDialect
		want    string
	}{
		{"", dialectSQLite, "/data/pigo.db"},
		{"sqlite:state.db", dialectSQLite, "/data/state.db"},
		{"sqlite:///var/lib/pigo/pigo.db", dialectSQLite, "/var/lib/pigo/pigo.db"},
		{"/abs/other.db", dialectSQLite, "/abs/other.db"},
		{"postgres://u:p@db/pigo", dialectPostgres, "postgres://u:p@db/pigo"},
		{"postgresql://db/pigo?sslmode=disable", dialectPostgres, "postgresql://db/pigo?sslmode=disable"},
	}
	for _, tc := range cases {
		got, err := parseDBTarget(tc.raw, "/data")
		if err != nil {
			t.Fatalf("%q: %v", tc.raw, err)
		}
		value := got.path
		if got.dialect == dialectPostgres {
			value = got.dsn
		}
		if got.dialect != tc.dialect || value != tc.want {
			t.Errorf("%q = %v %q", tc.raw, got.dialect, value)
		}
	}
	if _, err := parseDBTarget("mysql://x", "/data"); err == nil {
		t.Error("an unsupported scheme must be refused")
	}
	if s := (dbTarget{dialect: dialectPostgres, dsn: "postgres://u:secret@db/pigo"}).String(); strings.Contains(s, "secret") {
		t.Errorf("the password leaked into %q", s)
	}
}

func TestRebind(t *testing.T) {
	pg := &sqlDB{dialect: dialectPostgres}
	if got := pg.rebind("SELECT a FROM t WHERE b = ? AND c IN (?, ?)"); got != "SELECT a FROM t WHERE b = $1 AND c IN ($2, $3)" {
		t.Errorf("postgres = %q", got)
	}
	lite := &sqlDB{dialect: dialectSQLite}
	if got := lite.rebind("x = ?"); got != "x = ?" {
		t.Errorf("sqlite = %q", got)
	}
}

func TestSplitStatements(t *testing.T) {
	got := splitStatements("-- comment;\nCREATE TABLE a (\n  x TEXT\n);\n\nCREATE INDEX i ON a (x);\n")
	if len(got) != 2 || !strings.HasPrefix(got[0], "CREATE TABLE a") || got[1] != "CREATE INDEX i ON a (x)" {
		t.Errorf("statements = %q", got)
	}
}

// TestSchemaUpgradeOnce: reopening a database does not re-run its scripts.
func TestSchemaUpgradeOnce(t *testing.T) {
	forEachDB(t, func(t *testing.T, target dbTarget) {
		mustOpen(t, target).Close()
		db := mustOpen(t, target)
		migrations, err := loadMigrations()
		if err != nil {
			t.Fatal(err)
		}
		n, err := db.count("SELECT COUNT(*) FROM schema_migrations")
		if err != nil || n != len(migrations) {
			t.Errorf("recorded versions = %d (%v), want %d", n, err, len(migrations))
		}
	})
}

// TestInstanceLock: a second process on the same database is refused, and the
// lock goes with the first.
func TestInstanceLock(t *testing.T) {
	forEachDB(t, func(t *testing.T, target dbTarget) {
		first := mustOpen(t, target)
		if _, err := openDB(target); !errors.Is(err, errDBInUse) {
			t.Fatalf("second open = %v, want the instance lock", err)
		}
		first.Close()
		mustOpen(t, target)
	})
}

// TestAuthStorePersists: accounts and logins survive a restart, expired logins
// do not, and every change reached the database.
func TestAuthStorePersists(t *testing.T) {
	forEachDB(t, func(t *testing.T, target dbTarget) {
		db := mustOpen(t, target)
		store, err := newAuthStore(db)
		if err != nil {
			t.Fatal(err)
		}
		alice, aliceToken, first, err := store.register("alice", "password-alice")
		if err != nil || !first {
			t.Fatalf("register = %v first=%v", err, first)
		}
		bob, _, _, err := store.register("bob", "password-bob")
		if err != nil {
			t.Fatal(err)
		}
		_, loginToken, err := store.login("alice", "password-alice")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.logout(aliceToken); err != nil {
			t.Fatal(err)
		}
		if _, err := store.setDisabled(bob.ID, true); err != nil {
			t.Fatal(err)
		}
		// A login that has already expired.
		if err := insertAuthSession(db, authSession{TokenHash: "old", UserID: alice.ID, CreatedAt: time.Now().Add(-48 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		db.Close()

		db = mustOpen(t, target)
		reloaded, err := newAuthStore(db)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := reloaded.authenticate(loginToken); !ok {
			t.Error("a live login did not survive the restart")
		}
		if _, ok := reloaded.authenticate(aliceToken); ok {
			t.Error("a logged-out token came back")
		}
		if u, ok := reloaded.findUser(bob.ID); !ok || u.DisabledAt == nil {
			t.Errorf("bob = %+v %v, want disabled", u, ok)
		}
		if n, _ := db.count("SELECT COUNT(*) FROM auth_sessions WHERE token_hash = ?", "old"); n != 0 {
			t.Error("an expired login was not pruned")
		}
		if err := reloaded.deleteUser(alice.ID); err != nil {
			t.Fatal(err)
		}
		if n, _ := db.count("SELECT COUNT(*) FROM auth_sessions WHERE user_id = ?", alice.ID); n != 0 {
			t.Error("a deleted user's logins remain")
		}
		if _, _, _, err := reloaded.register("Alice", "another-password"); err != nil {
			t.Errorf("a deleted username could not be reused: %v", err)
		}
	})
}

func textMessage(role, text string) agentcore.Message {
	if role == agentcore.RoleUser {
		return agentcore.UserMessage{RoleField: agentcore.RoleUser, Content: agentcore.ContentList{agentcore.NewTextContent(text)}}
	}
	return agentcore.AssistantMessage{RoleField: agentcore.RoleAssistant, Content: agentcore.ContentList{agentcore.NewTextContent(text)}}
}

// TestRemoveSession: deleting a session removes its row and directory
// (transcript included), and leaves the ledger alone.
func TestRemoveSession(t *testing.T) {
	s := &apiServer{db: openTestDB(t)}
	paths := newSessionPaths(t.TempDir(), "s1")
	if err := paths.create(); err != nil {
		t.Fatal(err)
	}
	if err := upsertSession(s.db, sessionMeta{ID: "s1"}); err != nil {
		t.Fatal(err)
	}
	managed := &managedSession{paths: paths, meta: sessionMeta{ID: "s1"}}
	if err := s.saveTranscript(managed, agentcore.MessageList{textMessage(agentcore.RoleUser, "hi")}); err != nil {
		t.Fatal(err)
	}
	ledger, _ := newLedgerStore(s.db)
	if err := ledger.append(ledgerEntry{ID: "l1", At: time.Now(), SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if !hasTranscript(paths) {
		t.Fatal("transcript not stored")
	}
	if err := s.removeSession("s1", paths); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.db.count("SELECT COUNT(*) FROM sessions"); n != 0 {
		t.Error("session row remains")
	}
	if n, _ := s.db.count("SELECT COUNT(*) FROM ledger"); n != 1 {
		t.Error("the ledger must keep a deleted session's calls")
	}
	if _, err := os.Stat(paths.Root); !os.IsNotExist(err) {
		t.Errorf("session directory remains: %v", err)
	}
}
