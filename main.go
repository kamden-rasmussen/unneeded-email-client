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

	userCtxs := buildUserContexts(cfg, users)

	// Union of all clients across users for button-callback routing.
	allClients := make(map[string]email.Actioner)
	userMMMap := make(map[string]*notify.Mattermost)
	for _, uctx := range userCtxs {
		for name, c := range uctx.clients {
			allClients[name] = c
		}
		userMMMap[uctx.username] = uctx.mm
	}

	var ah *actions.Handler
	if cfg.Mattermost.CallbackURL != "" {
		// Use the first user's MM as the fallback for posts that lack a user context.
		fallbackMM := userCtxs[users[0].MattermostUser].mm
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
			ConfirmFilter: func(user string, f config.Filter) error {
				uctx, ok := userCtxs[user]
				if !ok {
					return fmt.Errorf("unknown user %q", user)
				}
				uctx.prefs.Filters = append(uctx.prefs.Filters, f)
				return config.SavePreferences(uctx.prefsPath, uctx.prefs)
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
	for _, uctx := range userCtxs {
		uctx := uctx
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
					log.Printf("[%s] account %q not found in config", u.MattermostUser, name)
				}
			}
		}

		prefs, err := config.LoadPreferences(u.Preferences)
		if err != nil {
			log.Printf("[%s] preferences: %v — using defaults", u.MattermostUser, err)
			prefs = &config.Preferences{Digest: config.DigestOpts{MaxPreviewLength: 150, VIPFirst: true}}
		}

		clients := buildActionClients(userAccounts)
		ctxs[u.MattermostUser] = &userCtx{
			username:   u.MattermostUser,
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

		if len(uctx.prefs.Filters) > 0 {
			before := len(emails)
			emails = applyFilters(emails, uctx.prefs.Filters, uctx.clients)
			if n := before - len(emails); n > 0 {
				log.Printf("[%s] filters: auto-processed %d emails", acc.Name, n)
			}
		}

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
	if err := uctx.mm.SendDigest(newEmails, totalCounts, nextOffsets); err != nil {
		return err
	}
	log.Printf("[%s] send: %s, total: %s", uctx.username, time.Since(tSend).Round(time.Millisecond), time.Since(t0).Round(time.Millisecond))

	return db.SetPollCursor(uctx.username, time.Now().UnixMilli())
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

	return uctx.mm.SendDigest(chunk, nil, nextOffsets)
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
	messages, err := mm.GetNewMessages(cursor)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for _, msg := range messages {
		reply, err := executeFn(strings.TrimSpace(msg.Message))
		if err != nil {
			log.Printf("[%s] command %q: %v", username, msg.Message, err)
			mm.PostMessage("Error: " + err.Error()) //nolint:errcheck
			continue
		}
		if reply != "" {
			mm.PostMessage(reply) //nolint:errcheck
		}
	}
	return db.SetPollCursor(username, now)
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

	if action == "filter" || action == "filters" {
		if len(fields) > 1 && fields[1] == "add" {
			if len(fields) < 4 {
				return "Usage: `filter add <sender> archive|mark_read|mute|move <label>`", nil
			}
			filterActions, labelName := parseFilterActions(fields[3:])
			if len(filterActions) == 0 {
				return "Specify at least one action: archive, mark_read, mute, move <label>", nil
			}
			return proposeFilter(config.Filter{Sender: fields[2], Actions: filterActions, LabelName: labelName}, uctx, ah)
		}
		return handleFilterCommand(fields[1:], uctx.prefs, uctx.prefsPath)
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
		case "add_filter":
			if len(cmd.FilterActions) == 0 {
				return "Couldn't determine filter actions — try: `filter add amazon.com archive`", nil
			}
			return proposeFilter(config.Filter{Sender: cmd.Sender, Actions: cmd.FilterActions, LabelName: cmd.Label}, uctx, ah)
		case "list_filters":
			return listFiltersText(uctx.prefs.Filters), nil
		case "remove_filter":
			return removeFilter(cmd.Sender, uctx.prefs, uctx.prefsPath)
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
		"- `filter add <sender> <action...>` — add an auto-filter\n" +
		"- `filter list` — show active filters\n" +
		"- `filter remove <sender>` — remove a filter\n" +
		"- `add gmail <name>` — add a Gmail account\n" +
		"- `rename <old> <new>` — rename an account\n" +
		"\nFilter actions: `archive`, `mark_read`, `mute`, `move <label>`\n" +
		"Or just type naturally: _always archive amazon.com_, _move github.com to Dev_"
}

// --- filter helpers ---

func handleFilterCommand(args []string, prefs *config.Preferences, prefsPath string) (string, error) {
	if len(args) == 0 {
		return listFiltersText(prefs.Filters), nil
	}
	switch args[0] {
	case "list":
		return listFiltersText(prefs.Filters), nil
	case "remove", "delete":
		if len(args) < 2 {
			return "Usage: `filter remove <sender>`", nil
		}
		return removeFilter(args[1], prefs, prefsPath)
	}
	return "Usage: `filter list` · `filter add <sender> <actions>` · `filter remove <sender>`", nil
}

// proposeFilter sends a draft filter to the user for approval.
// Falls back to direct save if no callback URL is configured (no buttons available).
func proposeFilter(f config.Filter, uctx *userCtx, ah *actions.Handler) (string, error) {
	if ah == nil {
		uctx.prefs.Filters = append(uctx.prefs.Filters, f)
		if err := config.SavePreferences(uctx.prefsPath, uctx.prefs); err != nil {
			return "", fmt.Errorf("saving filter: %w", err)
		}
		desc := strings.Join(f.Actions, " + ")
		if f.LabelName != "" {
			desc += " → " + f.LabelName
		}
		return fmt.Sprintf("Filter added: **%s** → %s", f.Sender, desc), nil
	}
	confirm := func() error {
		uctx.prefs.Filters = append(uctx.prefs.Filters, f)
		return config.SavePreferences(uctx.prefsPath, uctx.prefs)
	}
	return "", ah.ProposeFilter(uctx.username, f, uctx.mm, confirm)
}

func parseFilterActions(args []string) (actions []string, labelName string) {
	for i := 0; i < len(args); i++ {
		switch strings.ToLower(args[i]) {
		case "archive":
			actions = append(actions, "archive")
		case "read", "mark_read":
			actions = append(actions, "mark_read")
		case "mute", "skip":
			actions = append(actions, "mute")
		case "move":
			actions = append(actions, "move")
			if i+1 < len(args) {
				labelName = strings.Join(args[i+1:], " ")
				return
			}
		}
	}
	return
}

func removeFilter(sender string, prefs *config.Preferences, prefsPath string) (string, error) {
	var kept []config.Filter
	for _, f := range prefs.Filters {
		if !strings.EqualFold(f.Sender, sender) {
			kept = append(kept, f)
		}
	}
	if len(kept) == len(prefs.Filters) {
		return fmt.Sprintf("No filter found for **%s**.", sender), nil
	}
	prefs.Filters = kept
	if err := config.SavePreferences(prefsPath, prefs); err != nil {
		return "", fmt.Errorf("saving filters: %w", err)
	}
	return fmt.Sprintf("Filter removed for **%s**.", sender), nil
}

func listFiltersText(filters []config.Filter) string {
	if len(filters) == 0 {
		return "No filters configured. Add one with `filter add <sender> <actions>`."
	}
	lines := make([]string, 0, len(filters))
	for _, f := range filters {
		desc := strings.Join(f.Actions, " + ")
		if f.LabelName != "" {
			desc += " → " + f.LabelName
		}
		lines = append(lines, fmt.Sprintf("- **%s** → %s", f.Sender, desc))
	}
	return "**Filters:**\n" + strings.Join(lines, "\n")
}

// applyFilters runs each email against active filters and returns only unmatched emails.
func applyFilters(emails []email.Email, filters []config.Filter, clients map[string]email.Actioner) []email.Email {
	labelCache := make(map[string][]email.Label)
	var kept []email.Email
	for _, e := range emails {
		matched := false
		for _, f := range filters {
			if !email.MatchesSender(e.FromAddr, f.Sender) {
				continue
			}
			matched = true
			client, ok := clients[e.Account]
			if !ok {
				break
			}
			for _, act := range f.Actions {
				switch act {
				case "archive":
					client.Archive(e.MsgID) //nolint:errcheck
				case "mark_read":
					client.MarkRead(e.MsgID) //nolint:errcheck
				case "mute":
					// no API call — just excluded from digest
				case "move":
					if f.LabelName == "" {
						break
					}
					if _, ok := labelCache[e.Account]; !ok {
						if lbls, err := client.ListLabels(); err == nil {
							labelCache[e.Account] = lbls
						}
					}
					for _, l := range labelCache[e.Account] {
						if strings.EqualFold(l.Name, f.LabelName) {
							client.MoveToLabel(e.MsgID, l.ID) //nolint:errcheck
							break
						}
					}
				}
			}
			break
		}
		if !matched {
			kept = append(kept, e)
		}
	}
	return kept
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
