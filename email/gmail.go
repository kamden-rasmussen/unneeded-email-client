package email

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"

	"github.com/kamden/emailagent/config"
)

const (
	gmailClientID     = "195489124467-gp35qrirjndsrgkj7rndg6pme6itj59s.apps.googleusercontent.com"
	gmailClientSecret = "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"
)

func gmailOAuthConfig() *oauth2.Config {
	return &oauth2.Config{
		ClientID:     gmailClientID,
		ClientSecret: gmailClientSecret,
		Scopes:       []string{gmail.GmailModifyScope},
		Endpoint:     google.Endpoint,
	}
}

type GmailClient struct {
	cfg    config.Account
	svc    *gmail.Service
	labels []Label // in-process cache populated on first ListLabels call
}

func NewGmailClient(acc config.Account) (*GmailClient, error) {
	tokenFile := acc.TokenFile
	if tokenFile == "" {
		tokenFile = acc.Name + "_token.json"
	}

	httpClient, err := oauthHTTPClient(gmailOAuthConfig(), tokenFile)
	if err != nil {
		return nil, fmt.Errorf("oauth for %s: %w", acc.Name, err)
	}

	svc, err := gmail.NewService(context.Background(), option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("creating gmail service: %w", err)
	}

	return &GmailClient{cfg: acc, svc: svc}, nil
}

func (g *GmailClient) Name() string { return g.cfg.Name }

func (g *GmailClient) FetchNew(_ time.Time) ([]Email, error) {
	q := "in:inbox"

	var emails []Email
	pageToken := ""

	for {
		req := g.svc.Users.Messages.List("me").Q(q).MaxResults(50)
		if pageToken != "" {
			req = req.PageToken(pageToken)
		}

		r, err := req.Do()
		if err != nil {
			return nil, fmt.Errorf("listing messages: %w", err)
		}

		for _, m := range r.Messages {
			msg, err := g.svc.Users.Messages.Get("me", m.Id).
				Format("metadata").
				MetadataHeaders("From", "Subject", "Date").
				Do()
			if err != nil {
				continue
			}

			e := Email{
				ID:      "gmail-" + m.Id,
				MsgID:   m.Id,
				Account: g.cfg.Name,
				Preview: msg.Snippet,
			}

			for _, h := range msg.Payload.Headers {
				switch strings.ToLower(h.Name) {
				case "from":
					e.From, e.FromAddr = parseFrom(h.Value)
				case "subject":
					e.Subject = h.Value
				case "date":
					e.Date, _ = parseEmailDate(h.Value)
				}
			}
			if e.Date.IsZero() {
				e.Date = time.UnixMilli(msg.InternalDate)
			}

			emails = append(emails, e)
		}

		if r.NextPageToken == "" {
			break
		}
		pageToken = r.NextPageToken
	}

	return emails, nil
}

func (g *GmailClient) Close() error { return nil }

func (g *GmailClient) MoveToLabel(msgID, labelID string) error {
	_, err := g.svc.Users.Messages.Modify("me", msgID, &gmail.ModifyMessageRequest{
		AddLabelIds:    []string{labelID},
		RemoveLabelIds: []string{"INBOX"},
	}).Do()
	return err
}

func (g *GmailClient) ListLabels() ([]Label, error) {
	if g.labels != nil {
		return g.labels, nil
	}
	resp, err := g.svc.Users.Labels.List("me").Do()
	if err != nil {
		return nil, err
	}
	for _, l := range resp.Labels {
		if l.Type != "user" {
			continue
		}
		g.labels = append(g.labels, Label{ID: l.Id, Name: l.Name})
	}
	return g.labels, nil
}

// InferLabel queries Gmail for recent organized messages from senderDomain and
// returns the most common user label on those messages, or nil if none found.
func (g *GmailClient) InferLabel(senderDomain string) (*Label, error) {
	q := fmt.Sprintf("from:@%s -in:inbox -in:sent -in:draft -in:spam -in:trash", senderDomain)
	listResp, err := g.svc.Users.Messages.List("me").Q(q).MaxResults(10).Do()
	if err != nil {
		return nil, err
	}
	if len(listResp.Messages) == 0 {
		return nil, nil
	}

	limit := len(listResp.Messages)
	if limit > 5 {
		limit = 5
	}

	// Count user label occurrences across sampled messages.
	// Gmail user-label IDs always start with "Label_"; system labels are ALL_CAPS.
	counts := make(map[string]int)
	for _, m := range listResp.Messages[:limit] {
		msg, err := g.svc.Users.Messages.Get("me", m.Id).Format("minimal").Do()
		if err != nil {
			continue
		}
		seen := make(map[string]bool)
		for _, id := range msg.LabelIds {
			if !seen[id] && strings.HasPrefix(id, "Label_") {
				counts[id]++
				seen[id] = true
			}
		}
	}

	if len(counts) == 0 {
		return nil, nil
	}

	var bestID string
	var bestCount int
	for id, n := range counts {
		if n > bestCount {
			bestCount = n
			bestID = id
		}
	}

	// Resolve ID to name using the cached label list.
	labels, err := g.ListLabels()
	if err != nil {
		return nil, err
	}
	for i := range labels {
		if labels[i].ID == bestID {
			return &labels[i], nil
		}
	}
	return nil, nil
}

func (g *GmailClient) Archive(msgID string) error {
	_, err := g.svc.Users.Messages.Modify("me", msgID, &gmail.ModifyMessageRequest{
		RemoveLabelIds: []string{"INBOX"},
	}).Do()
	return err
}

func (g *GmailClient) MarkRead(msgID string) error {
	_, err := g.svc.Users.Messages.Modify("me", msgID, &gmail.ModifyMessageRequest{
		RemoveLabelIds: []string{"UNREAD"},
	}).Do()
	return err
}

// oauthHTTPClient loads a saved token or runs the browser auth flow.
func oauthHTTPClient(cfg *oauth2.Config, tokenFile string) (*http.Client, error) {
	tok, err := loadToken(tokenFile)
	if err != nil {
		tok, err = browserAuthFlow(cfg, 0)
		if err != nil {
			return nil, err
		}
		if err := saveToken(tokenFile, tok); err != nil {
			return nil, err
		}
	}
	return cfg.Client(context.Background(), tok), nil
}

func loadToken(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	return tok, json.NewDecoder(f).Decode(tok)
}

// browserAuthFlow starts a local HTTP server, opens the browser for consent,
// waits for Google to redirect back with the auth code, then exchanges it.
// port fixes the listener port; pass 0 to pick a random available port.
func browserAuthFlow(cfg *oauth2.Config, port int) (*oauth2.Token, error) {
	addr := "127.0.0.1:0"
	if port > 0 {
		// Bind on all interfaces so Docker port mapping and SSH tunnels work.
		addr = fmt.Sprintf("0.0.0.0:%d", port)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("starting local listener: %w", err)
	}
	port = ln.Addr().(*net.TCPAddr).Port

	// Google Desktop-app credentials accept any localhost port.
	cfg.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d", port)

	state := fmt.Sprintf("state-%d", time.Now().UnixNano())
	authURL := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "invalid state", http.StatusBadRequest)
			errCh <- fmt.Errorf("oauth state mismatch")
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			errCh <- fmt.Errorf("no auth code in callback")
			return
		}
		fmt.Fprint(w, "<html><body><h2>Authorization successful — you can close this tab.</h2></body></html>")
		codeCh <- code
	})

	srv := &http.Server{Handler: mux}
	go srv.Serve(ln) //nolint:errcheck
	defer srv.Close()

	fmt.Printf("Opening browser for Gmail authorization...\n")
	if exec.Command("xdg-open", authURL).Start() != nil {
		exec.Command("open", authURL).Start() //nolint:errcheck
	}
	fmt.Printf("If the browser did not open, visit:\n%s\n", authURL)

	select {
	case code := <-codeCh:
		tok, err := cfg.Exchange(context.Background(), code)
		if err != nil {
			return nil, fmt.Errorf("exchanging code: %w", err)
		}
		return tok, nil
	case err := <-errCh:
		return nil, err
	case <-time.After(5 * time.Minute):
		return nil, fmt.Errorf("authorization timed out")
	}
}

// RunGmailAuth runs the OAuth2 browser flow and saves the token to tokenFile.
// port fixes the callback listener port (useful for SSH tunneling); 0 picks a random port.
func RunGmailAuth(tokenFile string, port int) error {
	tok, err := browserAuthFlow(gmailOAuthConfig(), port)
	if err != nil {
		return err
	}
	if err := saveToken(tokenFile, tok); err != nil {
		return err
	}
	fmt.Printf("Token saved to %s\n", tokenFile)
	return nil
}

func saveToken(path string, tok *oauth2.Token) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("saving token to %s: %w", path, err)
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(tok)
}

func parseFrom(s string) (name, addr string) {
	s = strings.TrimSpace(s)
	open := strings.Index(s, "<")
	close := strings.Index(s, ">")
	if open >= 0 && close > open {
		name = strings.TrimSpace(s[:open])
		addr = strings.TrimSpace(s[open+1 : close])
	} else {
		addr = s
	}
	if name == "" {
		name = addr
	}
	return
}

var emailDateFormats = []string{
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 02 Jan 2006 15:04:05 -0700",
	"2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
}

func parseEmailDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, f := range emailDateFormats {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable date: %s", s)
}
