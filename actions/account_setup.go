package actions

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kamden/emailagent/config"
	"github.com/kamden/emailagent/email"
	"github.com/kamden/emailagent/notify"
)

// saveTokenFunc returns a closure that persists a refreshed OAuth token to the database.
func (h *Handler) saveTokenFunc(accountName string) func(tokenJSON string) error {
	return func(tokenJSON string) error {
		if h.SaveToken != nil {
			return h.SaveToken(accountName, tokenJSON)
		}
		return nil
	}
}

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

// HandleRemoveCommand processes a "remove <name>" text command from the DM channel.
func (h *Handler) HandleRemoveCommand(name string) {
	if _, exists := h.Clients[name]; !exists {
		h.MMClient.PostMessage(fmt.Sprintf("Account **%s** not found.", name)) //nolint:errcheck
		return
	}

	if h.RemoveAccount != nil {
		if err := h.RemoveAccount(name); err != nil {
			log.Printf("remove account %s: %v", name, err)
			h.MMClient.PostMessage("Error removing account: " + err.Error()) //nolint:errcheck
			return
		}
	}

	delete(h.Clients, name)
	h.MMClient.PostMessage(fmt.Sprintf("✓ Account **%s** removed.", name)) //nolint:errcheck
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
	acc := config.Account{Name: name, Type: "gmail"}

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
		expiresAt:   time.Now().Add(10 * time.Minute),
		refresh: func(tokenJSON string) (email.Actioner, error) {
			acc.TokenJSON = tokenJSON
			return email.NewGmailClient(acc, h.saveTokenFunc(name))
		},
		newAccount: &acc,
	})

	h.MMClient.PostMessage(fmt.Sprintf( //nolint:errcheck
		"To add Gmail account **%s**, authorize it here:\n\n[Authorize Gmail](%s)\n\n_Link expires in 10 minutes._",
		name, authURL,
	))
}

func (h *Handler) handleAddIMAPPrompt(name, accountType string) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		h.MMClient.PostMessage("Failed to generate setup link.") //nolint:errcheck
		return
	}
	token := hex.EncodeToString(b)

	h.pendingIMAPSetups.Store(token, &imapPendingSetup{
		accountName: name,
		accountType: accountType,
		mmClient:    h.MMClient,
		expiresAt:   time.Now().Add(15 * time.Minute),
	})

	formURL := h.CallbackURL + "/setup/imap/form?token=" + token
	defaults := imapProviderDefaults(accountType)
	h.MMClient.PostMessage(fmt.Sprintf( //nolint:errcheck
		"Setting up %s account **%s**. Enter your credentials here (link expires in 15 minutes):\n\n%s",
		defaults.title, name, formURL,
	))
}

// --- /setup/imap/start — button click sends a browser-form link ---

type imapStartPayload struct {
	TriggerID string `json:"trigger_id"`
	Context   struct {
		AccountName   string `json:"account_name"`
		AccountType   string `json:"account_type"`
		WebhookSecret string `json:"webhook_secret"`
	} `json:"context"`
}

type imapPendingSetup struct {
	accountName string
	accountType string
	mmClient    *notify.Mattermost
	expiresAt   time.Time
}

func (h *Handler) handleIMAPStart(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var p imapStartPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		log.Printf("imap_start: decode: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	log.Printf("imap_start: account=%q type=%q secret_ok=%v",
		p.Context.AccountName, p.Context.AccountType, h.validSecret(p.Context.WebhookSecret))
	if !h.validSecret(p.Context.WebhookSecret) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	token := hex.EncodeToString(b)

	mm := h.MMClient
	h.pendingIMAPSetups.Store(token, &imapPendingSetup{
		accountName: p.Context.AccountName,
		accountType: p.Context.AccountType,
		mmClient:    mm,
		expiresAt:   time.Now().Add(15 * time.Minute),
	})

	formURL := h.CallbackURL + "/setup/imap/form?token=" + token
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck
		"ephemeral_text": fmt.Sprintf("Click here to enter your credentials (link expires in 15 minutes):\n%s", formURL),
	})
}

// --- /setup/imap/form — browser form for IMAP credentials ---

func (h *Handler) handleIMAPForm(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	val, ok := h.pendingIMAPSetups.Load(token)
	if !ok {
		http.Error(w, "Invalid or expired setup link. Please try again.", http.StatusBadRequest)
		return
	}
	pending := val.(*imapPendingSetup)
	if time.Now().After(pending.expiresAt) {
		h.pendingIMAPSetups.Delete(token)
		http.Error(w, "This setup link has expired. Please try again.", http.StatusBadRequest)
		return
	}

	var errorMsg string
	var submittedName, submittedEmail, submittedHost, submittedPort string

	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		submittedName = r.FormValue("account_name")
		submittedEmail = r.FormValue("email")
		password := r.FormValue("password")
		submittedHost = r.FormValue("host")
		submittedPort = r.FormValue("port")

		if submittedName == "" || submittedEmail == "" || password == "" || submittedHost == "" {
			errorMsg = "All fields are required."
		} else {
			port := 993
			if p, err := strconv.Atoi(submittedPort); err == nil && p > 0 {
				port = p
			}
			acc := config.Account{
				Name:     submittedName,
				Type:     "imap",
				Email:    submittedEmail,
				Password: password,
				Host:     submittedHost,
				Port:     port,
			}
			testClient, err := email.NewIMAPClient(acc)
			if err != nil {
				errorMsg = "Could not connect — check your credentials and host: " + err.Error()
			} else {
				testClient.Close() //nolint:errcheck
				liveClient, err := email.NewIMAPClient(acc)
				if err != nil {
					errorMsg = "Connection verified but could not create client: " + err.Error()
				} else {
					h.pendingIMAPSetups.Delete(token)
					h.Clients[acc.Name] = liveClient
					if h.AddAccount != nil {
						if err := h.AddAccount(acc, liveClient); err != nil {
							log.Printf("persist imap account %s: %v", acc.Name, err)
						} else if h.OnAccountAdded != nil {
							go h.OnAccountAdded(acc.Name)
						}
					}
					log.Printf("added %s account %q (%s)", pending.accountType, acc.Name, acc.Email)
					pending.mmClient.PostMessage(fmt.Sprintf("✓ Account **%s** (%s) added successfully.", acc.Name, acc.Email)) //nolint:errcheck
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					fmt.Fprintf(w, "<html><body><h2>Account <b>%s</b> added successfully — you can close this tab.</h2></body></html>",
						htmlEscape(acc.Name))
					return
				}
			}
		}
	}

	// Render the form (initial load or after validation error).
	defaults := imapProviderDefaults(pending.accountType)
	accountName := submittedName
	if accountName == "" {
		accountName = pending.accountName
	}
	emailVal := submittedEmail
	hostVal := submittedHost
	if hostVal == "" {
		hostVal = defaults.host
	}
	portVal := submittedPort
	if portVal == "" {
		portVal = defaults.port
	}

	providerSelect := ""
	if pending.accountType == "imap" {
		providerSelect = `
    <label>Provider
      <select name="provider" style="display:block;width:100%;margin-top:6px;padding:8px 10px;border:1px solid #ccc;border-radius:4px;font-size:15px;box-sizing:border-box" onchange="setProvider(this.value)">
        <option value="">Custom IMAP</option>
        <option value="icloud">Apple / iCloud</option>
        <option value="outlook">Outlook / Hotmail</option>
      </select>
    </label>
    <script>
    function setProvider(p) {
      var presets = {
        icloud: {emailPlaceholder:"you@icloud.com", host:"imap.mail.me.com", port:"993",
                 passLabel:"App-Specific Password", passPlaceholder:"xxxx-xxxx-xxxx-xxxx",
                 hint:"Generate at appleid.apple.com → Sign-In and Security → App-Specific Passwords"},
        outlook: {emailPlaceholder:"you@outlook.com", host:"outlook.office365.com", port:"993",
                  passLabel:"Password / App Password", passPlaceholder:"", hint:""}
      };
      var d = presets[p] || {emailPlaceholder:"", host:"", port:"993", passLabel:"Password", passPlaceholder:"", hint:""};
      document.querySelector('[name=email]').placeholder = d.emailPlaceholder;
      document.querySelector('[name=host]').value = d.host;
      document.querySelector('[name=port]').value = d.port;
      document.getElementById('pass-label').textContent = d.passLabel;
      document.querySelector('[name=password]').placeholder = d.passPlaceholder;
      var hintEl = document.getElementById('pass-hint');
      hintEl.textContent = d.hint;
      hintEl.style.display = d.hint ? '' : 'none';
    }
    </script>`
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
  <meta charset="UTF-8">
  <title>%s</title>
  <style>
    body{font-family:-apple-system,system-ui,sans-serif;max-width:480px;margin:60px auto;padding:0 20px;color:#333}
    h2{margin-bottom:24px}
    label{display:block;margin-top:16px;font-weight:600;font-size:14px}
    input{display:block;width:100%%;margin-top:6px;padding:8px 10px;border:1px solid #ccc;border-radius:4px;font-size:15px;box-sizing:border-box}
    .hint{font-size:12px;color:#666;margin-top:4px}
    button{margin-top:24px;background:#166de0;color:#fff;border:none;padding:10px 28px;font-size:15px;border-radius:4px;cursor:pointer}
    button:hover{background:#1451a8}
    .err{background:#fde8e8;border:1px solid #f87171;border-radius:4px;padding:12px;margin-bottom:20px;color:#b91c1c}
  </style>
</head>
<body>
  <h2>%s</h2>
  %s
  <form method="POST">
    %s
    <label>Account Name<input name="account_name" value="%s" placeholder="e.g. work, icloud, outlook" required></label>
    <label>%s<input name="email" type="email" value="%s" placeholder="%s" required></label>
    <label><span id="pass-label">%s</span><input name="password" type="password" placeholder="%s" required>
      <div class="hint" id="pass-hint"%s>%s</div></label>
    <label>IMAP Host<input name="host" value="%s" required></label>
    <label>IMAP Port<input name="port" value="%s" required></label>
    <button type="submit">Add Account</button>
  </form>
</body>
</html>`,
		htmlEscape(defaults.title),
		htmlEscape(defaults.title),
		func() string {
			if errorMsg != "" {
				return `<div class="err">` + htmlEscape(errorMsg) + `</div>`
			}
			return ""
		}(),
		providerSelect,
		htmlEscape(accountName),
		defaults.emailLabel, htmlEscape(emailVal), htmlEscape(defaults.emailPlaceholder),
		defaults.passwordLabel, htmlEscape(defaults.passwordPlaceholder),
		func() string {
			if defaults.passwordHint == "" {
				return ` style="display:none"`
			}
			return ""
		}(),
		htmlEscape(defaults.passwordHint),
		htmlEscape(hostVal),
		htmlEscape(portVal),
	)
}

type imapDefaults struct {
	title               string
	emailLabel          string
	emailPlaceholder    string
	passwordLabel       string
	passwordPlaceholder string
	passwordHint        string
	host                string
	port                string
}

func imapProviderDefaults(accountType string) imapDefaults {
	switch accountType {
	case "icloud":
		return imapDefaults{
			title:               "Add iCloud Account",
			emailLabel:          "iCloud Email",
			emailPlaceholder:    "you@icloud.com",
			passwordLabel:       "App-Specific Password",
			passwordPlaceholder: "xxxx-xxxx-xxxx-xxxx",
			passwordHint:        "Generate at appleid.apple.com → Sign-In and Security → App-Specific Passwords",
			host:                "imap.mail.me.com",
			port:                "993",
		}
	case "outlook":
		return imapDefaults{
			title:               "Add Outlook Account",
			emailLabel:          "Email Address",
			emailPlaceholder:    "you@outlook.com",
			passwordLabel:       "Password / App Password",
			passwordPlaceholder: "",
			host:                "outlook.office365.com",
			port:                "993",
		}
	default:
		return imapDefaults{
			title:               "Add IMAP Account",
			emailLabel:          "Email Address",
			emailPlaceholder:    "",
			passwordLabel:       "Password",
			passwordPlaceholder: "",
			host:                "",
			port:                "993",
		}
	}
}

func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&#34;")
	return s
}
