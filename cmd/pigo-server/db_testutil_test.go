package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// openTestDB opens a fresh SQLite database in the test's temporary directory.
func openTestDB(t *testing.T) *sqlDB {
	t.Helper()
	db, err := openDB(dbTarget{dialect: dialectSQLite, path: filepath.Join(t.TempDir(), "pigo.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

// testDBTargets are the databases the storage tests run against: SQLite
// always, and PostgreSQL when PIGO_TEST_POSTGRES holds a connection string.
// Each PostgreSQL run gets its own schema, dropped afterwards, so nothing
// already in that database is touched.
func testDBTargets(t *testing.T) map[string]func(t *testing.T) dbTarget {
	targets := map[string]func(t *testing.T) dbTarget{
		"sqlite": func(t *testing.T) dbTarget {
			return dbTarget{dialect: dialectSQLite, path: filepath.Join(t.TempDir(), "pigo.db")}
		},
	}
	dsn := os.Getenv("PIGO_TEST_POSTGRES")
	if dsn == "" {
		return targets
	}
	targets["postgres"] = func(t *testing.T) dbTarget {
		t.Helper()
		admin, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		var b [6]byte
		_, _ = rand.Read(b[:])
		schema := "pigo_test_" + hex.EncodeToString(b[:])
		if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
			admin.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE")
			admin.Close()
		})
		sep := "?"
		if containsQuery(dsn) {
			sep = "&"
		}
		return dbTarget{dialect: dialectPostgres, dsn: dsn + sep + "search_path=" + schema}
	}
	return targets
}

func containsQuery(dsn string) bool {
	for _, r := range dsn {
		if r == '?' {
			return true
		}
	}
	return false
}

// forEachDB runs fn against every test database. fn gets the target so it can
// close and reopen the database (a restart).
func forEachDB(t *testing.T, fn func(t *testing.T, target dbTarget)) {
	for name, make := range testDBTargets(t) {
		t.Run(name, func(t *testing.T) {
			fn(t, make(t))
		})
	}
}

// mustOpen opens target and closes it at the end of the test.
func mustOpen(t *testing.T, target dbTarget) *sqlDB {
	t.Helper()
	db, err := openDB(target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}
