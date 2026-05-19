package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Accounts     []Account  `yaml:"accounts"`
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
	CredentialsFile string `yaml:"credentials_file"`
	TokenFile       string `yaml:"token_file"`
}

type Mattermost struct {
	BotToken    string `yaml:"bot_token"`
	ServerURL   string `yaml:"server_url"`
	DMUser      string `yaml:"dm_user"`
	Username    string `yaml:"username"`
	CallbackURL string `yaml:"callback_url"` // base URL Mattermost can reach us at, e.g. http://10.10.0.14:8090
	Port        int    `yaml:"port"`          // local port to listen on, default 8090
}

type Ollama struct {
	Enabled bool   `yaml:"enabled"`
	Host    string `yaml:"host"`
	Model   string `yaml:"model"`
}

type Preferences struct {
	VIPSenders   []string   `yaml:"vip_senders"`
	MuteSenders  []string   `yaml:"mute_senders"`
	MuteSubjects []string   `yaml:"mute_subjects"`
	Categories   []Category `yaml:"categories"`
	Digest       DigestOpts `yaml:"digest"`
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
	if cfg.Ollama.Host == "" {
		cfg.Ollama.Host = "http://localhost:11434"
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
