package ai

import (
	"encoding/json"
	"fmt"
	"strings"
)

// InterpretedCommand is the structured result of natural-language command parsing.
type InterpretedCommand struct {
	Action        string   `json:"action"`
	Account       string   `json:"account,omitempty"`
	Numbers       []int    `json:"numbers,omitempty"`
	All           bool     `json:"all,omitempty"`
	Sender        string   `json:"sender,omitempty"`
	FilterActions []string `json:"filter_actions,omitempty"`
	Label         string   `json:"label,omitempty"`
	AccountType   string   `json:"account_type,omitempty"`
	AccountName   string   `json:"account_name,omitempty"`
}

// Enabled reports whether AI processing is turned on.
func (p *Processor) Enabled() bool {
	return p.cfg.Enabled
}

// Interpret sends message to the local LLM and parses it into a command.
// accounts is shown to the model so it can resolve account references.
func (p *Processor) Interpret(message string, accounts []string) (*InterpretedCommand, error) {
	accountList := strings.Join(accounts, ", ")
	if accountList == "" {
		accountList = "(none)"
	}
	prompt := fmt.Sprintf(
		"You are a command parser for an email assistant. Parse the user message into exactly one JSON command.\n\n"+
			"Available commands:\n"+
			`{"action":"digest"} — fetch and show new emails for all accounts`+"\n"+
			`{"action":"digest","account":"NAME"} — fetch emails for one account only`+"\n"+
			`{"action":"list_accounts"} — list configured email accounts and addresses`+"\n"+
			`{"action":"archive","numbers":[N,...]} — archive emails by digest number`+"\n"+
			`{"action":"archive","all":true} — archive all emails in the digest`+"\n"+
			`{"action":"read","numbers":[N,...]} — mark emails as read`+"\n"+
			`{"action":"read","all":true} — mark all emails as read`+"\n"+
			`{"action":"delete","numbers":[N,...]} — delete emails by digest number`+"\n"+
			`{"action":"done","numbers":[N,...]} — archive and mark read by number`+"\n"+
			`{"action":"done","all":true} — archive and mark read all emails`+"\n"+
			`{"action":"add_filter","sender":"domain.com","filter_actions":["archive"]}`+" — auto-filter future emails from sender; filter_actions can include archive, mark_read, mute, move; add \"label\":\"Name\" for move\n"+
			`{"action":"list_filters"} — list active auto-filters`+"\n"+
			`{"action":"remove_filter","sender":"domain.com"} — remove an auto-filter`+"\n"+
			`{"action":"add_account","account_type":"imap","account_name":"NAME"} — add an IMAP account; account_type is gmail, imap, or icloud; account_name is a short label chosen by the user or inferred from context (e.g. "outlook", "work"); omit account_name if not mentioned`+"\n"+
			`{"action":"unknown"} — cannot interpret the message`+"\n\n"+
			"Available accounts: %s\n\n"+
			"Reply with ONLY a valid JSON object. No explanation, no markdown fences.\n\n"+
			"Message: %s",
		accountList, message,
	)

	raw, err := p.generate(prompt)
	if err != nil {
		return nil, err
	}
	raw = clean(raw)

	// Extract the first JSON object in case the model adds surrounding text.
	if i := strings.Index(raw, "{"); i > 0 {
		raw = raw[i:]
	}
	if i := strings.LastIndex(raw, "}"); i >= 0 {
		raw = raw[:i+1]
	}

	var cmd InterpretedCommand
	if err := json.Unmarshal([]byte(raw), &cmd); err != nil {
		return nil, fmt.Errorf("parse LLM response %q: %w", raw, err)
	}
	return &cmd, nil
}
