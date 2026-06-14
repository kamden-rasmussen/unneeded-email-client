package config

import (
	"crypto/rand"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Accounts     []Account  `yaml:"accounts"`
	Users        []User     `yaml:"users"`
	Mattermost   Mattermost `yaml:"mattermost"`
	Schedule     string     `yaml:"schedule"`
	Ollama       Ollama     `yaml:"ollama"`
	Database     string     `yaml:"database"`
	PollInterval int        `yaml:"poll_interval"` // seconds, default 30
}

type Account struct {
	Name            string `yaml:"name"`
	Type            string `yaml:"type"` // "gmail" | "imap"
	Email           string `yaml:"email"`
	Host            string `yaml:"host"`
	Port            int    `yaml:"port"`
	Password        string `yaml:"password"`
	ArchiveMailbox  string `yaml:"archive_mailbox"` // IMAP only; defaults to "Archive"
	TrashMailbox    string `yaml:"trash_mailbox"`   // IMAP only; defaults to "Trash"
	TokenFile string `yaml:"token_file"`
}

type Mattermost struct {
	BotToken      string `yaml:"bot_token"`
	ServerURL     string `yaml:"server_url"`
	DMUser        string `yaml:"dm_user"`
	Username      string `yaml:"username"`
	CallbackURL   string `yaml:"callback_url"`   // base URL Mattermost can reach us at, e.g. https://email-agent.example.com
	Port          int    `yaml:"port"`            // local port to listen on, default 8090
	WebhookSecret string `yaml:"webhook_secret"` // shared secret embedded in action contexts; required when callback_url is set
}

type Ollama struct {
	Enabled  bool   `yaml:"enabled"`
	Host     string `yaml:"host"`
	Model    string `yaml:"model"`
	Token    string `yaml:"token"`    // optional Bearer token for authenticated instances
	Provider string `yaml:"provider"` // "ollama" (default) or "openai" for OpenAI-compatible APIs (e.g. oMLX)
}

type Preferences struct {
	VIPSenders   []string   `yaml:"vip_senders"`
	MuteSenders  []string   `yaml:"mute_senders"`
	MuteSubjects []string   `yaml:"mute_subjects"`
	Categories   []Category `yaml:"categories"`
	Digest       DigestOpts `yaml:"digest"`
	Filters      []Filter   `yaml:"filters"`
}

// Filter auto-processes emails matching a sender pattern before they reach the digest.
// Actions: "archive", "mark_read", "mute" (skip digest only), "move".
type Filter struct {
	Sender    string   `yaml:"sender"`               // domain or full address (case-insensitive)
	Actions   []string `yaml:"actions"`
	LabelName string   `yaml:"label_name,omitempty"` // required for "move" action
}

type Category struct {
	Name     string   `yaml:"name"`
	Keywords []string `yaml:"keywords"`
	Senders  []string `yaml:"senders"`
}

type DigestOpts struct {
	VIPFirst         bool `yaml:"vip_first"`
	MaxPreviewLength int  `yaml:"max_preview_length"`
}

// User maps a Mattermost username to a subset of email accounts and their own preferences file.
type User struct {
	ID             string   `yaml:"id"`            // stable UUID; auto-generated on first run
	MattermostUser string   `yaml:"mattermost_user"`
	Accounts       []string `yaml:"accounts"`      // names from the top-level accounts list
	Preferences    string   `yaml:"preferences"`   // path to preferences file
}

func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	var cfg Config
	if err := yaml.NewDecoder(f).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if cfg.Database == "" {
		cfg.Database = "email_agent.db"
	}
	if cfg.Schedule == "" {
		cfg.Schedule = "0 7 * * *"
	}
	if h := os.Getenv("OLLAMA_HOST"); h != "" {
		cfg.Ollama.Host = h
	}
	if cfg.Ollama.Host == "" {
		cfg.Ollama.Host = "http://localhost:11434"
	}
	if t := os.Getenv("OLLAMA_TOKEN"); t != "" {
		cfg.Ollama.Token = t
	}
	if cfg.Ollama.Model == "" {
		cfg.Ollama.Model = "llama3.2"
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 30
	}
	if cfg.Mattermost.Port == 0 {
		cfg.Mattermost.Port = 8090
	}
	if cfg.Mattermost.CallbackURL != "" && cfg.Mattermost.WebhookSecret == "" {
		return nil, fmt.Errorf("mattermost.webhook_secret is required when callback_url is set")
	}

	return &cfg, nil
}

// RenameAccount reads the config at path, renames the matching account, and writes it back.
// If the account's token_file was the default (<oldName>_token.json) it is updated to <newName>_token.json.
func RenameAccount(path, oldName, newName string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	var raw Config
	if err := yaml.NewDecoder(f).Decode(&raw); err != nil {
		f.Close()
		return fmt.Errorf("parsing config: %w", err)
	}
	f.Close()

	found := false
	for i := range raw.Accounts {
		if raw.Accounts[i].Name == oldName {
			raw.Accounts[i].Name = newName
			if raw.Accounts[i].TokenFile == oldName+"_token.json" {
				raw.Accounts[i].TokenFile = newName + "_token.json"
			}
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("account %q not found in config", oldName)
	}

	out, err := os.OpenFile(path, os.O_RDWR|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	defer out.Close()
	enc := yaml.NewEncoder(out)
	enc.SetIndent(2)
	return enc.Encode(&raw)
}

// AppendAccount reads the config at path, appends acc to the accounts list, and writes it back.
// It re-reads from disk so env-var overrides (OLLAMA_TOKEN etc.) are never written to the file.
func AppendAccount(path string, acc Account) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	var raw Config
	if err := yaml.NewDecoder(f).Decode(&raw); err != nil {
		f.Close()
		return fmt.Errorf("parsing config: %w", err)
	}
	f.Close()

	raw.Accounts = append(raw.Accounts, acc)

	// Add the new account name to any user that has an explicit accounts list.
	for i := range raw.Users {
		if len(raw.Users[i].Accounts) > 0 {
			raw.Users[i].Accounts = append(raw.Users[i].Accounts, acc.Name)
		}
	}

	out, err := os.OpenFile(path, os.O_RDWR|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	defer out.Close()
	enc := yaml.NewEncoder(out)
	enc.SetIndent(2)
	return enc.Encode(&raw)
}

// RemoveAccount reads the config at path, removes the named account, and writes it back.
// Returns the removed Account so the caller can clean up token files etc.
func RemoveAccount(path, name string) (Account, error) {
	f, err := os.Open(path)
	if err != nil {
		return Account{}, fmt.Errorf("reading config: %w", err)
	}
	var raw Config
	if err := yaml.NewDecoder(f).Decode(&raw); err != nil {
		f.Close()
		return Account{}, fmt.Errorf("parsing config: %w", err)
	}
	f.Close()

	var removed Account
	found := false
	filtered := make([]Account, 0, len(raw.Accounts))
	for _, acc := range raw.Accounts {
		if acc.Name == name {
			removed = acc
			found = true
		} else {
			filtered = append(filtered, acc)
		}
	}
	if !found {
		return Account{}, fmt.Errorf("account %q not found in config", name)
	}
	raw.Accounts = filtered

	for i := range raw.Users {
		updated := make([]string, 0, len(raw.Users[i].Accounts))
		for _, n := range raw.Users[i].Accounts {
			if n != name {
				updated = append(updated, n)
			}
		}
		raw.Users[i].Accounts = updated
	}

	out, err := os.OpenFile(path, os.O_RDWR|os.O_TRUNC, 0600)
	if err != nil {
		return Account{}, fmt.Errorf("writing config: %w", err)
	}
	defer out.Close()
	enc := yaml.NewEncoder(out)
	enc.SetIndent(2)
	return removed, enc.Encode(&raw)
}

func LoadPreferences(path string) (*Preferences, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Preferences{Digest: DigestOpts{MaxPreviewLength: 150, VIPFirst: true}}, nil
		}
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	var prefs Preferences
	if err := yaml.NewDecoder(f).Decode(&prefs); err != nil {
		return nil, fmt.Errorf("parsing preferences: %w", err)
	}
	if prefs.Digest.MaxPreviewLength == 0 {
		prefs.Digest.MaxPreviewLength = 150
	}
	return &prefs, nil
}

// EffectiveUsers returns the configured users, filling in defaultPrefsPath where no preferences path is set.
func (cfg *Config) EffectiveUsers(defaultPrefsPath string) []User {
	for i := range cfg.Users {
		if cfg.Users[i].Preferences == "" {
			cfg.Users[i].Preferences = defaultPrefsPath
		}
	}
	return cfg.Users
}

func SavePreferences(path string, prefs *Preferences) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("writing preferences %s: %w", path, err)
	}
	defer f.Close()
	enc := yaml.NewEncoder(f)
	enc.SetIndent(2)
	return enc.Encode(prefs)
}

// EnsureUserIDs generates a UUID for any user missing one, updates the slice in place,
// and persists the change back to the config file. Returns true if any IDs were generated.
func EnsureUserIDs(path string, users []User) (bool, error) {
	changed := false
	for i := range users {
		if users[i].ID == "" {
			users[i].ID = newUUID()
			changed = true
		}
	}
	if !changed {
		return false, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return true, fmt.Errorf("reading config: %w", err)
	}
	var raw Config
	err = yaml.NewDecoder(f).Decode(&raw)
	f.Close()
	if err != nil {
		return true, fmt.Errorf("parsing config: %w", err)
	}

	// Match by MattermostUser (the only stable key before IDs existed).
	for i := range raw.Users {
		for _, u := range users {
			if raw.Users[i].MattermostUser == u.MattermostUser {
				raw.Users[i].ID = u.ID
				break
			}
		}
	}

	out, err := os.OpenFile(path, os.O_RDWR|os.O_TRUNC, 0600)
	if err != nil {
		return true, fmt.Errorf("writing config: %w", err)
	}
	defer out.Close()
	enc := yaml.NewEncoder(out)
	enc.SetIndent(2)
	return true, enc.Encode(&raw)
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
