package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/kamden/emailagent/actions"
	"github.com/kamden/emailagent/ai"
	"github.com/kamden/emailagent/config"
	"github.com/kamden/emailagent/email"
	"github.com/kamden/emailagent/notify"
	"github.com/kamden/emailagent/storage"
)

var (
	configPath = flag.String("config", "config.yaml", "path to config file")
	prefsPath  = flag.String("prefs", "preferences.yaml", "path to preferences file")
	runNow     = flag.Bool("now", false, "run digest immediately")
)

func main() {
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	prefs, err := config.LoadPreferences(*prefsPath)
	if err != nil {
		log.Fatalf("preferences: %v", err)
	}

	db, err := storage.Open(cfg.Database)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer db.Close()

	notifier := notify.NewMattermost(cfg.Mattermost)
	processor := ai.New(cfg.Ollama, prefs)
	actionClients := buildActionClients(cfg.Accounts)

	// Start the HTTP action server for Mattermost button callbacks.
	if cfg.Mattermost.CallbackURL != "" {
		ah := &actions.Handler{
			Clients:     actionClients,
			MMClient:    notifier,
			CallbackURL: cfg.Mattermost.CallbackURL,
			DB:          db,
		}
		mux := http.NewServeMux()
		ah.Register(mux)
		addr := fmt.Sprintf(":%d", cfg.Mattermost.Port)
		go func() {
			log.Printf("action server listening on %s", addr)
			logged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				log.Printf("HTTP %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
				mux.ServeHTTP(w, r)
			})
			if err := http.ListenAndServe(addr, logged); err != nil {
				log.Printf("action server: %v", err)
			}
		}()
	}

	suggesters := buildSuggesters(actionClients)

	run := func() {
		if err := digest(cfg, db, processor, notifier, suggesters); err != nil {
			log.Printf("digest error: %v", err)
		}
	}

	// Start poll loop — watches DM channel for text commands like "archive 1 2"
	go pollLoop(cfg, db, actionClients, notifier)

	if *runNow {
		run()
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		log.Println("digest sent — polling for commands (Ctrl+C to quit)")
		<-sig
		return
	}

	c := cron.New()
	if _, err := c.AddFunc(cfg.Schedule, run); err != nil {
		log.Fatalf("invalid schedule %q: %v", cfg.Schedule, err)
	}
	c.Start()

	log.Printf("email agent running, schedule: %s", cfg.Schedule)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	c.Stop()
	log.Println("shutting down")
}

func digest(cfg *config.Config, db *storage.DB, proc *ai.Processor, notifier *notify.Mattermost, suggesters map[string]email.Suggester) error {
	log.Println("running digest...")
	now := time.Now()

	var newEmails []email.Email

	for _, acc := range cfg.Accounts {
		emails, err := fetchAccount(acc, db)
		if err != nil {
			log.Printf("[%s] fetch error: %v", acc.Name, err)
			continue
		}

		for i := range emails {
			e := &emails[i]

			if proc.IsMuted(e) {
				continue
			}

			// seen, err := db.IsSeen(e.ID)
			// if err != nil {
			// 	log.Printf("[%s] db lookup error: %v", acc.Name, err)
			// 	continue
			// }
			// if seen {
			// 	continue
			// }

			e.VIP = proc.IsVIP(e)
			e.Category = proc.Categorize(e)
			e.Preview = proc.Summarize(e)

			// if err := db.MarkSeen(e.ID, acc.Name); err != nil {
			// 	log.Printf("[%s] mark seen: %v", acc.Name, err)
			// }

			newEmails = append(newEmails, *e)
		}

		if err := db.SetLastRun(acc.Name, now); err != nil {
			log.Printf("[%s] set last run: %v", acc.Name, err)
		}
	}

	// Enrich each email with a suggested destination label.
	for i := range newEmails {
		e := &newEmails[i]
		domain := email.SenderDomain(e.FromAddr)
		if domain == "" {
			continue
		}
		// DB cache first (avoids Gmail API calls for known senders).
		if sug, err := db.GetSenderSuggestion(domain); err == nil && sug != nil {
			e.Suggestion = &email.Label{ID: sug.LabelID, Name: sug.LabelName}
			continue
		}
		// Infer from Gmail if we have a client for this account.
		if s, ok := suggesters[e.Account]; ok {
			if label, err := s.InferLabel(domain); err == nil && label != nil {
				e.Suggestion = label
				db.SetSenderSuggestion(domain, label.ID, label.Name, "inferred") //nolint:errcheck
			}
		}
	}

	// Assign numbers and save mappings for the poll loop
	de := make([]storage.DigestEmail, len(newEmails))
	for i := range newEmails {
		newEmails[i].Number = i + 1
		de[i] = storage.DigestEmail{
			Number:  i + 1,
			EmailID: newEmails[i].ID,
			Account: newEmails[i].Account,
			GmailID: strings.TrimPrefix(newEmails[i].ID, "gmail-"),
			Subject: newEmails[i].Subject,
			From:    newEmails[i].From,
		}
	}
	if err := db.SaveDigestEmails(de); err != nil {
		log.Printf("saving digest emails: %v", err)
	}

	log.Printf("sending digest: %d new email(s)", len(newEmails))
	if err := notifier.SendDigest(newEmails); err != nil {
		return err
	}

	// Advance the poll cursor so we only process replies after this digest
	return db.SetPollCursor(time.Now().UnixMilli())
}

func fetchAccount(acc config.Account, db *storage.DB) ([]email.Email, error) {
	since, err := db.LastRun(acc.Name)
	if err != nil {
		return nil, fmt.Errorf("last run: %w", err)
	}

	var c email.Client
	switch acc.Type {
	case "gmail":
		c, err = email.NewGmailClient(acc)
	case "imap":
		c, err = email.NewIMAPClient(acc)
	default:
		return nil, fmt.Errorf("unknown account type %q", acc.Type)
	}
	if err != nil {
		return nil, err
	}
	defer c.Close()

	return c.FetchNew(since)
}

func buildSuggesters(clients map[string]email.Actioner) map[string]email.Suggester {
	out := make(map[string]email.Suggester)
	for name, c := range clients {
		if s, ok := c.(email.Suggester); ok {
			out[name] = s
		}
	}
	return out
}

func buildActionClients(accounts []config.Account) map[string]email.Actioner {
	clients := make(map[string]email.Actioner)
	for _, acc := range accounts {
		if acc.Type != "gmail" {
			continue
		}
		c, err := email.NewGmailClient(acc)
		if err != nil {
			log.Printf("action client for %s: %v", acc.Name, err)
			continue
		}
		clients[acc.Name] = c
	}
	return clients
}

// pollLoop checks the DM channel on an interval for commands from the user.
func pollLoop(cfg *config.Config, db *storage.DB, clients map[string]email.Actioner, mm *notify.Mattermost) {
	interval := time.Duration(cfg.PollInterval) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		if err := processPoll(db, clients, mm); err != nil {
			log.Printf("poll: %v", err)
		}
	}
}

func processPoll(db *storage.DB, clients map[string]email.Actioner, mm *notify.Mattermost) error {
	cursor, err := db.GetPollCursor()
	if err != nil || cursor == 0 {
		return err // no digest sent yet
	}

	messages, err := mm.GetNewMessages(cursor)
	if err != nil {
		return err
	}

	now := time.Now().UnixMilli()

	for _, msg := range messages {
		reply, err := executeCommand(strings.TrimSpace(msg.Message), db, clients)
		if err != nil {
			log.Printf("command %q: %v", msg.Message, err)
			mm.PostMessage("Error: " + err.Error()) //nolint:errcheck
			continue
		}
		if reply != "" {
			mm.PostMessage(reply) //nolint:errcheck
		}
	}

	return db.SetPollCursor(now)
}

// executeCommand parses and runs a command like "archive 1 3" or "read 2" or "done all".
func executeCommand(text string, db *storage.DB, clients map[string]email.Actioner) (string, error) {
	fields := strings.Fields(strings.ToLower(text))
	if len(fields) < 2 {
		return "", nil
	}

	action := fields[0]
	if action != "archive" && action != "read" && action != "done" {
		return "", nil // not a command we recognise, ignore
	}

	var targets []storage.DigestEmail

	if fields[1] == "all" {
		all, err := db.GetAllDigestEmails()
		if err != nil {
			return "", err
		}
		targets = all
	} else {
		for _, tok := range fields[1:] {
			n, err := strconv.Atoi(tok)
			if err != nil {
				continue
			}
			e, err := db.GetDigestEmail(n)
			if err != nil {
				continue
			}
			targets = append(targets, e)
		}
	}

	if len(targets) == 0 {
		return "", nil
	}

	var done []string
	for _, e := range targets {
		client, ok := clients[e.Account]
		if !ok {
			continue
		}
		switch action {
		case "archive":
			if err := client.Archive(e.GmailID); err != nil {
				return "", fmt.Errorf("archiving [%d]: %w", e.Number, err)
			}
		case "read":
			if err := client.MarkRead(e.GmailID); err != nil {
				return "", fmt.Errorf("marking read [%d]: %w", e.Number, err)
			}
		case "done":
			client.Archive(e.GmailID)   //nolint:errcheck
			client.MarkRead(e.GmailID) //nolint:errcheck
		}
		done = append(done, fmt.Sprintf("[%d] %s — %s", e.Number, e.From, e.Subject))
	}

	if len(done) == 0 {
		return "", nil
	}

	verb := map[string]string{"archive": "Archived", "read": "Marked read", "done": "Done"}[action]
	return fmt.Sprintf("%s ✓\n%s", verb, strings.Join(done, "\n")), nil
}
