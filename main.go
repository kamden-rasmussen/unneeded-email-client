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
	"sync"
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

// userCtx holds all per-user state: Mattermost channel, email clients, preferences, and AI processor.
type userCtx struct {
	username   string
	mm         *notify.Mattermost
	prefs      *config.Preferences
	prefsPath  string
	proc       *ai.Processor // carries per-user prefs for IsVIP/IsMuted/Categorize
	clients    map[string]email.Actioner
	suggesters map[string]email.Suggester
	counters   map[string]email.Counter
	accounts   []config.Account
	digestMu   sync.Mutex
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "auth" {
		runAuth(os.Args[2:])
		return
	}

	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	db, err := storage.Open(cfg.Database)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer db.Close()

	users := cfg.EffectiveUsers(*prefsPath)
	if len(users) == 0 {
		log.Fatal("no users configured — add a 'users' section or set mattermost.dm_user")
	}
	if generated, err := config.EnsureUserIDs(*configPath, users); err != nil {
		log.Printf("warning: could not persist user IDs: %v", err)
	} else if generated {
		log.Printf("generated UUIDs for new user(s) — saved to config")
	}

	userCtxs := buildUserContexts(cfg, users)

	// Union of all clients across users for button-callback routing.
	allClients := make(map[string]email.Actioner)
	userMMMap := make(map[string]*notify.Mattermost)
	for _, u := range users {
		uctx := userCtxs[u.ID]
		for name, c := range uctx.clients {
			allClients[name] = c
		}
		userMMMap[u.ID] = uctx.mm
	}

	var ah *actions.Handler
	if cfg.Mattermost.CallbackURL != "" {
		// Use the first user's MM as the fallback for posts that lack a user context.
		fallbackMM := userCtxs[users[0].ID].mm
		ah = &actions.Handler{
			Clients:       allClients,
			MMClient:      fallbackMM,
			UserMMClients: userMMMap,
			CallbackURL:   cfg.Mattermost.CallbackURL,
			DB:            db,
			WebhookSecret: cfg.Mattermost.WebhookSecret,
			ConfigPath:    *configPath,
			AddAccount: func(acc config.Account, client email.Actioner) error {
				if err := config.AppendAccount(*configPath, acc); err != nil {
					return err
				}
				cfg.Accounts = append(cfg.Accounts, acc)
				allClients[acc.Name] = client
				return nil
			},
			RenameAccount: func(oldName, newName string) error {
				oldToken := oldName + "_token.json"
				newToken := newName + "_token.json"
				if _, err := os.Stat(oldToken); err == nil {
					os.Rename(oldToken, newToken) //nolint:errcheck
				}
				for i := range cfg.Accounts {
					if cfg.Accounts[i].Name == oldName {
						cfg.Accounts[i].Name = newName
						if cfg.Accounts[i].TokenFile == oldToken {
							cfg.Accounts[i].TokenFile = newToken
						}
						break
					}
				}
				if c, ok := allClients[oldName]; ok {
					allClients[newName] = c
					delete(allClients, oldName)
				}
				return config.RenameAccount(*configPath, oldName, newName)
			},
			NextChunk: func(user, account string, offset int) error {
				uctx, ok := userCtxs[user]
				if !ok {
					return fmt.Errorf("unknown user %q", user)
				}
				return loadNextChunk(uctx, account, offset, db)
			},
			TriggerReauth: func(accountName string) {
				for _, uctx := range userCtxs {
					for _, acc := range uctx.accounts {
						if acc.Name != accountName {
							continue
						}
						tokenFile := acc.TokenFile
						if tokenFile == "" {
							tokenFile = acc.Name + "_token.json"
						}
						url, err := ah.StartGmailReauth(acc.Name, tokenFile, func() (email.Actioner, error) {
							return email.NewGmailClient(acc)
						})
						if err != nil {
							log.Printf("trigger reauth for %s: %v", accountName, err)
							return
						}
						uctx.mm.PostMessage(fmt.Sprintf( //nolint:errcheck
							"⚠️ Gmail account **%s** needs re-authorization to manage filters.\n\n[Click here to re-authorize](%s)\n\n_Link expires in 10 minutes._",
							accountName, url,
						))
						return
					}
				}
			},
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

	// Start a poll loop and wire a digest function for each user.
	for _, u := range users {
		uctx := userCtxs[u.ID]
		digestFn := func(accountFilter string) error {
			return digest(uctx, cfg, db, ah, accountFilter)
		}
		executeFn := func(text string) (string, error) {
			return executeCommand(text, uctx, db, ah, digestFn)
		}
		go pollLoop(cfg.PollInterval, db, uctx.mm, uctx.username, executeFn)
	}

	runAll := func() {
		for _, uctx := range userCtxs {
			if err := digest(uctx, cfg, db, ah, ""); err != nil {
				log.Printf("[%s] digest: %v", uctx.username, err)
			}
		}
	}

	if *runNow {
		runAll()
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		log.Println("digest sent — polling for commands (Ctrl+C to quit)")
		<-sig
		return
	}

	c := cron.New()
	if _, err := c.AddFunc(cfg.Schedule, runAll); err != nil {
		log.Fatalf("invalid schedule %q: %v", cfg.Schedule, err)
	}
	c.Start()

	log.Printf("email agent running — %d user(s), schedule: %s", len(userCtxs), cfg.Schedule)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	c.Stop()
	log.Println("shutting down")
}

func buildUserContexts(cfg *config.Config, users []config.User) map[string]*userCtx {
	accountByName := make(map[string]config.Account)
	for _, a := range cfg.Accounts {
		accountByName[a.Name] = a
	}

	ctxs := make(map[string]*userCtx)
	for _, u := range users {
		var userAccounts []config.Account
		if len(u.Accounts) == 0 {
			userAccounts = cfg.Accounts
		} else {
			for _, name := range u.Accounts {
				if acc, ok := accountByName[name]; ok {
					userAccounts = append(userAccounts, acc)
				} else {
					log.Printf("[%s] account %q not found in config", u.ID, name)
				}
			}
		}

		prefs, err := config.LoadPreferences(u.Preferences)
		if err != nil {
			log.Printf("[%s] preferences: %v — using defaults", u.ID, err)
			prefs = &config.Preferences{Digest: config.DigestOpts{MaxPreviewLength: 150, VIPFirst: true}}
		}

		clients := buildActionClients(userAccounts)
		ctxs[u.ID] = &userCtx{
			username:   u.ID,
			mm:         notify.NewMattermostForUser(cfg.Mattermost, u.MattermostUser),
			prefs:      prefs,
			prefsPath:  u.Preferences,
			proc:       ai.New(cfg.Ollama, prefs),
			clients:    clients,
			suggesters: buildSuggesters(clients),
			counters:   buildCounters(clients),
			accounts:   userAccounts,
		}
	}
	return ctxs
}

func digest(uctx *userCtx, cfg *config.Config, db *storage.DB, ah *actions.Handler, accountFilter string) error {
	if !uctx.digestMu.TryLock() {
		log.Printf("[%s] digest already in progress, skipping", uctx.username)
		return nil
	}
	defer uctx.digestMu.Unlock()

	t0 := time.Now()
	log.Printf("[%s] running digest...", uctx.username)
	now := t0

	reauthFn := func(acc config.Account) {
		if ah == nil {
			log.Printf("[%s] token expired but no callback_url configured for re-auth", acc.Name)
			return
		}
		tokenFile := acc.TokenFile
		if tokenFile == "" {
			tokenFile = acc.Name + "_token.json"
		}
		url, err := ah.StartGmailReauth(acc.Name, tokenFile, func() (email.Actioner, error) {
			return email.NewGmailClient(acc)
		})
		if err != nil {
			log.Printf("start reauth for %s: %v", acc.Name, err)
			return
		}
		uctx.mm.PostMessage(fmt.Sprintf( //nolint:errcheck
			"⚠️ Gmail token for **%s** has expired.\n\n[Click here to re-authorize](%s)\n\n_Link expires in 10 minutes._",
			acc.Name, url,
		))
	}

	var newEmails []email.Email
	totalCounts := make(map[string]int)
	nextOffsets := make(map[string]int)

	for _, acc := range uctx.accounts {
		if accountFilter != "" && !strings.EqualFold(acc.Name, accountFilter) {
			continue
		}
		tFetch := time.Now()
		emails, err := fetchAccount(acc, db)
		if err != nil {
			log.Printf("[%s] fetch error: %v", acc.Name, err)
			if strings.Contains(err.Error(), "invalid_grant") {
				reauthFn(acc)
			}
			continue
		}
		log.Printf("[%s] fetch: %d emails in %s", acc.Name, len(emails), time.Since(tFetch).Round(time.Millisecond))

		if c, ok := uctx.counters[acc.Name]; ok {
			if n, err := c.InboxCount(); err == nil {
				totalCounts[acc.Name] = n
			} else {
				log.Printf("[%s] inbox count: %v", acc.Name, err)
			}
		}
		if totalCounts[acc.Name] == 0 {
			totalCounts[acc.Name] = len(emails)
		}

		tAI := time.Now()
		accountShown := 0
		for i := range emails {
			if accountShown >= maxPerAccountConst {
				break
			}
			e := &emails[i]
			if uctx.proc.IsMuted(e) {
				continue
			}
			e.User = uctx.username
			e.VIP = uctx.proc.IsVIP(e)
			e.Category = uctx.proc.Categorize(e)
			e.Preview = uctx.proc.Summarize(e)
			newEmails = append(newEmails, *e)
			accountShown++
		}
		if accountShown >= maxPerAccountConst {
			nextOffsets[acc.Name] = maxPerAccountConst
		}
		log.Printf("[%s] AI enrichment: %s", acc.Name, time.Since(tAI).Round(time.Millisecond))

		if err := db.SetLastRun(acc.Name, now); err != nil {
			log.Printf("[%s] set last run: %v", acc.Name, err)
		}
	}

	tSuggest := time.Now()
	for i := range newEmails {
		e := &newEmails[i]
		domain := email.SenderDomain(e.FromAddr)
		if domain == "" {
			continue
		}
		if sug, err := db.GetSenderSuggestion(uctx.username, domain); err == nil && sug != nil {
			e.Suggestion = &email.Label{ID: sug.LabelID, Name: sug.LabelName}
			continue
		}
		if s, ok := uctx.suggesters[e.Account]; ok {
			if label, err := s.InferLabel(domain); err == nil && label != nil {
				e.Suggestion = label
				db.SetSenderSuggestion(uctx.username, domain, label.ID, label.Name, "inferred") //nolint:errcheck
			}
		}
	}
	log.Printf("[%s] label suggestions: %s", uctx.username, time.Since(tSuggest).Round(time.Millisecond))

	de := make([]storage.DigestEmail, len(newEmails))
	for i := range newEmails {
		newEmails[i].Number = i + 1
		de[i] = storage.DigestEmail{
			Number:   i + 1,
			EmailID:  newEmails[i].ID,
			Account:  newEmails[i].Account,
			MsgID:    newEmails[i].MsgID,
			Subject:  newEmails[i].Subject,
			From:     newEmails[i].From,
			FromAddr: newEmails[i].FromAddr,
		}
	}
	if err := db.SaveDigestEmails(uctx.username, de); err != nil {
		log.Printf("[%s] saving digest emails: %v", uctx.username, err)
	}

	tSend := time.Now()
	for acc, total := range totalCounts {
		shown := 0
		for _, e := range newEmails {
			if e.Account == acc {
				shown++
			}
		}
		log.Printf("[%s] digest: showing %d of %d", acc, shown, total)
	}

	summary := uctx.proc.SummarizeDigest(newEmails)
	if err := uctx.mm.SendDigest(newEmails, totalCounts, nextOffsets, summary); err != nil {
		return err
	}
	log.Printf("[%s] send: %s, total: %s", uctx.username, time.Since(tSend).Round(time.Millisecond), time.Since(t0).Round(time.Millisecond))
	return nil
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

func buildCounters(clients map[string]email.Actioner) map[string]email.Counter {
	out := make(map[string]email.Counter)
	for name, c := range clients {
		if cnt, ok := c.(email.Counter); ok {
			out[name] = cnt
		}
	}
	return out
}

const (
	maxPerAccountConst      = 25
	maxFetchPerAccountConst = 50
)

func loadNextChunk(uctx *userCtx, account string, offset int, db *storage.DB) error {
	client, ok := uctx.clients[account]
	if !ok {
		return fmt.Errorf("no client for account %q", account)
	}
	paginator, ok := client.(email.Paginator)
	if !ok {
		return fmt.Errorf("account %q does not support pagination", account)
	}

	fetched, err := paginator.FetchFrom(offset, maxFetchPerAccountConst)
	if err != nil {
		return fmt.Errorf("fetching chunk for %s: %w", account, err)
	}
	if len(fetched) == 0 {
		return uctx.mm.PostMessage(fmt.Sprintf("No more emails to load for **%s**.", account))
	}

	var chunk []email.Email
	for i := range fetched {
		if len(chunk) >= maxPerAccountConst {
			break
		}
		e := &fetched[i]
		if uctx.proc.IsMuted(e) {
			continue
		}
		e.User = uctx.username
		e.VIP = uctx.proc.IsVIP(e)
		e.Category = uctx.proc.Categorize(e)
		e.Preview = uctx.proc.Summarize(e)
		chunk = append(chunk, *e)
	}

	if len(chunk) == 0 {
		return uctx.mm.PostMessage(fmt.Sprintf("No more emails to load for **%s**.", account))
	}

	maxNum, err := db.MaxDigestNumber(uctx.username)
	if err != nil {
		return err
	}
	de := make([]storage.DigestEmail, len(chunk))
	for i := range chunk {
		chunk[i].Number = maxNum + i + 1
		de[i] = storage.DigestEmail{
			Number:   chunk[i].Number,
			EmailID:  chunk[i].ID,
			Account:  chunk[i].Account,
			MsgID:    chunk[i].MsgID,
			Subject:  chunk[i].Subject,
			From:     chunk[i].From,
			FromAddr: chunk[i].FromAddr,
		}
	}
	if err := db.AppendDigestEmails(uctx.username, de); err != nil {
		log.Printf("append digest emails: %v", err)
	}

	for i := range chunk {
		e := &chunk[i]
		domain := email.SenderDomain(e.FromAddr)
		if domain == "" {
			continue
		}
		if sug, err := db.GetSenderSuggestion(uctx.username, domain); err == nil && sug != nil {
			e.Suggestion = &email.Label{ID: sug.LabelID, Name: sug.LabelName}
			continue
		}
		if s, ok := uctx.suggesters[e.Account]; ok {
			if label, err := s.InferLabel(domain); err == nil && label != nil {
				e.Suggestion = label
				db.SetSenderSuggestion(uctx.username, domain, label.ID, label.Name, "inferred") //nolint:errcheck
			}
		}
	}

	var nextOffsets map[string]int
	if len(chunk) >= maxPerAccountConst {
		nextOffsets = map[string]int{account: offset + maxPerAccountConst}
	}

	return uctx.mm.SendDigest(chunk, nil, nextOffsets, "")
}

func buildActionClients(accounts []config.Account) map[string]email.Actioner {
	clients := make(map[string]email.Actioner)
	for _, acc := range accounts {
		var (
			c   email.Actioner
			err error
		)
		switch acc.Type {
		case "gmail":
			c, err = email.NewGmailClient(acc)
		case "imap":
			c, err = email.NewIMAPClient(acc)
		default:
			continue
		}
		if err != nil {
			log.Printf("action client for %s: %v", acc.Name, err)
			continue
		}
		clients[acc.Name] = c
	}
	return clients
}

// pollLoop checks the DM channel on an interval for commands from the user.
func pollLoop(intervalSec int, db *storage.DB, mm *notify.Mattermost, username string, executeFn func(string) (string, error)) {
	ticker := time.NewTicker(time.Duration(intervalSec) * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if err := processPoll(db, mm, username, executeFn); err != nil {
			log.Printf("[%s] poll: %v", username, err)
		}
	}
}

func processPoll(db *storage.DB, mm *notify.Mattermost, username string, executeFn func(string) (string, error)) error {
	cursor, err := db.GetPollCursor(username)
	if err != nil {
		return err
	}
	if cursor == 0 {
		return db.SetPollCursor(username, time.Now().UnixMilli())
	}
	messages, maxSeen, err := mm.GetNewMessages(cursor)
	if err != nil {
		return err
	}
	log.Printf("[%s] poll: cursor=%d maxSeen=%d messages=%d", username, cursor, maxSeen, len(messages))
	// Advance the cursor using Mattermost's own timestamps to avoid clock-skew duplicates.
	if maxSeen > cursor {
		if err := db.SetPollCursor(username, maxSeen); err != nil {
			return err
		}
	}
	digestQueued := false
	for _, msg := range messages {
		text := strings.TrimSpace(msg.Message)
		if isDigestCommand(text) {
			if digestQueued {
				log.Printf("[%s] skipping duplicate digest command %q", username, text)
				continue
			}
			digestQueued = true
		}
		reply, err := executeFn(text)
		if err != nil {
			log.Printf("[%s] command %q: %v", username, text, err)
			mm.PostMessage("Error: " + err.Error()) //nolint:errcheck
			continue
		}
		if reply != "" {
			mm.PostMessage(reply) //nolint:errcheck
		}
	}
	return nil
}

// isDigestCommand reports whether text triggers a full email digest fetch.
func isDigestCommand(text string) bool {
	fields := strings.Fields(strings.ToLower(text))
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "check", "digest":
		return true
	case "list":
		return len(fields) == 1 || fields[1] != "accounts"
	}
	return false
}

// executeCommand parses and runs a command. Known keywords are handled directly;
// anything else is sent to the local LLM for natural-language interpretation.
func executeCommand(text string, uctx *userCtx, db *storage.DB, ah *actions.Handler, digestFn func(string) error) (string, error) {
	fields := strings.Fields(strings.ToLower(text))
	if len(fields) == 0 {
		return "", nil
	}

	action := fields[0]

	if action == "check" || action == "digest" {
		return "", digestFn("")
	}

	if action == "list" {
		if len(fields) > 1 && fields[1] == "accounts" {
			return listAccountsText(uctx.accounts), nil
		}
		return "", digestFn("")
	}

	if action == "accounts" {
		return listAccountsText(uctx.accounts), nil
	}

	if action == "help" {
		return commandHelp(), nil
	}

	if action == "add" {
		if len(fields) < 3 {
			return "Usage: `add gmail <name>` · `add imap <name>` · `add icloud <name>`", nil
		}
		if ah == nil {
			return "Account setup requires `callback_url` to be configured.", nil
		}
		ah.HandleAddCommand(fields[1], fields[2])
		return "", nil
	}

	if action == "rename" {
		if len(fields) < 3 {
			return "Usage: `rename <old-name> <new-name>`", nil
		}
		if ah == nil {
			return "Rename requires `callback_url` to be configured.", nil
		}
		ah.HandleRenameCommand(fields[1], fields[2])
		return "", nil
	}

	if action == "archive" || action == "read" || action == "done" || action == "delete" {
		if len(fields) < 2 {
			return fmt.Sprintf("Usage: `%s <number> [number...]` or `%s all`", action, action), nil
		}
		return doEmailAction(action, fields[1:], uctx.username, db, uctx.clients)
	}

	// Natural-language fallback via local LLM.
	if uctx.proc.Enabled() {
		accountNames := make([]string, 0, len(uctx.accounts))
		for _, a := range uctx.accounts {
			accountNames = append(accountNames, a.Name)
		}
		cmd, err := uctx.proc.Interpret(text, accountNames)
		if err != nil {
			log.Printf("[%s] nlp interpret %q: %v", uctx.username, text, err)
			return "", nil
		}
		log.Printf("[%s] nlp: %q → action=%s account=%q numbers=%v all=%v", uctx.username, text, cmd.Action, cmd.Account, cmd.Numbers, cmd.All)
		switch cmd.Action {
		case "digest":
			return "", digestFn(cmd.Account)
		case "list_accounts":
			return listAccountsText(uctx.accounts), nil
		case "archive", "read", "done", "delete":
			var toks []string
			if cmd.All {
				toks = []string{"all"}
			} else {
				for _, n := range cmd.Numbers {
					toks = append(toks, strconv.Itoa(n))
				}
			}
			if len(toks) == 0 {
				return "Couldn't figure out which emails — try: `archive 1 2` or `archive all`", nil
			}
			return doEmailAction(cmd.Action, toks, uctx.username, db, uctx.clients)
		case "add_account":
			if ah == nil {
				return "Account setup requires `callback_url` to be configured.", nil
			}
			accountType := cmd.AccountType
			if accountType == "" {
				accountType = "imap"
			}
			ah.HandleAddCommand(accountType, cmd.AccountName)
			return "", nil
		case "unknown":
			return commandHelp(), nil
		}
	}

	return "", nil
}

// doEmailAction executes archive/read/done/delete against the current digest for a user.
func doEmailAction(action string, toks []string, username string, db *storage.DB, clients map[string]email.Actioner) (string, error) {
	var targets []storage.DigestEmail

	if len(toks) > 0 && toks[0] == "all" {
		all, err := db.GetAllDigestEmails(username)
		if err != nil {
			return "", err
		}
		targets = all
	} else {
		for _, tok := range toks {
			n, err := strconv.Atoi(tok)
			if err != nil {
				continue
			}
			e, err := db.GetDigestEmail(username, n)
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
			if err := client.Archive(e.MsgID); err != nil {
				return "", fmt.Errorf("archiving [%d]: %w", e.Number, err)
			}
		case "read":
			if err := client.MarkRead(e.MsgID); err != nil {
				return "", fmt.Errorf("marking read [%d]: %w", e.Number, err)
			}
		case "done":
			client.Archive(e.MsgID)  //nolint:errcheck
			client.MarkRead(e.MsgID) //nolint:errcheck
		case "delete":
			if err := client.Delete(e.MsgID); err != nil {
				return "", fmt.Errorf("deleting [%d]: %w", e.Number, err)
			}
		}
		done = append(done, fmt.Sprintf("[%d] %s — %s", e.Number, e.From, e.Subject))
	}

	if len(done) == 0 {
		return "", nil
	}

	verb := map[string]string{"archive": "Archived", "read": "Marked read", "done": "Done", "delete": "Deleted"}[action]
	return fmt.Sprintf("%s ✓\n%s", verb, strings.Join(done, "\n")), nil
}

func listAccountsText(accounts []config.Account) string {
	if len(accounts) == 0 {
		return "No accounts configured."
	}
	lines := make([]string, 0, len(accounts))
	for _, a := range accounts {
		if a.Email != "" {
			lines = append(lines, fmt.Sprintf("- **%s** — %s (%s)", a.Name, a.Email, a.Type))
		} else {
			lines = append(lines, fmt.Sprintf("- **%s** (%s)", a.Name, a.Type))
		}
	}
	return "**Accounts:**\n" + strings.Join(lines, "\n")
}

func commandHelp() string {
	return "**Commands:**\n" +
		"- `check` / `digest` / `list` — fetch new emails\n" +
		"- `accounts` / `list accounts` — show configured email accounts\n" +
		"- `archive <N> [N...]` / `archive all`\n" +
		"- `read <N> [N...]` / `read all`\n" +
		"- `delete <N> [N...]`\n" +
		"- `done <N> [N...]` / `done all` — archive + mark read\n" +
		"- `add gmail <name>` — add a Gmail account\n" +
		"- `add imap <name>` — add an IMAP account\n" +
		"- `add icloud <name>` — add an iCloud account\n" +
		"- `rename <old> <new>` — rename an account\n" +
		"\nTo create filters, use the **Create Filter...** button on any email card."
}

// runAuth handles the `email-agent auth [--port N] <type> <name>` subcommand.
func runAuth(args []string) {
	fs := flag.NewFlagSet("auth", flag.ExitOnError)
	port := fs.Int("port", 0, "fixed callback port (use with SSH tunneling or Docker: -L <port>:localhost:<port>)")
	fs.Parse(args) //nolint:errcheck
	rest := fs.Args()

	if len(rest) < 2 {
		log.Fatal("usage: email-agent auth [--port N] gmail <account-name>")
	}
	accountType, name := rest[0], rest[1]

	switch accountType {
	case "gmail":
		tokenFile := name + "_token.json"
		fmt.Printf("Authenticating Gmail account %q — token will be saved to %s\n", name, tokenFile)
		if *port > 0 {
			fmt.Printf("Listening on port %d (forward this port if on a remote machine)\n", *port)
		}
		if err := email.RunGmailAuth(tokenFile, *port); err != nil {
			log.Fatalf("auth: %v", err)
		}
		fmt.Printf("\nAdd this to your config.yaml:\n\n  - name: %s\n    type: gmail\n    email: you@gmail.com\n", name)
	default:
		log.Fatalf("unknown account type %q — supported: gmail", accountType)
	}
}
