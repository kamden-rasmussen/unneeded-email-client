package ai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kamden/emailagent/config"
	"github.com/kamden/emailagent/email"
)

type Processor struct {
	cfg    config.Ollama
	prefs  *config.Preferences
	client *http.Client
}

func New(cfg config.Ollama, prefs *config.Preferences) *Processor {
	return &Processor{
		cfg:    cfg,
		prefs:  prefs,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Categorize uses Ollama when enabled, falls back to keyword matching on error.
func (p *Processor) Categorize(e *email.Email) string {
	if p.cfg.Enabled {
		if cat, err := p.ollamaCategorizate(e); err == nil {
			return cat
		} else {
			log.Printf("ollama categorize: %v — falling back to keywords", err)
		}
	}
	return p.keywordCategorize(e)
}

// Summarize returns a one-sentence AI summary, or the raw preview if Ollama is off/unavailable.
func (p *Processor) Summarize(e *email.Email) string {
	if !p.cfg.Enabled || e.Preview == "" {
		return e.Preview
	}
	prompt := fmt.Sprintf(
		"Summarize this email in one concise sentence (max 20 words). Respond with only the summary sentence, no preamble or explanation.\n\nFrom: %s\nSubject: %s\nContent: %s",
		e.From, e.Subject, e.Preview,
	)
	summary, err := p.generate(prompt)
	if err != nil {
		log.Printf("ollama summarize: %v", err)
		return e.Preview
	}
	return clean(summary)
}

func (p *Processor) IsVIP(e *email.Email) bool {
	addr := strings.ToLower(e.FromAddr)
	for _, vip := range p.prefs.VIPSenders {
		if strings.EqualFold(addr, strings.ToLower(vip)) {
			return true
		}
	}
	return false
}

func (p *Processor) IsMuted(e *email.Email) bool {
	from := strings.ToLower(e.FromAddr)
	for _, s := range p.prefs.MuteSenders {
		if strings.Contains(from, strings.ToLower(s)) {
			return true
		}
	}
	subj := strings.ToLower(e.Subject)
	for _, s := range p.prefs.MuteSubjects {
		if strings.Contains(subj, strings.ToLower(s)) {
			return true
		}
	}
	return false
}

func (p *Processor) ollamaCategorizate(e *email.Email) (string, error) {
	names := p.categoryNames()
	prompt := fmt.Sprintf(
		"Classify this email into exactly one of these categories: %s\n\nFrom: %s\nSubject: %s\n\nRespond with only the category name, nothing else.",
		strings.Join(names, ", "),
		e.From, e.Subject,
	)
	resp, err := p.generate(prompt)
	if err != nil {
		return "", err
	}
	resp = clean(resp)
	for _, name := range names {
		if strings.EqualFold(resp, name) {
			return name, nil
		}
	}
	return "General", nil
}

func (p *Processor) generate(prompt string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":  p.cfg.Model,
		"prompt": prompt,
		"stream": false,
	})
	if err != nil {
		return "", err
	}

	resp, err := p.client.Post(p.cfg.Host+"/api/generate", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("ollama request: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Response string `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("ollama response: %w", err)
	}
	return result.Response, nil
}

func (p *Processor) keywordCategorize(e *email.Email) string {
	subject := strings.ToLower(e.Subject)
	from := strings.ToLower(e.FromAddr)

	for _, cat := range p.prefs.Categories {
		for _, kw := range cat.Keywords {
			if strings.Contains(subject, strings.ToLower(kw)) {
				return cat.Name
			}
		}
		for _, sender := range cat.Senders {
			if strings.Contains(from, strings.ToLower(sender)) {
				return cat.Name
			}
		}
	}
	return "General"
}

func (p *Processor) categoryNames() []string {
	names := make([]string, 0, len(p.prefs.Categories)+1)
	for _, c := range p.prefs.Categories {
		names = append(names, c.Name)
	}
	return append(names, "General")
}

// clean strips thinking tags (qwen3 etc.) and whitespace from LLM output.
func clean(s string) string {
	if start := strings.Index(s, "<think>"); start >= 0 {
		if end := strings.Index(s, "</think>"); end >= 0 {
			s = s[end+len("</think>"):]
		}
	}
	return strings.TrimSpace(s)
}
