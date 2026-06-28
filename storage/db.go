package storage

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kamden/emailagent/config"
)

var cryptoRandRead = rand.Read

type DB struct {
	db *sql.DB
}

// LabelSuggestion is a cached sender-domain → label mapping.
type LabelSuggestion struct {
	Domain    string
	LabelID   string
	LabelName string
}

type DigestEmail struct {
	Number   int
	EmailID  string
	Account  string
	MsgID    string
	Subject  string
	From     string
	FromAddr string
}

func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening db: %w", err)
	}

	// Migrate old single-user schemas before creating new tables.
	migrateSchema(db)

	schema := []string{
		`CREATE TABLE IF NOT EXISTS seen_emails (
			id       TEXT PRIMARY KEY,
			account  TEXT NOT NULL,
			seen_at  DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS last_run (
			account  TEXT PRIMARY KEY,
			ran_at   DATETIME NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS digest_emails (
			user      TEXT NOT NULL DEFAULT '',
			number    INTEGER NOT NULL,
			email_id  TEXT NOT NULL,
			account   TEXT NOT NULL,
			msg_id    TEXT NOT NULL,
			subject   TEXT NOT NULL,
			from_name TEXT NOT NULL,
			from_addr TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (user, number)
		)`,
		`CREATE TABLE IF NOT EXISTS poll_state (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS sender_suggestions (
			user        TEXT NOT NULL DEFAULT '',
			domain      TEXT NOT NULL,
			label_id    TEXT NOT NULL,
			label_name  TEXT NOT NULL,
			source      TEXT NOT NULL DEFAULT 'inferred',
			updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (user, domain)
		)`,
		`CREATE TABLE IF NOT EXISTS accounts (
			name             TEXT PRIMARY KEY,
			type             TEXT NOT NULL,
			email            TEXT NOT NULL DEFAULT '',
			host             TEXT NOT NULL DEFAULT '',
			port             INTEGER NOT NULL DEFAULT 0,
			password         TEXT NOT NULL DEFAULT '',
			archive_mailbox  TEXT NOT NULL DEFAULT '',
			trash_mailbox    TEXT NOT NULL DEFAULT '',
			token_file       TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS users (
			id               TEXT PRIMARY KEY,
			mattermost_user  TEXT NOT NULL UNIQUE,
			timezone         TEXT NOT NULL DEFAULT '',
			preferences_json TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE TABLE IF NOT EXISTS user_accounts (
			user_id      TEXT NOT NULL,
			account_name TEXT NOT NULL,
			PRIMARY KEY (user_id, account_name)
		)`,
		`CREATE TABLE IF NOT EXISTS app_settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	}
	for _, q := range schema {
		if _, err := db.Exec(q); err != nil {
			return nil, fmt.Errorf("schema: %w", err)
		}
	}

	return &DB{db: db}, nil
}

// ---- app_settings ----

func (d *DB) GetSetting(key, defaultVal string) string {
	var v string
	if err := d.db.QueryRow("SELECT value FROM app_settings WHERE key = ?", key).Scan(&v); err != nil {
		return defaultVal
	}
	return v
}

func (d *DB) SetSetting(key, value string) error {
	_, err := d.db.Exec("INSERT OR REPLACE INTO app_settings (key, value) VALUES (?, ?)", key, value)
	return err
}

// IsConfigured reports whether the database contains Mattermost bootstrap settings.
func (d *DB) IsConfigured() bool {
	return d.GetSetting("mattermost.server_url", "") != ""
}

// ---- accounts ----

func (d *DB) GetAllAccounts() ([]config.Account, error) {
	rows, err := d.db.Query("SELECT name, type, email, host, port, password, archive_mailbox, trash_mailbox, token_file FROM accounts ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var accs []config.Account
	for rows.Next() {
		var a config.Account
		if err := rows.Scan(&a.Name, &a.Type, &a.Email, &a.Host, &a.Port, &a.Password, &a.ArchiveMailbox, &a.TrashMailbox, &a.TokenFile); err != nil {
			return nil, err
		}
		accs = append(accs, a)
	}
	return accs, rows.Err()
}

func (d *DB) GetAccount(name string) (config.Account, error) {
	var a config.Account
	err := d.db.QueryRow("SELECT name, type, email, host, port, password, archive_mailbox, trash_mailbox, token_file FROM accounts WHERE name = ?", name).
		Scan(&a.Name, &a.Type, &a.Email, &a.Host, &a.Port, &a.Password, &a.ArchiveMailbox, &a.TrashMailbox, &a.TokenFile)
	return a, err
}

func (d *DB) UpsertAccount(acc config.Account) error {
	_, err := d.db.Exec(
		`INSERT OR REPLACE INTO accounts (name, type, email, host, port, password, archive_mailbox, trash_mailbox, token_file)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		acc.Name, acc.Type, acc.Email, acc.Host, acc.Port, acc.Password, acc.ArchiveMailbox, acc.TrashMailbox, acc.TokenFile,
	)
	return err
}

func (d *DB) DeleteAccount(name string) (config.Account, error) {
	acc, err := d.GetAccount(name)
	if err == sql.ErrNoRows {
		return config.Account{}, fmt.Errorf("account %q not found", name)
	}
	if err != nil {
		return config.Account{}, err
	}
	tx, err := d.db.Begin()
	if err != nil {
		return config.Account{}, err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.Exec("DELETE FROM accounts WHERE name = ?", name); err != nil {
		return config.Account{}, err
	}
	if _, err := tx.Exec("DELETE FROM user_accounts WHERE account_name = ?", name); err != nil {
		return config.Account{}, err
	}
	return acc, tx.Commit()
}

func (d *DB) RenameAccount(oldName, newName string) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var a config.Account
	if err := tx.QueryRow("SELECT name, type, email, host, port, password, archive_mailbox, trash_mailbox, token_file FROM accounts WHERE name = ?", oldName).
		Scan(&a.Name, &a.Type, &a.Email, &a.Host, &a.Port, &a.Password, &a.ArchiveMailbox, &a.TrashMailbox, &a.TokenFile); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("account %q not found", oldName)
		}
		return err
	}

	tokenFile := a.TokenFile
	if tokenFile == oldName+"_token.json" {
		tokenFile = newName + "_token.json"
	}

	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO accounts (name, type, email, host, port, password, archive_mailbox, trash_mailbox, token_file)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		newName, a.Type, a.Email, a.Host, a.Port, a.Password, a.ArchiveMailbox, a.TrashMailbox, tokenFile,
	); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM accounts WHERE name = ?", oldName); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE user_accounts SET account_name = ? WHERE account_name = ?", newName, oldName); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- users ----

func (d *DB) GetAllUsers() ([]config.User, error) {
	rows, err := d.db.Query("SELECT id, mattermost_user, timezone FROM users ORDER BY mattermost_user")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []config.User
	for rows.Next() {
		var u config.User
		if err := rows.Scan(&u.ID, &u.MattermostUser, &u.Timezone); err != nil {
			return nil, err
		}
		accs, err := d.getUserAccounts(u.ID)
		if err != nil {
			return nil, err
		}
		u.Accounts = accs
		users = append(users, u)
	}
	return users, rows.Err()
}

func (d *DB) UpsertUser(u config.User) error {
	_, err := d.db.Exec(
		"INSERT OR REPLACE INTO users (id, mattermost_user, timezone, preferences_json) VALUES (?, ?, ?, COALESCE((SELECT preferences_json FROM users WHERE id = ?), '{}'))",
		u.ID, u.MattermostUser, u.Timezone, u.ID,
	)
	return err
}

func (d *DB) SetUserTimezone(mattermostUser, tz string) error {
	res, err := d.db.Exec("UPDATE users SET timezone = ? WHERE mattermost_user = ?", tz, mattermostUser)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("user %q not found", mattermostUser)
	}
	return nil
}

func (d *DB) AddUserAccount(userID, accountName string) error {
	_, err := d.db.Exec("INSERT OR IGNORE INTO user_accounts (user_id, account_name) VALUES (?, ?)", userID, accountName)
	return err
}

func (d *DB) RemoveUserAccount(userID, accountName string) error {
	_, err := d.db.Exec("DELETE FROM user_accounts WHERE user_id = ? AND account_name = ?", userID, accountName)
	return err
}

func (d *DB) getUserAccounts(userID string) ([]string, error) {
	rows, err := d.db.Query("SELECT account_name FROM user_accounts WHERE user_id = ? ORDER BY account_name", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// ---- preferences ----

func (d *DB) LoadUserPreferences(userID string) (*config.Preferences, error) {
	var raw string
	err := d.db.QueryRow("SELECT preferences_json FROM users WHERE id = ?", userID).Scan(&raw)
	if err == sql.ErrNoRows || raw == "" || raw == "{}" {
		return defaultPrefs(), nil
	}
	if err != nil {
		return nil, err
	}
	var prefs config.Preferences
	if err := json.Unmarshal([]byte(raw), &prefs); err != nil {
		return defaultPrefs(), nil
	}
	if prefs.Digest.MaxPreviewLength == 0 {
		prefs.Digest.MaxPreviewLength = 150
	}
	return &prefs, nil
}

func (d *DB) SaveUserPreferences(userID string, prefs *config.Preferences) error {
	b, err := json.Marshal(prefs)
	if err != nil {
		return err
	}
	_, err = d.db.Exec("UPDATE users SET preferences_json = ? WHERE id = ?", string(b), userID)
	return err
}

func defaultPrefs() *config.Preferences {
	return &config.Preferences{Digest: config.DigestOpts{MaxPreviewLength: 150, VIPFirst: true}}
}

// ---- seed from YAML config ----

// SeedFromYAML imports accounts, users, preferences, and app settings from a YAML config.
// It is a no-op if the database is already configured.
func (d *DB) SeedFromYAML(cfg *config.Config, defaultPrefsPath string) error {
	if d.IsConfigured() {
		return nil
	}

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	// App settings.
	settings := map[string]string{
		"mattermost.bot_token":      cfg.Mattermost.BotToken,
		"mattermost.server_url":     cfg.Mattermost.ServerURL,
		"mattermost.dm_user":        cfg.Mattermost.DMUser,
		"mattermost.username":       cfg.Mattermost.Username,
		"mattermost.callback_url":   cfg.Mattermost.CallbackURL,
		"mattermost.port":           strconv.Itoa(cfg.Mattermost.Port),
		"mattermost.webhook_secret": cfg.Mattermost.WebhookSecret,
		"ollama.enabled":            strconv.FormatBool(cfg.Ollama.Enabled),
		"ollama.host":               cfg.Ollama.Host,
		"ollama.model":              cfg.Ollama.Model,
		"ollama.token":              cfg.Ollama.Token,
		"ollama.provider":           cfg.Ollama.Provider,
		"schedule":                  cfg.Schedule,
		"poll_interval":             strconv.Itoa(cfg.PollInterval),
		"database":                  cfg.Database,
	}
	for k, v := range settings {
		if _, err := tx.Exec("INSERT OR IGNORE INTO app_settings (key, value) VALUES (?, ?)", k, v); err != nil {
			return fmt.Errorf("seed setting %s: %w", k, err)
		}
	}

	// Accounts.
	for _, acc := range cfg.Accounts {
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO accounts (name, type, email, host, port, password, archive_mailbox, trash_mailbox, token_file)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			acc.Name, acc.Type, acc.Email, acc.Host, acc.Port, acc.Password, acc.ArchiveMailbox, acc.TrashMailbox, acc.TokenFile,
		); err != nil {
			return fmt.Errorf("seed account %s: %w", acc.Name, err)
		}
	}

	// Users (fill in default preferences path where not set).
	users := cfg.Users
	for i := range users {
		if users[i].Preferences == "" {
			users[i].Preferences = defaultPrefsPath
		}
	}
	for _, u := range users {
		if u.ID == "" {
			u.ID = newUserID()
		}
		prefsJSON := "{}"
		if prefs, err := config.LoadPreferences(u.Preferences); err == nil {
			if b, err := json.Marshal(prefs); err == nil {
				prefsJSON = string(b)
			}
		}
		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO users (id, mattermost_user, timezone, preferences_json) VALUES (?, ?, ?, ?)",
			u.ID, u.MattermostUser, u.Timezone, prefsJSON,
		); err != nil {
			return fmt.Errorf("seed user %s: %w", u.MattermostUser, err)
		}
		for _, accName := range u.Accounts {
			if _, err := tx.Exec(
				"INSERT OR IGNORE INTO user_accounts (user_id, account_name) VALUES (?, ?)",
				u.ID, accName,
			); err != nil {
				return fmt.Errorf("seed user_accounts %s/%s: %w", u.ID, accName, err)
			}
		}
	}

	return tx.Commit()
}

// LoadConfig reads the full application configuration from the database.
func (d *DB) LoadConfig() (*config.Config, error) {
	port, _ := strconv.Atoi(d.GetSetting("mattermost.port", "8090"))
	pollInterval, _ := strconv.Atoi(d.GetSetting("poll_interval", "30"))
	ollamaEnabled, _ := strconv.ParseBool(d.GetSetting("ollama.enabled", "false"))

	cfg := &config.Config{
		Mattermost: config.Mattermost{
			BotToken:      d.GetSetting("mattermost.bot_token", ""),
			ServerURL:     d.GetSetting("mattermost.server_url", ""),
			DMUser:        d.GetSetting("mattermost.dm_user", ""),
			Username:      d.GetSetting("mattermost.username", "Email Agent"),
			CallbackURL:   d.GetSetting("mattermost.callback_url", ""),
			Port:          port,
			WebhookSecret: d.GetSetting("mattermost.webhook_secret", ""),
		},
		Ollama: config.Ollama{
			Enabled:  ollamaEnabled,
			Host:     d.GetSetting("ollama.host", "http://localhost:11434"),
			Model:    d.GetSetting("ollama.model", "llama3.2"),
			Token:    d.GetSetting("ollama.token", ""),
			Provider: d.GetSetting("ollama.provider", "ollama"),
		},
		Schedule:     d.GetSetting("schedule", "0 7 * * *"),
		PollInterval: pollInterval,
		Database:     d.GetSetting("database", "email_agent.db"),
	}

	// Apply env var overrides (same as config.Load does).
	if h := os.Getenv("OLLAMA_HOST"); h != "" {
		cfg.Ollama.Host = h
	}
	if t := os.Getenv("OLLAMA_TOKEN"); t != "" {
		cfg.Ollama.Token = t
	}

	accounts, err := d.GetAllAccounts()
	if err != nil {
		return nil, fmt.Errorf("load accounts: %w", err)
	}
	cfg.Accounts = accounts

	users, err := d.GetAllUsers()
	if err != nil {
		return nil, fmt.Errorf("load users: %w", err)
	}
	cfg.Users = users

	return cfg, nil
}

// migrateSchema detects old single-user schemas and upgrades them.
func migrateSchema(db *sql.DB) {
	// digest_emails: old schema had (number INTEGER PRIMARY KEY) without a user column.
	// Data is ephemeral so we drop and recreate.
	var n int
	if db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('digest_emails') WHERE name='user'`).Scan(&n) == nil && n == 0 {
		db.Exec("DROP TABLE IF EXISTS digest_emails") //nolint:errcheck
	}

	// one-time migration: rename old gmail_id column to msg_id (no-op if already correct)
	db.Exec("ALTER TABLE digest_emails RENAME COLUMN gmail_id TO msg_id") //nolint:errcheck

	// digest_emails: add from_addr column if not present
	if db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('digest_emails') WHERE name='from_addr'`).Scan(&n) == nil && n == 0 {
		db.Exec("ALTER TABLE digest_emails ADD COLUMN from_addr TEXT NOT NULL DEFAULT ''") //nolint:errcheck
	}

	// sender_suggestions: add user column if not present (added when multi-user support landed)
	if db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sender_suggestions') WHERE name='user'`).Scan(&n) == nil && n == 0 {
		db.Exec("ALTER TABLE sender_suggestions ADD COLUMN user TEXT NOT NULL DEFAULT ''") //nolint:errcheck
	}
}

func (d *DB) IsSeen(id string) (bool, error) {
	var n int
	err := d.db.QueryRow("SELECT COUNT(*) FROM seen_emails WHERE id = ?", id).Scan(&n)
	return n > 0, err
}

func (d *DB) MarkSeen(id, account string) error {
	_, err := d.db.Exec(
		"INSERT OR IGNORE INTO seen_emails (id, account) VALUES (?, ?)",
		id, account,
	)
	return err
}

func (d *DB) LastRun(account string) (time.Time, error) {
	var t time.Time
	err := d.db.QueryRow("SELECT ran_at FROM last_run WHERE account = ?", account).Scan(&t)
	if err == sql.ErrNoRows {
		return time.Now().Add(-24 * time.Hour), nil
	}
	return t, err
}

func (d *DB) SetLastRun(account string, t time.Time) error {
	_, err := d.db.Exec(
		"INSERT OR REPLACE INTO last_run (account, ran_at) VALUES (?, ?)",
		account, t,
	)
	return err
}

// SaveDigestEmails replaces the current digest email mappings for a user.
func (d *DB) SaveDigestEmails(user string, emails []DigestEmail) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM digest_emails WHERE user = ?", user); err != nil {
		return err
	}
	for _, e := range emails {
		if _, err := tx.Exec(
			"INSERT INTO digest_emails (user, number, email_id, account, msg_id, subject, from_name, from_addr) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			user, e.Number, e.EmailID, e.Account, e.MsgID, e.Subject, e.From, e.FromAddr,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) GetDigestEmail(user string, number int) (DigestEmail, error) {
	var e DigestEmail
	err := d.db.QueryRow(
		"SELECT number, email_id, account, msg_id, subject, from_name, from_addr FROM digest_emails WHERE user = ? AND number = ?",
		user, number,
	).Scan(&e.Number, &e.EmailID, &e.Account, &e.MsgID, &e.Subject, &e.From, &e.FromAddr)
	return e, err
}

// MaxDigestNumber returns the highest number currently in the digest for a user, or 0 if empty.
func (d *DB) MaxDigestNumber(user string) (int, error) {
	var n int
	err := d.db.QueryRow("SELECT COALESCE(MAX(number), 0) FROM digest_emails WHERE user = ?", user).Scan(&n)
	return n, err
}

// AppendDigestEmails inserts new digest email records without clearing existing ones.
func (d *DB) AppendDigestEmails(user string, emails []DigestEmail) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range emails {
		if _, err := tx.Exec(
			"INSERT OR IGNORE INTO digest_emails (user, number, email_id, account, msg_id, subject, from_name, from_addr) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			user, e.Number, e.EmailID, e.Account, e.MsgID, e.Subject, e.From, e.FromAddr,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) GetAllDigestEmails(user string) ([]DigestEmail, error) {
	rows, err := d.db.Query(
		"SELECT number, email_id, account, msg_id, subject, from_name, from_addr FROM digest_emails WHERE user = ? ORDER BY number",
		user,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var emails []DigestEmail
	for rows.Next() {
		var e DigestEmail
		if err := rows.Scan(&e.Number, &e.EmailID, &e.Account, &e.MsgID, &e.Subject, &e.From, &e.FromAddr); err != nil {
			return nil, err
		}
		emails = append(emails, e)
	}
	return emails, nil
}

// GetPollCursor returns the millisecond timestamp to fetch messages after for a user.
func (d *DB) GetPollCursor(user string) (int64, error) {
	var val string
	err := d.db.QueryRow("SELECT value FROM poll_state WHERE key = ?", cursorKey(user)).Scan(&val)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(val, 10, 64)
}

func (d *DB) SetPollCursor(user string, millis int64) error {
	_, err := d.db.Exec(
		"INSERT OR REPLACE INTO poll_state (key, value) VALUES (?, ?)",
		cursorKey(user), strconv.FormatInt(millis, 10),
	)
	return err
}

func cursorKey(user string) string {
	if user == "" {
		return "cursor"
	}
	return "cursor:" + user
}

// GetSenderSuggestion returns a cached label suggestion for a sender domain scoped to a user.
func (d *DB) GetSenderSuggestion(user, domain string) (*LabelSuggestion, error) {
	var sug LabelSuggestion
	var source string
	var updatedAt time.Time

	err := d.db.QueryRow(
		`SELECT domain, label_id, label_name, source, updated_at
		 FROM sender_suggestions WHERE user = ? AND domain = ?`,
		user, domain,
	).Scan(&sug.Domain, &sug.LabelID, &sug.LabelName, &source, &updatedAt)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if source == "inferred" && time.Since(updatedAt) > 24*time.Hour {
		return nil, nil
	}
	return &sug, nil
}

// SetSenderSuggestion upserts a label suggestion for a sender domain scoped to a user.
func (d *DB) SetSenderSuggestion(user, domain, labelID, labelName, source string) error {
	_, err := d.db.Exec(
		`INSERT OR REPLACE INTO sender_suggestions (user, domain, label_id, label_name, source, updated_at)
		 VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		user, domain, labelID, labelName, source,
	)
	return err
}

// MigrateUserKeys renames user keys from old Mattermost usernames to UUIDs across all
// user-scoped tables. Safe to call on every startup — rows already using UUIDs are untouched.
func (d *DB) MigrateUserKeys(oldToNew map[string]string) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	for old, newKey := range oldToNew {
		if old == "" || old == newKey {
			continue
		}
		if _, err := tx.Exec("UPDATE digest_emails SET user = ? WHERE user = ?", newKey, old); err != nil {
			return fmt.Errorf("migrate digest_emails %s: %w", old, err)
		}
		if _, err := tx.Exec("UPDATE sender_suggestions SET user = ? WHERE user = ?", newKey, old); err != nil {
			return fmt.Errorf("migrate sender_suggestions %s: %w", old, err)
		}
		oldKey := "cursor:" + old
		newCursorKey := "cursor:" + newKey
		var exists int
		tx.QueryRow("SELECT COUNT(*) FROM poll_state WHERE key = ?", newCursorKey).Scan(&exists) //nolint:errcheck
		if exists > 0 {
			// New key already written by poll loop — drop the old one.
			if _, err := tx.Exec("DELETE FROM poll_state WHERE key = ?", oldKey); err != nil {
				return fmt.Errorf("migrate poll_state %s: %w", old, err)
			}
		} else {
			if _, err := tx.Exec("UPDATE poll_state SET key = ? WHERE key = ?", newCursorKey, oldKey); err != nil {
				return fmt.Errorf("migrate poll_state %s: %w", old, err)
			}
		}
	}
	return tx.Commit()
}

func (d *DB) Close() error {
	return d.db.Close()
}

// newUserID generates a random UUID v4 for a new user.
func newUserID() string {
	b := make([]byte, 16)
	if _, err := cryptoRandRead(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
