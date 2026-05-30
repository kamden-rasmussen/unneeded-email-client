package actions

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/kamden/emailagent/config"
	"github.com/kamden/emailagent/email"
	"github.com/kamden/emailagent/notify"
)

// HandleRenameCommand processes a "rename <old> <new>" text command from the DM channel.
func (h *Handler) HandleRenameCommand(oldName, newName string) {
	if _, exists := h.Clients[oldName]; !exists {
		h.MMClient.PostMessage(fmt.Sprintf("Account **%s** not found.", oldName)) //nolint:errcheck
		return
	}
	if _, exists := h.Clients[newName]; exists {
		h.MMClient.PostMessage(fmt.Sprintf("Account **%s** already exists.", newName)) //nolint:errcheck
		return
	}

	h.Clients[newName] = h.Clients[oldName]
	delete(h.Clients, oldName)

	if h.RenameAccount != nil {
		if err := h.RenameAccount(oldName, newName); err != nil {
			log.Printf("rename account %s→%s: %v", oldName, newName, err)
			h.MMClient.PostMessage("Error persisting rename: " + err.Error()) //nolint:errcheck
			return
		}
	}

	h.MMClient.PostMessage(fmt.Sprintf("✓ Account renamed **%s** → **%s**.", oldName, newName)) //nolint:errcheck
}

// HandleAddCommand processes an "add <type> <name>" text command from the DM channel.
func (h *Handler) HandleAddCommand(accountType, name string) {
	if _, exists := h.Clients[name]; exists {
		h.MMClient.PostMessage(fmt.Sprintf("Account **%s** already exists.", name)) //nolint:errcheck
		return
	}
	switch accountType {
	case "gmail":
		h.handleAddGmail(name)
	case "imap", "icloud":
		h.handleAddIMAPPrompt(name, accountType)
	default:
		h.MMClient.PostMessage(fmt.Sprintf("Unknown account type **%s**. Supported: `gmail`, `imap`, `icloud`.", accountType)) //nolint:errcheck
	}
}

func (h *Handler) handleAddGmail(name string) {
	tokenFile := name + "_token.json"
	acc := config.Account{Name: name, Type: "gmail", TokenFile: tokenFile}

	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		h.MMClient.PostMessage("Failed to generate authorization link.") //nolint:errcheck
		return
	}
	state := hex.EncodeToString(b)

	redirectURL := h.CallbackURL + "/setup/gmail/callback"
	authURL, err := email.GmailAuthURL(redirectURL, state)
	if err != nil {
		log.Printf("add gmail %s: %v", name, err)
		h.MMClient.PostMessage("Failed to generate authorization link: " + err.Error()) //nolint:errcheck
		return
	}

	h.pendingOAuths.Store(state, &pendingOAuth{
		accountName: name,
		tokenFile:   tokenFile,
		expiresAt:   time.Now().Add(10 * time.Minute),
		refresh:     func() (email.Actioner, error) { return email.NewGmailClient(acc) },
		newAccount:  &acc,
	})

	h.MMClient.PostMessage(fmt.Sprintf( //nolint:errcheck
		"To add Gmail account **%s**, authorize it here:\n\n[Authorize Gmail](%s)\n\n_Link expires in 10 minutes._",
		name, authURL,
	))
}

func (h *Handler) handleAddIMAPPrompt(name, accountType string) {
	label := "IMAP"
	if accountType == "icloud" {
		label = "iCloud"
	}
	ctx := map[string]any{
		"account_name":   name,
		"account_type":   accountType,
		"webhook_secret": h.WebhookSecret,
	}
	err := h.MMClient.SendPromptWithButton(
		fmt.Sprintf("Setting up %s account **%s**. Click below to enter your credentials.", label, name),
		notify.Action{
			ID:          "imap_setup_" + name,
			Name:        "Enter Credentials",
			Type:        "button",
			Style:       "primary",
			Integration: &notify.Integration{URL: h.CallbackURL + "/setup/imap/start", Context: ctx},
		},
	)
	if err != nil {
		log.Printf("send imap prompt: %v", err)
	}
}

// --- /setup/imap/start — button click opens the credentials dialog ---

type imapStartPayload struct {
	TriggerID string `json:"trigger_id"`
	Context   struct {
		AccountName   string `json:"account_name"`
		AccountType   string `json:"account_type"`
		WebhookSecret string `json:"webhook_secret"`
	} `json:"context"`
}

type imapSetupState struct {
	AccountName   string `json:"account_name"`
	AccountType   string `json:"account_type"`
	WebhookSecret string `json:"webhook_secret"`
}

func (h *Handler) handleIMAPStart(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var p imapStartPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !h.validSecret(p.Context.WebhookSecret) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	var elements []notify.DialogElement
	if p.Context.AccountType == "icloud" {
		elements = []notify.DialogElement{
			{DisplayName: "iCloud Email", Name: "email", Type: "text", Placeholder: "you@icloud.com"},
			{DisplayName: "App-Specific Password", Name: "password", Type: "text", Placeholder: "xxxx-xxxx-xxxx-xxxx"},
			{DisplayName: "IMAP Host", Name: "host", Type: "text", Default: "imap.mail.me.com"},
			{DisplayName: "IMAP Port", Name: "port", Type: "text", Default: "993"},
		}
	} else {
		elements = []notify.DialogElement{
			{DisplayName: "Email Address", Name: "email", Type: "text"},
			{DisplayName: "Password", Name: "password", Type: "text"},
			{DisplayName: "IMAP Host", Name: "host", Type: "text"},
			{DisplayName: "IMAP Port", Name: "port", Type: "text", Default: "993"},
		}
	}

	title := "Add IMAP Account"
	if p.Context.AccountType == "icloud" {
		title = "Add iCloud Account"
	}

	stateJSON, _ := json.Marshal(imapSetupState{
		AccountName:   p.Context.AccountName,
		AccountType:   p.Context.AccountType,
		WebhookSecret: h.WebhookSecret,
	})

	if err := h.MMClient.OpenDialog(
		p.TriggerID,
		h.CallbackURL+"/setup/imap/callback",
		"imap_setup",
		title,
		string(stateJSON),
		"Next",
		elements,
	); err != nil {
		log.Printf("open imap dialog: %v", err)
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, "{}")
}

// --- /setup/imap/callback — dialog submission ---

func (h *Handler) handleIMAPCallback(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var sub dialogSubmission
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var state imapSetupState
	if err := json.Unmarshal([]byte(sub.State), &state); err != nil {
		respondDialogError(w, "invalid state")
		return
	}
	if !h.validSecret(state.WebhookSecret) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if sub.Cancelled {
		w.WriteHeader(http.StatusOK)
		return
	}

	emailAddr := subStr(sub.Submission, "email")
	password := subStr(sub.Submission, "password")
	host := subStr(sub.Submission, "host")
	portStr := subStr(sub.Submission, "port")

	if emailAddr == "" || password == "" || host == "" {
		respondDialogError(w, "Email, password, and host are required.")
		return
	}

	port := 993
	if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
		port = p
	}

	acc := config.Account{
		Name:     state.AccountName,
		Type:     "imap",
		Email:    emailAddr,
		Password: password,
		Host:     host,
		Port:     port,
	}

	// Test the connection before committing.
	testClient, err := email.NewIMAPClient(acc)
	if err != nil {
		log.Printf("imap setup %s: connection test failed: %v", state.AccountName, err)
		respondDialogError(w, "Could not connect — check your credentials and host: "+err.Error())
		return
	}
	testClient.Close() //nolint:errcheck

	// Create the live client to put in the in-memory map.
	liveClient, err := email.NewIMAPClient(acc)
	if err != nil {
		respondDialogError(w, "Connection verified but failed to create live client: "+err.Error())
		return
	}

	h.Clients[acc.Name] = liveClient

	if h.AddAccount != nil {
		if err := h.AddAccount(acc, liveClient); err != nil {
			log.Printf("persist imap account %s: %v", acc.Name, err)
		}
	}

	log.Printf("added %s account %q (%s)", state.AccountType, state.AccountName, emailAddr)
	h.MMClient.PostMessage(fmt.Sprintf("✓ Account **%s** (%s) added successfully.", state.AccountName, emailAddr)) //nolint:errcheck

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, "{}")
}
