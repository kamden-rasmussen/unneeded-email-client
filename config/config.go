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
	Timezone       string   `yaml:"timezone"`      // IANA timezone, e.g. "America/Los_Angeles"
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

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
