package storage

import (
	"database/sql"
	"fmt"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

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
	}
	for _, q := range schema {
		if _, err := db.Exec(q); err != nil {
			return nil, fmt.Errorf("schema: %w", err)
		}
	}

	return &DB{db: db}, nil
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

func (d *DB) Close() error {
	return d.db.Close()
}
