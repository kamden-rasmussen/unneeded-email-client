package actions

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/kamden/emailagent/email"
)

type pendingOAuth struct {
	accountName string
	tokenFile   string
	expiresAt   time.Time
}

func (h *Handler) handleGmailCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	errParam := r.URL.Query().Get("error")

	val, ok := h.pendingOAuths.LoadAndDelete(state)
	if !ok {
		http.Error(w, "Invalid or expired authorization link. Please try again.", http.StatusBadRequest)
		return
	}
	pending := val.(*pendingOAuth)
	if time.Now().After(pending.expiresAt) {
		http.Error(w, "Authorization link expired. Please try again.", http.StatusBadRequest)
		return
	}

	if errParam != "" {
		fmt.Fprint(w, "<html><body><h2>Authorization cancelled.</h2></body></html>")
		return
	}

	redirectURL := h.CallbackURL + "/setup/gmail/callback"
	tok, err := email.GmailExchangeCode(redirectURL, code)
	if err != nil {
		log.Printf("gmail oauth exchange for %s: %v", pending.accountName, err)
		http.Error(w, "Failed to complete authorization.", http.StatusInternalServerError)
		return
	}

	if err := email.SaveToken(pending.tokenFile, tok); err != nil {
		log.Printf("save token %s: %v", pending.tokenFile, err)
		http.Error(w, "Failed to save token.", http.StatusInternalServerError)
		return
	}

	log.Printf("re-authorized Gmail account %q", pending.accountName)
	h.MMClient.PostMessage(fmt.Sprintf("✓ Gmail **%s** re-authorized successfully.", pending.accountName)) //nolint:errcheck

	fmt.Fprint(w, "<html><body><h2>Authorization successful — you can close this tab.</h2></body></html>")
}
