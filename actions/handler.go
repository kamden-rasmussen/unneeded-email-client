package actions

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/kamden/emailagent/email"
	"github.com/kamden/emailagent/notify"
	"github.com/kamden/emailagent/storage"
)

// Handler handles Mattermost interactive button callbacks and dialog submissions.
type Handler struct {
	Clients     map[string]email.Actioner
	MMClient    *notify.Mattermost
	CallbackURL string
	DB          *storage.DB // for recording user-confirmed sender→label mappings
}

// Register wires up HTTP routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/actions/email", h.handleEmailAction)
	mux.HandleFunc("/actions/move_dialog", h.handleMoveDialog)
}

// --- button action ---

type buttonPayload struct {
	PostID    string        `json:"post_id"`
	TriggerID string        `json:"trigger_id"`
	Context   actionContext `json:"context"`
}

type actionContext struct {
	Action       string `json:"action"`
	GmailID      string `json:"gmail_id"`
	Account      string `json:"account"`
	EmailID      string `json:"email_id"`
	Number       int    `json:"number"`
	LabelID      string `json:"label_id,omitempty"`
	LabelName    string `json:"label_name,omitempty"`
	SenderDomain string `json:"sender_domain,omitempty"`
}

type buttonResponse struct {
	Update        *buttonUpdate `json:"update,omitempty"`
	EphemeralText string        `json:"ephemeral_text,omitempty"`
}

type buttonUpdate struct {
	Props map[string]any `json:"props"`
}

func (h *Handler) handleEmailAction(w http.ResponseWriter, r *http.Request) {
	var p buttonPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
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
		if err := client.Archive(ctx.GmailID); err != nil {
			log.Printf("archive %s: %v", ctx.EmailID, err)
			respondButton(w, buttonResponse{EphemeralText: "Error: " + err.Error()})
			return
		}
		log.Printf("archived %s", ctx.EmailID)
		props, _ := markedDoneProps(h.MMClient, p.PostID, ctx.EmailID, "Archived")
		resp := buttonResponse{EphemeralText: "Archived ✓"}
		if props != nil {
			resp.Update = &buttonUpdate{Props: props}
		}
		respondButton(w, resp)

	case "mark_read":
		if err := client.MarkRead(ctx.GmailID); err != nil {
			log.Printf("mark_read %s: %v", ctx.EmailID, err)
			respondButton(w, buttonResponse{EphemeralText: "Error: " + err.Error()})
			return
		}
		log.Printf("marked read %s", ctx.EmailID)
		props, _ := markedDoneProps(h.MMClient, p.PostID, ctx.EmailID, "Marked Read")
		resp := buttonResponse{EphemeralText: "Marked Read ✓"}
		if props != nil {
			resp.Update = &buttonUpdate{Props: props}
		}
		respondButton(w, resp)

	case "move_direct":
		// Suggested label — one-click move with no dialog.
		if err := client.MoveToLabel(ctx.GmailID, ctx.LabelID); err != nil {
			log.Printf("move_direct %s: %v", ctx.EmailID, err)
			respondButton(w, buttonResponse{EphemeralText: "Error: " + err.Error()})
			return
		}
		log.Printf("moved %s to %s (suggested)", ctx.EmailID, ctx.LabelName)
		// Record user confirmation to improve future suggestions.
		if h.DB != nil && ctx.SenderDomain != "" {
			h.DB.SetSenderSuggestion(ctx.SenderDomain, ctx.LabelID, ctx.LabelName, "user") //nolint:errcheck
		}
		props, _ := markedDoneProps(h.MMClient, p.PostID, ctx.EmailID, "Moved to "+ctx.LabelName)
		resp := buttonResponse{EphemeralText: "Moved to " + ctx.LabelName + " ✓"}
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
			PostID:       p.PostID,
			GmailID:      ctx.GmailID,
			Account:      ctx.Account,
			EmailID:      ctx.EmailID,
			Number:       ctx.Number,
			SenderDomain: ctx.SenderDomain,
		})

		if err := h.MMClient.OpenDialog(
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

	default:
		respondButton(w, buttonResponse{EphemeralText: "unknown action: " + ctx.Action})
	}
}

// --- move dialog submission ---

type moveState struct {
	PostID       string `json:"post_id"`
	GmailID      string `json:"gmail_id"`
	Account      string `json:"account"`
	EmailID      string `json:"email_id"`
	Number       int    `json:"number"`
	SenderDomain string `json:"sender_domain,omitempty"`
}

type dialogSubmission struct {
	CallbackID string            `json:"callback_id"`
	State      string            `json:"state"`
	Submission map[string]string `json:"submission"`
	Cancelled  bool              `json:"cancelled"`
}

func (h *Handler) handleMoveDialog(w http.ResponseWriter, r *http.Request) {
	var sub dialogSubmission
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if sub.Cancelled {
		w.WriteHeader(http.StatusOK)
		return
	}

	var state moveState
	if err := json.Unmarshal([]byte(sub.State), &state); err != nil {
		respondDialogError(w, "invalid request state")
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

	if err := client.MoveToLabel(state.GmailID, labelID); err != nil {
		log.Printf("move %s to %s: %v", state.EmailID, labelID, err)
		respondDialogError(w, "Error moving email: "+err.Error())
		return
	}

	log.Printf("moved %s to %s", state.EmailID, labelName)

	// Record user-confirmed mapping so future digests can suggest this label.
	if h.DB != nil && state.SenderDomain != "" {
		h.DB.SetSenderSuggestion(state.SenderDomain, labelID, labelName, "user") //nolint:errcheck
	}

	// Update the original digest post to remove buttons and show the action taken.
	if state.PostID != "" {
		if err := updatePostDone(h.MMClient, state.PostID, state.EmailID, "Moved to "+labelName); err != nil {
			log.Printf("update post %s: %v", state.PostID, err)
		}
	}

	// Empty JSON body signals success to Mattermost.
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, "{}")
}

// --- helpers ---

// markedDoneProps fetches the post, clears buttons on the matching attachment,
// and returns updated props suitable for inclusion in a button response "update".
func markedDoneProps(mm *notify.Mattermost, postID, emailID, label string) (map[string]any, error) {
	post, err := mm.GetPost(postID)
	if err != nil {
		return nil, err
	}

	props, _ := post["props"].(map[string]any)
	rawAtts, _ := props["attachments"].([]any)

	for i, rawAtt := range rawAtts {
		att, ok := rawAtt.(map[string]any)
		if !ok {
			continue
		}
		if attachmentOwnsEmail(att, emailID) {
			text, _ := att["text"].(string)
			if text != "" {
				att["text"] = text + "\n\n**" + label + " ✓**"
			} else {
				att["text"] = "**" + label + " ✓**"
			}
			att["actions"] = nil
			rawAtts[i] = att
			break
		}
	}

	return map[string]any{"attachments": rawAtts}, nil
}

// updatePostDone patches the post directly (used after dialog submissions, which
// cannot return an "update" in their response).
func updatePostDone(mm *notify.Mattermost, postID, emailID, label string) error {
	post, err := mm.GetPost(postID)
	if err != nil {
		return fmt.Errorf("get post: %w", err)
	}

	props, _ := post["props"].(map[string]any)
	msg, _ := post["message"].(string)
	rawAtts, _ := props["attachments"].([]any)

	for i, rawAtt := range rawAtts {
		att, ok := rawAtt.(map[string]any)
		if !ok {
			continue
		}
		if attachmentOwnsEmail(att, emailID) {
			text, _ := att["text"].(string)
			if text != "" {
				att["text"] = text + "\n\n**" + label + " ✓**"
			} else {
				att["text"] = "**" + label + " ✓**"
			}
			att["actions"] = nil
			rawAtts[i] = att
			break
		}
	}

	return mm.PatchPost(postID, msg, rawAtts)
}

// attachmentOwnsEmail checks whether the attachment contains actions for the given emailID.
// Action IDs are set as "archive__gmail-abc" so we can identify ownership via suffix.
func attachmentOwnsEmail(att map[string]any, emailID string) bool {
	hexID := strings.TrimPrefix(emailID, "gmail-")
	actions, _ := att["actions"].([]any)
	for _, rawAction := range actions {
		action, _ := rawAction.(map[string]any)
		id, _ := action["id"].(string)
		if strings.HasSuffix(id, hexID) {
			return true
		}
	}
	return false
}

func respondButton(w http.ResponseWriter, resp buttonResponse) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp) //nolint:errcheck
}

func respondDialogError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"error": msg}) //nolint:errcheck
}
