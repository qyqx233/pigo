// Schema upgrades: numbered SQL scripts embedded in the binary, applied in
// order at startup. schema_migrations records which have run; each script runs
// in one transaction with its record, so a failure leaves nothing half-applied
// (both SQLite and PostgreSQL have transactional DDL).
package main

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, name := range names {
		base := strings.TrimPrefix(name, "migrations/")
		prefix, _, ok := strings.Cut(base, "_")
		version, err := strconv.Atoi(prefix)
		if !ok || err != nil || version <= 0 {
			return nil, fmt.Errorf("migration %s: name must start with a positive number and an underscore", base)
		}
		body, err := migrationFiles.ReadFile(name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: version, name: base, sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i := 1; i < len(out); i++ {
		if out[i].version == out[i-1].version {
			return nil, fmt.Errorf("migrations %s and %s share a version", out[i-1].name, out[i].name)
		}
	}
	return out, nil
}

// splitStatements splits a script on ";" at the end of a line, dropping
// comment lines. The scripts are written so that no statement contains such a
// line, which keeps this independent of whether a driver accepts several
// statements in one call.
func splitStatements(script string) []string {
	var (
		out     []string
		current strings.Builder
	)
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		current.WriteString(line)
		current.WriteByte('\n')
		if strings.HasSuffix(trimmed, ";") {
			out = append(out, strings.TrimSuffix(strings.TrimSpace(current.String()), ";"))
			current.Reset()
		}
	}
	if rest := strings.TrimSpace(current.String()); rest != "" {
		out = append(out, rest)
	}
	return out
}

func (d *sqlDB) upgradeSchema() error {
	if _, err := d.exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
    version    BIGINT PRIMARY KEY,
    applied_at BIGINT NOT NULL
)`); err != nil {
		return err
	}
	applied := map[int]bool{}
	if err := d.query("SELECT version FROM schema_migrations", func(rows *sql.Rows) error {
		var v int
		if err := rows.Scan(&v); err != nil {
			return err
		}
		applied[v] = true
		return nil
	}); err != nil {
		return err
	}
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		err := d.inTx(func(tx *sqlTx) error {
			for _, stmt := range splitStatements(m.sql) {
				if _, err := tx.exec(stmt); err != nil {
					return fmt.Errorf("%s: %w\n%s", m.name, err, stmt)
				}
			}
			_, err := tx.exec("INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)", m.version, time.Now().UTC().UnixNano())
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}
