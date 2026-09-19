// Moving an older release's state, kept in JSON files in the data directory,
// into the database: "pigo-server migrate".
//
// The server refuses to start while any of those files is still in place —
// otherwise it would run on an empty database and silently ignore them. The
// migration imports everything in one transaction into an empty database, then
// moves the files to <data>/legacy/ (same relative paths), where they stay as
// a backup until removed by hand. Transcripts are already in their final form
// (sessions/<id>/transcript/chat.jsonl) and stay where they are, as do
// workspaces and every other real file.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// findLegacyFiles lists the old state files present under dataDir, relative
// to it.
func findLegacyFiles(dataDir string) []string {
	var out []string
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(dataDir, rel))
		return err == nil
	}
	for _, rel := range []string{"auth/auth.json", "settings.json", "credentials.json", "models/models.json"} {
		if exists(rel) {
			out = append(out, rel)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(dataDir, "ledger", "*.jsonl")); len(matches) > 0 {
		sort.Strings(matches)
		for _, m := range matches {
			out = append(out, filepath.Join("ledger", filepath.Base(m)))
		}
	}
	dirs, _ := os.ReadDir(filepath.Join(dataDir, "sessions"))
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		if rel := filepath.Join("sessions", d.Name(), "meta.json"); exists(rel) {
			out = append(out, rel)
		}
	}
	return out
}

func legacyFilesError(dataDir string, files []string) error {
	shown := files
	if len(shown) > 5 {
		shown = append(append([]string(nil), shown[:5]...), fmt.Sprintf("…（共 %d 项）", len(files)))
	}
	return fmt.Errorf("数据目录 %s 中还有旧版的 JSON 数据：%s。\n"+
		"服务端状态现在保存在数据库里。请先停止旧版服务，然后运行：\n"+
		"    pigo-server migrate -data %s   （使用 PostgreSQL 时再加 -db <连接串>）\n"+
		"迁移完成后旧文件会被移到 %s。",
		dataDir, strings.Join(shown, "、"), dataDir, filepath.Join(dataDir, "legacy"))
}

// legacyReport counts what an import brought over.
type legacyReport struct {
	Users, Logins, Credentials, CustomModels, Sessions, LedgerEntries int
	Settings                                                          bool
	// Skipped lists what could not be read; the files are still moved to
	// legacy/, so nothing is lost and startup is not blocked by them.
	Skipped []string
}

func (r legacyReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "用户 %d，登录 %d，密钥 %d，自定义模型 %d，会话 %d（对话记录保留在原处），账本记录 %d，设置 %s",
		r.Users, r.Logins, r.Credentials, r.CustomModels, r.Sessions, r.LedgerEntries,
		map[bool]string{true: "已导入", false: "无"}[r.Settings])
	for _, s := range r.Skipped {
		b.WriteString("\n  跳过：" + s)
	}
	return b.String()
}

// errDBNotEmpty refuses an import into a database that already has data.
var errDBNotEmpty = errors.New("目标数据库已有数据，拒绝导入（防止重复导入或覆盖）")

// dbIsEmpty reports whether the database holds no state yet.
func dbIsEmpty(db *sqlDB) (bool, error) {
	for _, table := range []string{"users", "auth_sessions", "credentials", "settings", "custom_models", "sessions", "ledger"} {
		n, err := db.count("SELECT COUNT(*) FROM " + table)
		if err != nil {
			return false, err
		}
		if n > 0 {
			return false, nil
		}
	}
	return true, nil
}

// importLegacy reads every old state file under dataDir and writes it to db in
// one transaction. It does not move the files.
func importLegacy(dataDir string, db *sqlDB) (legacyReport, error) {
	var report legacyReport
	empty, err := dbIsEmpty(db)
	if err != nil {
		return report, err
	}
	if !empty {
		return report, errDBNotEmpty
	}

	readJSON := func(rel string, into any) (bool, error) {
		data, err := os.ReadFile(filepath.Join(dataDir, rel))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if err := json.Unmarshal(data, into); err != nil {
			return false, fmt.Errorf("%s: %w", rel, err)
		}
		return true, nil
	}

	var auth authState
	if _, err := readJSON("auth/auth.json", &auth); err != nil {
		return report, err
	}
	var settings serverSettings
	hasSettings, err := readJSON("settings.json", &settings)
	if err != nil {
		return report, err
	}
	var creds credentialState
	if _, err := readJSON("credentials.json", &creds); err != nil {
		return report, err
	}
	var models []customModel
	if _, err := readJSON("models/models.json", &models); err != nil {
		return report, err
	}

	var sessions []sessionMeta
	dirs, _ := os.ReadDir(filepath.Join(dataDir, "sessions"))
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		rel := filepath.Join("sessions", d.Name(), "meta.json")
		var meta sessionMeta
		ok, err := readJSON(rel, &meta)
		if err != nil || (ok && meta.ID == "") {
			report.Skipped = append(report.Skipped, fmt.Sprintf("%s：无法读取（%v）", rel, err))
			continue
		}
		if !ok {
			continue
		}
		sessions = append(sessions, meta)
	}

	var ledger []ledgerEntry
	seenLedger := map[string]bool{}
	files, _ := filepath.Glob(filepath.Join(dataDir, "ledger", "*.jsonl"))
	sort.Strings(files)
	for _, name := range files {
		f, err := os.Open(name)
		if err != nil {
			return report, err
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		bad := 0
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var e ledgerEntry
			if json.Unmarshal([]byte(line), &e) != nil || e.ID == "" || seenLedger[e.ID] {
				bad++
				continue
			}
			seenLedger[e.ID] = true
			ledger = append(ledger, e)
		}
		err = scanner.Err()
		_ = f.Close()
		if err != nil {
			return report, fmt.Errorf("%s: %w", name, err)
		}
		if bad > 0 {
			report.Skipped = append(report.Skipped, fmt.Sprintf("ledger/%s：%d 行无法读取或重复", filepath.Base(name), bad))
		}
	}

	now := time.Now().UTC()
	err = db.inTx(func(tx *sqlTx) error {
		for _, u := range auth.Users {
			if err := insertUser(tx, u); err != nil {
				return fmt.Errorf("user %s: %w", u.Username, err)
			}
			report.Users++
		}
		for _, a := range auth.Sessions {
			if !a.ExpiresAt.After(now) {
				continue
			}
			if err := insertAuthSession(tx, a); err != nil {
				return fmt.Errorf("login token: %w", err)
			}
			report.Logins++
		}
		if hasSettings {
			if err := saveSettingsDoc(tx, settings); err != nil {
				return err
			}
			report.Settings = true
		}
		insertCred := func(owner, providerName, record string) error {
			_, err := tx.exec("INSERT INTO credentials (owner, provider, record) VALUES (?, ?, ?)", owner, providerName, record)
			report.Credentials++
			return err
		}
		for providerName, record := range creds.Public {
			if err := insertCred("", providerName, record); err != nil {
				return err
			}
		}
		for owner, slot := range creds.Users {
			for providerName, record := range slot {
				if err := insertCred(owner, providerName, record); err != nil {
					return err
				}
			}
		}
		for _, m := range models {
			if m.Scope == "" {
				m.Scope = modelScopeUser
			}
			if err := upsertCustomModel(tx, m); err != nil {
				return err
			}
			report.CustomModels++
		}
		for _, meta := range sessions {
			if err := upsertSession(tx, meta); err != nil {
				return fmt.Errorf("session %s: %w", meta.ID, err)
			}
			report.Sessions++
		}
		for _, e := range ledger {
			if err := insertLedgerEntry(tx, e); err != nil {
				return err
			}
			report.LedgerEntries++
		}
		return nil
	})
	if err != nil {
		return legacyReport{}, err
	}
	return report, nil
}

// moveLegacyFiles moves files (relative to dataDir) under a legacy directory,
// keeping their relative paths. It returns the directory and any file it could
// not move.
func moveLegacyFiles(dataDir string, files []string) (string, []string) {
	root := filepath.Join(dataDir, "legacy")
	if entries, err := os.ReadDir(root); err == nil && len(entries) > 0 {
		// Never mix two migrations' backups.
		root = filepath.Join(dataDir, "legacy-"+time.Now().Format("20060102-150405"))
	}
	var failed []string
	for _, rel := range files {
		dest := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err == nil {
			err = os.Rename(filepath.Join(dataDir, rel), dest)
			if err == nil {
				continue
			}
		}
		failed = append(failed, rel)
	}
	// The emptied ledger/, auth/ and models/ directories go too.
	for _, dir := range []string{"ledger", "auth", "models"} {
		_ = os.Remove(filepath.Join(dataDir, dir))
	}
	return root, failed
}

// runMigrate is "pigo-server migrate". It returns the exit status.
func runMigrate(args []string) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	dataDir := fs.String("data", defaultDataDir(), "data directory holding the old JSON files")
	dbURL := fs.String("db", os.Getenv("PIGO_DB"), "database: sqlite:<path> (default <data>/pigo.db) or postgres://…; also PIGO_DB")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "用法：pigo-server migrate [-data <目录>] [-db <连接串>]")
		fmt.Fprintln(fs.Output(), "把旧版保存在 JSON 文件里的服务端状态导入数据库，然后把旧文件移到 <data>/legacy/。请先停止旧版服务。")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	files := findLegacyFiles(*dataDir)
	if len(files) == 0 {
		fmt.Printf("%s 中没有需要迁移的旧文件。\n", *dataDir)
		return 0
	}
	target, err := parseDBTarget(*dbURL, *dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "迁移失败：", err)
		return 1
	}
	db, err := openDB(target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "迁移失败：", err)
		return 1
	}
	defer db.Close()

	fmt.Printf("从 %s 导入到 %s …\n", *dataDir, target)
	report, err := importLegacy(*dataDir, db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "迁移失败，数据库没有任何改动，旧文件保持原样：", err)
		return 1
	}
	fmt.Println("已导入：" + report.String())

	root, failed := moveLegacyFiles(*dataDir, files)
	if len(failed) > 0 {
		fmt.Fprintf(os.Stderr, "数据已导入，但以下旧文件没能移到 %s，请手动移走，否则服务会拒绝启动：\n", root)
		for _, rel := range failed {
			fmt.Fprintln(os.Stderr, "  "+filepath.Join(*dataDir, rel))
		}
		return 1
	}
	fmt.Printf("旧文件已移到 %s，确认无误后可以删除。\n", root)
	return 0
}
