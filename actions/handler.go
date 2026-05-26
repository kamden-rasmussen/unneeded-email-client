package actions

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kamden/emailagent/config"
	"github.com/kamden/emailagent/email"
	"github.com/kamden/emailagent/notify"
	"github.com/kamden/emailagent/storage"
)

// Handler handles Mattermost interactive button callbacks and dialog submissions.
type Handler struct {
	Clients       map[string]email.Actioner
	MMClient      *notify.Mattermost // fallback / single-user
	UserMMClients map[string]*notify.Mattermost
	CallbackURL   string
	DB            *storage.DB // for recording user-confirmed sender→label mappings
	WebhookSecret string
	ConfigPath    string
	// AddAccount is called when a new account is fully set up; it should persist
	// the account to config and update any live state in main (e.g. suggesters).
	AddAccount func(acc config.Account, client email.Actioner) error
	// RenameAccount is called to persist a rename and update live state in main.
	RenameAccount func(oldName, newName string) error
	// NextChunk is called when a user clicks "load next chunk"; it fetches and
	// posts the next page of emails for the given account starting at offset.
	NextChunk     func(user, account string, offset int) error
	pendingOAuths  sync.Map // state string → *pendingOAuth
	pendingFilters sync.Map // token string → pendingFilter
}

type pendingFilter struct {
	Filter    config.Filter
	ExpiresAt time.Time
	Confirm   func() error
}

// getMMForUser returns the Mattermost client for the given user, falling back to MMClient.
func (h *Handler) getMMForUser(user string) *notify.Mattermost {
	if user != "" {
		if mm, ok := h.UserMMClients[user]; ok {
			return mm
		}
	}
	return h.MMClient
}

// Register wires up HTTP routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/actions/email", h.handleEmailAction)
	mux.HandleFunc("/actions/move_dialog", h.handleMoveDialog)
	mux.HandleFunc("/setup/gmail/callback", h.handleGmailCallback)
	mux.HandleFunc("/setup/imap/start", h.handleIMAPStart)
	mux.HandleFunc("/setup/imap/callback", h.handleIMAPCallback)
}

// StartGmailReauth generates an OAuth2 authorization URL for re-authorizing a Gmail account.
// refresh is called after the new token is saved; its return value replaces the live client entry.
func (h *Handler) StartGmailReauth(accountName, tokenFile string, refresh func() (email.Actioner, error)) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	state := hex.EncodeToString(b)
	redirectURL := h.CallbackURL + "/setup/gmail/callback"
	authURL, err := email.GmailAuthURL(redirectURL, state)
	if err != nil {
		return "", err
	}
	h.pendingOAuths.Store(state, &pendingOAuth{
		accountName: accountName,
		tokenFile:   tokenFile,
		expiresAt:   time.Now().Add(10 * time.Minute),
		refresh:     refresh,
	})
	return authURL, nil
}

// --- button action ---

type buttonPayload struct {
	PostID    string        `json:"post_id"`
	TriggerID string        `json:"trigger_id"`
	UserName  string        `json:"user_name"`
	Context   actionContext `json:"context"`
}

type actionContext struct {
	Action        string `json:"action"`
	User          string `json:"user,omitempty"`
	MsgID         string `json:"msg_id"`
	Account       string `json:"account"`
	EmailID       string `json:"email_id"`
	Number        int    `json:"number"`
	Offset        int    `json:"offset,omitempty"`
	LabelID       string `json:"label_id,omitempty"`
	LabelName     string `json:"label_name,omitempty"`
	SenderDomain  string `json:"sender_domain,omitempty"`
	FilterToken   string `json:"filter_token,omitempty"`
	WebhookSecret string `json:"webhook_secret,omitempty"`
}

type buttonResponse struct {
	Update        *buttonUpdate `json:"update,omitempty"`
	EphemeralText string        `json:"ephemeral_text,omitempty"`
}

type buttonUpdate struct {
	Props map[string]any `json:"props"`
}

func (h *Handler) handleEmailAction(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var p buttonPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if !h.validSecret(p.Context.WebhookSecret) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Verify the Mattermost-supplied user matches the user embedded in the context.
	// This prevents a forged request from acting as a different user even if the
	// webhook_secret is known.
	if p.Context.User != "" && p.UserName != "" && p.UserName != p.Context.User {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ctx := p.Context
	client, ok := h.Clients[ctx.Account]
	if !ok {
		respondButton(w, buttonResponse{EphemeralText: "unknown account: " + ctx.Account})
		return
	}

	switch ctx.Action {
	case "archive":
		if err := client.MarkRead(ctx.MsgID); err != nil {
			log.Printf("mark_read before archive %s: %v", ctx.EmailID, err)
		}
		if err := client.Archive(ctx.MsgID); err != nil {
			log.Printf("archive %s: %v", ctx.EmailID, err)
			respondButton(w, buttonResponse{EphemeralText: "Error: " + err.Error()})
			return
		}
		log.Printf("archived %s", ctx.EmailID)
		mm := h.getMMForUser(ctx.User)
		props, _ := markedDoneProps(mm, p.PostID, ctx.Number, "Archived")
		resp := buttonResponse{EphemeralText: "Archived ✓"}
		if props != nil {
			resp.Update = &buttonUpdate{Props: props}
		}
		respondButton(w, resp)

	case "mark_read":
		if err := client.MarkRead(ctx.MsgID); err != nil {
			log.Printf("mark_read %s: %v", ctx.EmailID, err)
			respondButton(w, buttonResponse{EphemeralText: "Error: " + err.Error()})
			return
		}
		log.Printf("marked read %s", ctx.EmailID)
		mm := h.getMMForUser(ctx.User)
		props, _ := markedDoneProps(mm, p.PostID, ctx.Number, "Marked Read")
		resp := buttonResponse{EphemeralText: "Marked Read ✓"}
		if props != nil {
			resp.Update = &buttonUpdate{Props: props}
		}
		respondButton(w, resp)

	case "move_direct":
		// Suggested label — one-click move with no dialog.
		if err := client.MoveToLabel(ctx.MsgID, ctx.LabelID); err != nil {
			log.Printf("move_direct %s: %v", ctx.EmailID, err)
			respondButton(w, buttonResponse{EphemeralText: "Error: " + err.Error()})
			return
		}
		log.Printf("moved %s to %s (suggested)", ctx.EmailID, ctx.LabelName)
		// Record user confirmation to improve future suggestions.
		if h.DB != nil && ctx.SenderDomain != "" {
			h.DB.SetSenderSuggestion(ctx.User, ctx.SenderDomain, ctx.LabelID, ctx.LabelName, "user") //nolint:errcheck
		}
		mm := h.getMMForUser(ctx.User)
		props, _ := markedDoneProps(mm, p.PostID, ctx.Number, "Moved to "+ctx.LabelName)
		resp := buttonResponse{EphemeralText: "Moved to " + ctx.LabelName + " ✓"}
		if props != nil {
			resp.Update = &buttonUpdate{Props: props}
		}
		respondButton(w, resp)

	case "delete":
		if err := client.Delete(ctx.MsgID); err != nil {
			log.Printf("delete %s: %v", ctx.EmailID, err)
			respondButton(w, buttonResponse{EphemeralText: "Error: " + err.Error()})
			return
		}
		log.Printf("deleted %s", ctx.EmailID)
		mm := h.getMMForUser(ctx.User)
		props, _ := markedDoneProps(mm, p.PostID, ctx.Number, "Deleted")
		resp := buttonResponse{EphemeralText: "Deleted ✓"}
		if props != nil {
			resp.Update = &buttonUpdate{Props: props}
		}
		respondButton(w, resp)

	case "move":
		labels, err := client.ListLabels()
		if err != nil {
			respondButton(w, buttonResponse{EphemeralText: "Could not list folders: " + err.Error()})
			return
		}
		if len(labels) == 0 {
			respondButton(w, buttonResponse{EphemeralText: "No custom folders found — create labels in Gmail first."})
			return
		}

		opts := make([]notify.SelectOption, len(labels))
		for i, l := range labels {
			opts[i] = notify.SelectOption{Text: l.Name, Value: l.ID}
		}

		stateJSON, _ := json.Marshal(moveState{
			PostID:        p.PostID,
			MsgID:         ctx.MsgID,
			Account:       ctx.Account,
			EmailID:       ctx.EmailID,
			Number:        ctx.Number,
			User:          ctx.User,
			SenderDomain:  ctx.SenderDomain,
			WebhookSecret: h.WebhookSecret,
		})

		if err := h.getMMForUser(ctx.User).OpenDialog(
			p.TriggerID,
			h.CallbackURL+"/actions/move_dialog",
			"move_email",
			"Move to Folder",
			string(stateJSON),
			[]notify.DialogElement{{
				DisplayName: "Folder",
				Name:        "label_id",
				Type:        "select",
				Options:     opts,
			}},
		); err != nil {
			log.Printf("open dialog: %v", err)
			respondButton(w, buttonResponse{EphemeralText: "Could not open folder picker: " + err.Error()})
			return
		}
		// Empty response — Mattermost will show the dialog.
		respondButton(w, buttonResponse{})

	case "next_chunk":
		if h.NextChunk == nil {
			respondButton(w, buttonResponse{EphemeralText: "Load more is not configured."})
			return
		}
		respondButton(w, buttonResponse{EphemeralText: "Loading next emails for " + ctx.Account + "…"})
		mm := h.getMMForUser(ctx.User)
		go func() {
			if err := h.NextChunk(ctx.User, ctx.Account, ctx.Offset); err != nil {
				log.Printf("next_chunk %s offset %d: %v", ctx.Account, ctx.Offset, err)
				mm.PostMessage("Error loading next chunk: " + err.Error()) //nolint:errcheck
			}
		}()

	case "approve_filter", "approve_filter_apply":
		val, ok := h.pendingFilters.LoadAndDelete(ctx.FilterToken)
		if !ok {
			respondButton(w, buttonResponse{EphemeralText: "This filter approval has expired or was already handled."})
			return
		}
		pf := val.(pendingFilter)
		if time.Now().After(pf.ExpiresAt) {
			respondButton(w, buttonResponse{EphemeralText: "This filter approval has expired."})
			return
		}
		if err := pf.Confirm(); err != nil {
			log.Printf("confirm filter: %v", err)
			respondButton(w, buttonResponse{EphemeralText: "Error saving filter: " + err.Error()})
			return
		}
		resultText := "Filter saved ✓\n\n" + filterDescription(pf.Filter)
		if ctx.Action == "approve_filter_apply" {
			n := h.applyFilterToExisting(ctx.User, pf.Filter)
			if n > 0 {
				resultText += fmt.Sprintf("\n\nApplied to %d existing email(s).", n)
			} else {
				resultText += "\n\nNo matching emails found in current digest."
			}
		}
		respondButton(w, buttonResponse{
			EphemeralText: "Filter saved ✓",
			Update: &buttonUpdate{Props: map[string]any{
				"attachments": []notify.Attachment{{Text: resultText, Color: "#36a64f"}},
			}},
		})

	case "cancel_filter":
		val, ok := h.pendingFilters.LoadAndDelete(ctx.FilterToken)
		if !ok {
			respondButton(w, buttonResponse{EphemeralText: "Already handled."})
			return
		}
		pf := val.(pendingFilter)
		respondButton(w, buttonResponse{
			EphemeralText: "Filter cancelled.",
			Update: &buttonUpdate{Props: map[string]any{
				"attachments": []notify.Attachment{{Text: "~~" + filterDescription(pf.Filter) + "~~ _(cancelled)_"}},
			}},
		})

	default:
		respondButton(w, buttonResponse{EphemeralText: "unknown action: " + ctx.Action})
	}
}

// --- move dialog submission ---

type moveState struct {
	PostID        string `json:"post_id"`
	MsgID         string `json:"msg_id"`
	Account       string `json:"account"`
	EmailID       string `json:"email_id"`
	Number        int    `json:"number"`
	User          string `json:"user,omitempty"`
	SenderDomain  string `json:"sender_domain,omitempty"`
	WebhookSecret string `json:"webhook_secret,omitempty"`
}

type dialogSubmission struct {
	CallbackID string            `json:"callback_id"`
	State      string            `json:"state"`
	Submission map[string]string `json:"submission"`
	Cancelled  bool              `json:"cancelled"`
}

func (h *Handler) handleMoveDialog(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var sub dialogSubmission
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var state moveState
	if err := json.Unmarshal([]byte(sub.State), &state); err != nil {
		respondDialogError(w, "invalid request state")
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

	labelID := sub.Submission["label_id"]
	if labelID == "" {
		respondDialogError(w, "no folder selected")
		return
	}

	client, ok := h.Clients[state.Account]
	if !ok {
		respondDialogError(w, "unknown account: "+state.Account)
		return
	}

	// Look up label name for the confirmation message.
	labelName := labelID
	if labels, err := client.ListLabels(); err == nil {
		for _, l := range labels {
			if l.ID == labelID {
				labelName = l.Name
				break
			}
		}
	}

	if err := client.MoveToLabel(state.MsgID, labelID); err != nil {
		log.Printf("move %s to %s: %v", state.EmailID, labelID, err)
		respondDialogError(w, "Error moving email: "+err.Error())
		return
	}

	log.Printf("moved %s to %s", state.EmailID, labelName)

	// Record user-confirmed mapping so future digests can suggest this label.
	if h.DB != nil && state.SenderDomain != "" {
		h.DB.SetSenderSuggestion(state.User, state.SenderDomain, labelID, labelName, "user") //nolint:errcheck
	}

	// Update the original digest post to remove buttons and show the action taken.
	if state.PostID != "" {
		mm := h.getMMForUser(state.User)
		if err := updatePostDone(mm, state.PostID, state.Number, "Moved to "+labelName); err != nil {
			log.Printf("update post %s: %v", state.PostID, err)
		}
	}

	// Empty JSON body signals success to Mattermost.
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, "{}")
}

// --- filter approval ---

// ProposeFilter sends a draft filter card to the user with Approve / Approve+Apply / Cancel buttons.
func (h *Handler) ProposeFilter(user string, f config.Filter, mm *notify.Mattermost, confirm func() error) error {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	token := hex.EncodeToString(b)

	h.pendingFilters.Store(token, pendingFilter{
		Filter:    f,
		ExpiresAt: time.Now().Add(10 * time.Minute),
		Confirm:   confirm,
	})

	base := map[string]any{
		"user":           user,
		"filter_token":   token,
		"webhook_secret": h.WebhookSecret,
	}
	mkCtx := func(action string) map[string]any {
		ctx := make(map[string]any, len(base)+1)
		for k, v := range base {
			ctx[k] = v
		}
		ctx["action"] = action
		return ctx
	}

	att := notify.Attachment{
		Text: "Add this filter?\n\n" + filterDescription(f),
		Actions: []notify.Action{
			{
				ID:    "fa" + token[:6],
				Name:  "Approve",
				Type:  "button",
				Style: "success",
				Integration: &notify.Integration{URL: h.CallbackURL + "/actions/email", Context: mkCtx("approve_filter")},
			},
			{
				ID:    "faa" + token[:6],
				Name:  "Approve + Apply to existing",
				Type:  "button",
				Style: "primary",
				Integration: &notify.Integration{URL: h.CallbackURL + "/actions/email", Context: mkCtx("approve_filter_apply")},
			},
			{
				ID:    "fc" + token[:6],
				Name:  "Cancel",
				Type:  "button",
				Style: "danger",
				Integration: &notify.Integration{URL: h.CallbackURL + "/actions/email", Context: mkCtx("cancel_filter")},
			},
		},
	}
	return mm.PostAttachment("", att)
}

func filterDescription(f config.Filter) string {
	desc := strings.Join(f.Actions, " + ")
	if f.LabelName != "" {
		desc += " → " + f.LabelName
	}
	return fmt.Sprintf("Emails from **%s** → %s", f.Sender, desc)
}

// applyFilterToExisting applies filter actions to any digest emails currently in the DB for the user.
// Returns the number of emails acted on.
func (h *Handler) applyFilterToExisting(user string, f config.Filter) int {
	if h.DB == nil {
		return 0
	}
	emails, err := h.DB.GetAllDigestEmails(user)
	if err != nil {
		log.Printf("apply filter to existing: %v", err)
		return 0
	}

	labelCache := make(map[string][]email.Label)
	count := 0
	for _, de := range emails {
		if !email.MatchesSender(de.FromAddr, f.Sender) {
			continue
		}
		client, ok := h.Clients[de.Account]
		if !ok {
			continue
		}
		for _, act := range f.Actions {
			switch act {
			case "archive":
				client.Archive(de.MsgID) //nolint:errcheck
			case "mark_read":
				client.MarkRead(de.MsgID) //nolint:errcheck
			case "mute":
				// no-op: email is already in the digest
			case "move":
				if f.LabelName == "" {
					break
				}
				if _, ok := labelCache[de.Account]; !ok {
					if lbls, err := client.ListLabels(); err == nil {
						labelCache[de.Account] = lbls
					}
				}
				for _, l := range labelCache[de.Account] {
					if strings.EqualFold(l.Name, f.LabelName) {
						client.MoveToLabel(de.MsgID, l.ID) //nolint:errcheck
						break
					}
				}
			}
		}
		count++
	}
	return count
}

// --- helpers ---

// markedDoneProps returns updated props for a button response "update".
// Uses the cached attachment data so other buttons retain their integration contexts
// (Mattermost strips integration.context from GetPost responses).
func markedDoneProps(mm *notify.Mattermost, postID string, number int, label string) (map[string]any, error) {
	_, atts, ok := mm.GetDigestPost(postID)
	if !ok {
		return nil, fmt.Errorf("post %s not in digest cache", postID)
	}

	for i, att := range atts {
		if attachmentOwnsNumber(att, number) {
			if att.Text != "" {
				atts[i].Text = att.Text + "\n\n**" + label + " ✓**"
			} else {
				atts[i].Text = "**" + label + " ✓**"
			}
			atts[i].Actions = nil
			break
		}
	}

	mm.UpdateDigestPost(postID, atts)
	return map[string]any{"attachments": atts}, nil
}

// updatePostDone patches the post directly (used after dialog submissions, which
// cannot return an "update" in their response).
func updatePostDone(mm *notify.Mattermost, postID string, number int, label string) error {
	message, atts, ok := mm.GetDigestPost(postID)
	if !ok {
		return fmt.Errorf("post %s not in digest cache", postID)
	}

	for i, att := range atts {
		if attachmentOwnsNumber(att, number) {
			if att.Text != "" {
				atts[i].Text = att.Text + "\n\n**" + label + " ✓**"
			} else {
				atts[i].Text = "**" + label + " ✓**"
			}
			atts[i].Actions = nil
			break
		}
	}

	mm.UpdateDigestPost(postID, atts)
	return mm.PatchPost(postID, message, atts)
}

// attachmentOwnsNumber checks whether the attachment contains actions for the given digest number.
// Action IDs are formatted as "{2-char-prefix}{number}" (e.g. "ar1", "rd2").
func attachmentOwnsNumber(att notify.Attachment, number int) bool {
	numStr := strconv.Itoa(number)
	for _, a := range att.Actions {
		if len(a.ID) > 2 && a.ID[2:] == numStr {
			return true
		}
	}
	return false
}

// validSecret returns true if no secret is configured, or if the provided value
// matches using a constant-time comparison to prevent timing attacks.
func (h *Handler) validSecret(provided string) bool {
	if h.WebhookSecret == "" {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(h.WebhookSecret)) == 1
}

func respondButton(w http.ResponseWriter, resp buttonResponse) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp) //nolint:errcheck
}

func respondDialogError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"error": msg}) //nolint:errcheck
}
