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
	Number  int
	EmailID string
	Account string
	GmailID string
	Subject string
	From    string
}

func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening db: %w", err)
	}

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
			number    INTEGER PRIMARY KEY,
			email_id  TEXT NOT NULL,
			account   TEXT NOT NULL,
			gmail_id  TEXT NOT NULL,
			subject   TEXT NOT NULL,
			from_name TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS poll_state (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS sender_suggestions (
			domain      TEXT PRIMARY KEY,
			label_id    TEXT NOT NULL,
			label_name  TEXT NOT NULL,
			source      TEXT NOT NULL DEFAULT 'inferred',
			updated_at  DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
	}
	for _, q := range schema {
		if _, err := db.Exec(q); err != nil {
			return nil, fmt.Errorf("schema: %w", err)
		}
	}

	return &DB{db: db}, nil
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

// SaveDigestEmails replaces the current digest email mappings.
func (d *DB) SaveDigestEmails(emails []DigestEmail) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM digest_emails"); err != nil {
		return err
	}
	for _, e := range emails {
		if _, err := tx.Exec(
			"INSERT INTO digest_emails (number, email_id, account, gmail_id, subject, from_name) VALUES (?, ?, ?, ?, ?, ?)",
			e.Number, e.EmailID, e.Account, e.GmailID, e.Subject, e.From,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) GetDigestEmail(number int) (DigestEmail, error) {
	var e DigestEmail
	err := d.db.QueryRow(
		"SELECT number, email_id, account, gmail_id, subject, from_name FROM digest_emails WHERE number = ?",
		number,
	).Scan(&e.Number, &e.EmailID, &e.Account, &e.GmailID, &e.Subject, &e.From)
	return e, err
}

func (d *DB) GetAllDigestEmails() ([]DigestEmail, error) {
	rows, err := d.db.Query(
		"SELECT number, email_id, account, gmail_id, subject, from_name FROM digest_emails ORDER BY number",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var emails []DigestEmail
	for rows.Next() {
		var e DigestEmail
		if err := rows.Scan(&e.Number, &e.EmailID, &e.Account, &e.GmailID, &e.Subject, &e.From); err != nil {
			return nil, err
		}
		emails = append(emails, e)
	}
	return emails, nil
}

// GetPollCursor returns the millisecond timestamp to fetch messages after.
// Returns 0 if no digest has been sent yet.
func (d *DB) GetPollCursor() (int64, error) {
	var val string
	err := d.db.QueryRow("SELECT value FROM poll_state WHERE key = 'cursor'").Scan(&val)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(val, 10, 64)
}

func (d *DB) SetPollCursor(millis int64) error {
	_, err := d.db.Exec(
		"INSERT OR REPLACE INTO poll_state (key, value) VALUES ('cursor', ?)",
		strconv.FormatInt(millis, 10),
	)
	return err
}

// GetSenderSuggestion returns a cached label suggestion for the given sender domain.
// User-confirmed suggestions never expire; inferred ones expire after 24 hours.
func (d *DB) GetSenderSuggestion(domain string) (*LabelSuggestion, error) {
	var sug LabelSuggestion
	var source string
	var updatedAt time.Time

	err := d.db.QueryRow(
		`SELECT domain, label_id, label_name, source, updated_at
		 FROM sender_suggestions WHERE domain = ?`,
		domain,
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

// SetSenderSuggestion upserts a label suggestion for a sender domain.
// source should be "user" (from an explicit move action) or "inferred" (from Gmail scan).
func (d *DB) SetSenderSuggestion(domain, labelID, labelName, source string) error {
	_, err := d.db.Exec(
		`INSERT OR REPLACE INTO sender_suggestions (domain, label_id, label_name, source, updated_at)
		 VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		domain, labelID, labelName, source,
	)
	return err
}

func (d *DB) Close() error {
	return d.db.Close()
}
