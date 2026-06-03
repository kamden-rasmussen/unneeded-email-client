package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kamden/emailagent/config"
	"github.com/kamden/emailagent/email"
)

type digestCacheEntry struct {
	message string
	atts    []Attachment
}

type Mattermost struct {
	cfg         config.Mattermost
	dmUser      string // DM target; overrides cfg.DMUser when set
	http        *http.Client
	channelID   string
	botID       string
	digestCache sync.Map // postID → digestCacheEntry
}

type Post struct {
	ID      string
	UserID  string
	Message string
}

// Attachment is a Mattermost message attachment card.
type Attachment struct {
	Color   string   `json:"color,omitempty"`
	Title   string   `json:"title,omitempty"`
	Text    string   `json:"text,omitempty"`
	Footer  string   `json:"footer,omitempty"`
	Actions []Action `json:"actions,omitempty"`
}

// Action is a button inside an attachment.
type Action struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Type        string       `json:"type,omitempty"`
	Style       string       `json:"style,omitempty"` // "primary", "success", "danger", etc.
	Integration *Integration `json:"integration,omitempty"`
}

// Integration holds the callback URL and context for a button action.
type Integration struct {
	URL     string         `json:"url"`
	Context map[string]any `json:"context"`
}

// DialogElement is a form element in a Mattermost interactive dialog.
type DialogElement struct {
	DisplayName string         `json:"display_name"`
	Name        string         `json:"name"`
	Type        string         `json:"type"`
	Default     string         `json:"default,omitempty"`
	Placeholder string         `json:"placeholder,omitempty"`
	Optional    bool           `json:"optional,omitempty"`
	Options     []SelectOption `json:"options,omitempty"`
}

// SelectOption is a single option in a dialog select element.
type SelectOption struct {
	Text  string `json:"text"`
	Value string `json:"value"`
}

func NewMattermost(cfg config.Mattermost) *Mattermost {
	return &Mattermost{
		cfg:    cfg,
		dmUser: cfg.DMUser,
		http:   &http.Client{Timeout: 10 * time.Second},
	}
}

// NewMattermostForUser returns a Mattermost client that DMs a specific user.
func NewMattermostForUser(cfg config.Mattermost, dmUser string) *Mattermost {
	return &Mattermost{
		cfg:    cfg,
		dmUser: dmUser,
		http:   &http.Client{Timeout: 10 * time.Second},
	}
}

// SendDigest posts the digest. Uses per-email attachment cards with buttons when
// CallbackURL is configured; otherwise falls back to a plain text message.
// totalCounts maps account name → total inbox message count for "X of Y" header display.
// nextOffsets maps account name → inbox offset for the "load next chunk" button; omit or nil to suppress.
func (m *Mattermost) SendDigest(emails []email.Email, totalCounts map[string]int, nextOffsets map[string]int) error {
	header := fmt.Sprintf("### Email Digest — %s", time.Now().Format("Monday, January 2"))

	if len(emails) == 0 {
		return m.PostMessage(header + "\n\nNo new emails.")
	}

	if m.cfg.CallbackURL == "" {
		return m.PostMessage(formatDigest(emails))
	}

	header += fmt.Sprintf("\n\n**%d new email(s)**  _Reply with `archive 1 2`, `read 3`, `done all` if buttons aren't working_", len(emails))

	// sessionTotal is the highest email number in this batch; used for [N/total] display.
	sessionTotal := emails[len(emails)-1].Number

	// Count distinct accounts and how many emails we're showing per account.
	shownPerAccount := make(map[string]int)
	for _, e := range emails {
		shownPerAccount[e.Account]++
	}
	multiAccount := len(shownPerAccount) > 1

	// Show account headers if multi-account, or if any account is capped (showing fewer than total inbox).
	showHeaders := multiAccount
	if !showHeaders {
		for acc, shown := range shownPerAccount {
			if total := totalCounts[acc]; total > 0 && shown < total {
				showHeaders = true
				break
			}
		}
		if !showHeaders {
			for acc := range nextOffsets {
				if nextOffsets[acc] > 0 {
					showHeaders = true
					break
				}
			}
		}
	}

	var atts []Attachment
	var lastAccount string
	for i, e := range emails {
		if showHeaders && e.Account != lastAccount {
			shown := shownPerAccount[e.Account]
			total := totalCounts[e.Account]
			var headerText string
			if total > 0 && shown < total {
				headerText = fmt.Sprintf("**%s** · %d of %d", e.Account, shown, total)
			} else {
				headerText = "**" + e.Account + "**"
			}
			atts = append(atts, Attachment{
				Text:  headerText,
				Color: "#4A9EE8",
			})
			lastAccount = e.Account
		}
		atts = append(atts, buildEmailAttachment(e, m.cfg.CallbackURL, m.cfg.WebhookSecret, sessionTotal))

		// After the last email of a capped account, insert the "load next chunk" button.
		isLastForAccount := i == len(emails)-1 || emails[i+1].Account != e.Account
		if isLastForAccount {
			if offset, ok := nextOffsets[e.Account]; ok && offset > 0 {
				atts = append(atts, buildNextChunkAttachment(e.User, e.Account, offset, m.cfg.CallbackURL, m.cfg.WebhookSecret))
			}
		}
	}

	_, err := m.postWithAttachments(header, atts)
	return err
}

func buildNextChunkAttachment(user, account string, offset int, callbackURL, webhookSecret string) Attachment {
	return Attachment{
		Color: "#888888",
		Actions: []Action{{
			ID:    "nc" + strconv.Itoa(offset),
			Name:  "Load next 25",
			Type:  "button",
			Style: "primary",
			Integration: &Integration{
				URL: callbackURL + "/actions/email",
				Context: map[string]any{
					"action":         "next_chunk",
					"user":           user,
					"account":        account,
					"offset":         offset,
					"webhook_secret": webhookSecret,
				},
			},
		}},
	}
}

func buildEmailAttachment(e email.Email, callbackURL, webhookSecret string, sessionTotal int) Attachment {
	actionURL := callbackURL + "/actions/email"
	base := map[string]any{
		"user":           e.User,
		"msg_id":         e.MsgID,
		"account":        e.Account,
		"email_id":       e.ID,
		"number":         e.Number,
		"from_addr":      e.FromAddr,
		"sender_domain":  email.SenderDomain(e.FromAddr),
		"webhook_secret": webhookSecret,
	}

	mkCtx := func(action string) map[string]any {
		ctx := make(map[string]any, len(base)+1)
		for k, v := range base {
			ctx[k] = v
		}
		ctx["action"] = action
		return ctx
	}

	var color string
	switch {
	case e.VIP:
		color = "#FFD700"
	case e.Unread:
		color = "#1976D2"
	}

	title := fmt.Sprintf("[%d/%d] %s — %s", e.Number, sessionTotal, e.Subject, e.From)

	// Action IDs are number-based so they work for any account type (Gmail or IMAP).
	// Mattermost's router requires alphanumeric-only action IDs.
	n := strconv.Itoa(e.Number)

	var acts []Action

	if e.Suggestion != nil {
		sugCtx := mkCtx("move_direct")
		sugCtx["label_id"] = e.Suggestion.ID
		sugCtx["label_name"] = e.Suggestion.Name
		acts = append(acts, Action{
			ID:          "sg" + n,
			Name:        "→ " + e.Suggestion.Name,
			Type:        "button",
			Style:       "primary",
			Integration: &Integration{URL: actionURL, Context: sugCtx},
		})
	}

	readToggle := Action{Type: "button"}
	if e.Unread {
		readToggle.ID = "rd" + n
		readToggle.Name = "Mark Read"
		readToggle.Integration = &Integration{URL: actionURL, Context: mkCtx("mark_read")}
	} else {
		readToggle.ID = "ur" + n
		readToggle.Name = "Mark Unread"
		readToggle.Integration = &Integration{URL: actionURL, Context: mkCtx("mark_unread")}
	}

	acts = append(acts,
		Action{
			ID:          "ar" + n,
			Name:        "Archive",
			Type:        "button",
			Integration: &Integration{URL: actionURL, Context: mkCtx("archive")},
		},
		readToggle,
		Action{
			ID:          "mo" + n,
			Name:        "Move...",
			Type:        "button",
			Integration: &Integration{URL: actionURL, Context: mkCtx("move")},
		},
		Action{
			ID:          "dl" + n,
			Name:        "Delete",
			Type:        "button",
			Style:       "danger",
			Integration: &Integration{URL: actionURL, Context: mkCtx("delete")},
		},
		Action{
			ID:          "rf" + n,
			Name:        "Read Full Email",
			Type:        "button",
			Integration: &Integration{URL: actionURL, Context: mkCtx("read_full")},
		},
		Action{
			ID:          "cf" + n,
			Name:        "Create Filter...",
			Type:        "button",
			Integration: &Integration{URL: actionURL, Context: mkCtx("open_filter_dialog")},
		},
	)

	att := Attachment{
		Color:   color,
		Title:   title,
		Footer:  fmt.Sprintf("%s · %s", e.Account, relativeTime(e.Date)),
		Actions: acts,
	}

	if e.Preview != "" {
		att.Text = "_" + e.Preview + "_"
	}

	return att
}

// PostFullEmail posts a single attachment card and registers it in the digest cache.
// Returns the new post ID so callers can update it later via button actions.
func (m *Mattermost) PostFullEmail(message string, att Attachment) (string, error) {
	return m.postWithAttachments(message, []Attachment{att})
}

// SendPromptWithButton posts a message with a single action button.
func (m *Mattermost) SendPromptWithButton(message string, action Action) error {
	_, err := m.postWithAttachments(message, []Attachment{{Actions: []Action{action}}})
	return err
}

// PostAttachment posts a message with one or more attachment cards.
func (m *Mattermost) PostAttachment(message string, atts ...Attachment) error {
	_, err := m.postWithAttachments(message, atts)
	return err
}

// PostMessage sends a plain text message to the DM channel.
func (m *Mattermost) PostMessage(text string) error {
	return m.createPost(text)
}

// GetNewMessages returns messages from the user (not the bot) since sinceMillis.
func (m *Mattermost) GetNewMessages(sinceMillis int64) ([]Post, error) {
	if err := m.resolveChannelID(); err != nil {
		return nil, err
	}
	if err := m.resolveBotID(); err != nil {
		return nil, err
	}

	resp, err := m.apiGet(fmt.Sprintf("/channels/%s/posts?since=%d", m.channelID, sinceMillis))
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var result struct {
		Order []string `json:"order"`
		Posts map[string]struct {
			ID      string `json:"id"`
			UserID  string `json:"user_id"`
			Message string `json:"message"`
		} `json:"posts"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	var posts []Post
	for _, id := range result.Order {
		p := result.Posts[id]
		if p.UserID == m.botID {
			continue
		}
		posts = append(posts, Post{ID: p.ID, UserID: p.UserID, Message: p.Message})
	}
	return posts, nil
}

// GetPost returns the raw post map for updating attachments.
func (m *Mattermost) GetPost(postID string) (map[string]any, error) {
	resp, err := m.apiGet("/posts/" + postID)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var post map[string]any
	return post, json.Unmarshal(body, &post)
}

// PatchPost updates a post's message and attachment props in-place.
func (m *Mattermost) PatchPost(postID, message string, attachments []Attachment) error {
	body := map[string]any{
		"message": message,
		"props": map[string]any{
			"attachments": attachments,
		},
	}
	resp, err := m.apiPut("/posts/"+postID+"/patch", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mattermost patch %d: %s", resp.StatusCode, b)
	}
	return nil
}

// OpenDialog opens a Mattermost interactive dialog triggered by a button click.
func (m *Mattermost) OpenDialog(triggerID, submitURL, callbackID, title, state, submitLabel string, elements []DialogElement) error {
	body := map[string]any{
		"trigger_id": triggerID,
		"url":        submitURL,
		"dialog": map[string]any{
			"callback_id":  callbackID,
			"title":        title,
			"state":        state,
			"submit_label": submitLabel,
			"elements":     elements,
		},
	}
	resp, err := m.apiPost("/actions/dialogs/open", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mattermost dialog %d: %s", resp.StatusCode, b)
	}
	return nil
}

// GetDigestPost returns the cached message and attachments for a digest post.
// Attachments are returned as a copy so callers can modify without affecting the cache.
func (m *Mattermost) GetDigestPost(postID string) (message string, atts []Attachment, ok bool) {
	val, loaded := m.digestCache.Load(postID)
	if !loaded {
		return "", nil, false
	}
	e := val.(digestCacheEntry)
	cp := make([]Attachment, len(e.atts))
	copy(cp, e.atts)
	return e.message, cp, true
}

// UpdateDigestPost writes modified attachments back into the digest cache.
func (m *Mattermost) UpdateDigestPost(postID string, atts []Attachment) {
	val, ok := m.digestCache.Load(postID)
	if !ok {
		return
	}
	e := val.(digestCacheEntry)
	e.atts = atts
	m.digestCache.Store(postID, e)
}

func (m *Mattermost) postWithAttachments(message string, atts []Attachment) (string, error) {
	if err := m.resolveChannelID(); err != nil {
		return "", err
	}
	body := map[string]any{
		"channel_id": m.channelID,
		"message":    message,
		"props": map[string]any{
			"attachments": atts,
		},
	}
	resp, err := m.apiPost("/posts", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("mattermost %d: %s", resp.StatusCode, b)
	}
	var post struct {
		ID string `json:"id"`
	}
	json.Unmarshal(b, &post) //nolint:errcheck
	if post.ID != "" {
		m.digestCache.Store(post.ID, digestCacheEntry{message: message, atts: atts})
	}
	return post.ID, nil
}

func (m *Mattermost) createPost(message string) error {
	if err := m.resolveChannelID(); err != nil {
		return err
	}
	body := map[string]any{
		"channel_id": m.channelID,
		"message":    message,
	}
	resp, err := m.apiPost("/posts", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mattermost %d: %s", resp.StatusCode, b)
	}
	return nil
}

func (m *Mattermost) resolveChannelID() error {
	if m.channelID != "" {
		return nil
	}
	id, err := m.openDMChannel()
	if err != nil {
		return fmt.Errorf("opening DM with %q: %w", m.dmUser, err)
	}
	m.channelID = id
	return nil
}

func (m *Mattermost) resolveBotID() error {
	if m.botID != "" {
		return nil
	}
	id, err := m.myUserID()
	if err != nil {
		return err
	}
	m.botID = id
	return nil
}

func (m *Mattermost) openDMChannel() (string, error) {
	botID, err := m.myUserID()
	if err != nil {
		return "", fmt.Errorf("getting bot user ID: %w", err)
	}
	targetID, err := m.userIDByUsername(m.dmUser)
	if err != nil {
		return "", fmt.Errorf("getting user ID for %q: %w", m.dmUser, err)
	}
	resp, err := m.apiPost("/channels/direct", []string{botID, targetID})
	if err != nil {
		return "", err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("mattermost %d: %s", resp.StatusCode, body)
	}
	var ch struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &ch); err != nil || ch.ID == "" {
		return "", fmt.Errorf("unexpected response: %s", body)
	}
	return ch.ID, nil
}

func (m *Mattermost) myUserID() (string, error) {
	resp, err := m.apiGet("/users/me")
	if err != nil {
		return "", err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var u struct {
		ID string `json:"id"`
	}
	return u.ID, json.Unmarshal(body, &u)
}

func (m *Mattermost) userIDByUsername(username string) (string, error) {
	resp, err := m.apiGet("/users/username/" + username)
	if err != nil {
		return "", err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("user %q not found", username)
	}
	var u struct {
		ID string `json:"id"`
	}
	return u.ID, json.Unmarshal(body, &u)
}

func (m *Mattermost) apiPost(path string, body any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", m.cfg.ServerURL+"/api/v4"+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+m.cfg.BotToken)
	req.Header.Set("Content-Type", "application/json")
	return m.http.Do(req)
}

func (m *Mattermost) apiGet(path string) (*http.Response, error) {
	req, err := http.NewRequest("GET", m.cfg.ServerURL+"/api/v4"+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+m.cfg.BotToken)
	return m.http.Do(req)
}

func (m *Mattermost) apiPut(path string, body any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("PUT", m.cfg.ServerURL+"/api/v4"+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+m.cfg.BotToken)
	req.Header.Set("Content-Type", "application/json")
	return m.http.Do(req)
}

// formatDigest produces a plain-text digest (used when CallbackURL is not set).
func formatDigest(emails []email.Email) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("### Email Digest — %s\n\n", time.Now().Format("Monday, January 2")))

	if len(emails) == 0 {
		sb.WriteString("No new emails.")
		return sb.String()
	}

	sb.WriteString(fmt.Sprintf("**%d new email(s)**\n\n", len(emails)))

	for _, e := range emails {
		sb.WriteString(fmt.Sprintf("**[%d]** **%s** · %s\n", e.Number, e.From, e.Subject))
		if e.Preview != "" {
			sb.WriteString(fmt.Sprintf("_%s_\n", e.Preview))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("---\n")
	sb.WriteString("_Commands: `archive 1 2` · `read 3` · `done 4` · `archive all`_")
	return sb.String()
}

func relativeTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	now := time.Now()
	ay, am, ad := t.Date()
	by, bm, bd := now.Date()
	if ay == by && am == bm && ad == bd {
		return t.Format("3:04 PM")
	}
	return t.Format("Jan 2")
}
