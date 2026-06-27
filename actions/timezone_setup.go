package actions

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/kamden/emailagent/notify"
)

type pendingTZSetup struct {
	mattermostUser string
	mmClient       *notify.Mattermost
	expiresAt      time.Time
}

// HandleTimezoneCommand generates a one-time form link so the user can choose their timezone.
func (h *Handler) HandleTimezoneCommand(mm *notify.Mattermost, mattermostUser string) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		mm.PostMessage("Failed to generate setup link.") //nolint:errcheck
		return
	}
	token := hex.EncodeToString(b)

	h.pendingTZSetups.Store(token, &pendingTZSetup{
		mattermostUser: mattermostUser,
		mmClient:       mm,
		expiresAt:      time.Now().Add(15 * time.Minute),
	})

	formURL := h.CallbackURL + "/setup/timezone/form?token=" + token
	mm.PostMessage(fmt.Sprintf( //nolint:errcheck
		"Select your timezone here (link expires in 15 minutes):\n\n%s", formURL,
	))
}

func (h *Handler) handleTimezoneForm(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	val, ok := h.pendingTZSetups.Load(token)
	if !ok {
		http.Error(w, "Invalid or expired setup link. Please try again.", http.StatusBadRequest)
		return
	}
	pending := val.(*pendingTZSetup)
	if time.Now().After(pending.expiresAt) {
		h.pendingTZSetups.Delete(token)
		http.Error(w, "This setup link has expired. Please try again.", http.StatusBadRequest)
		return
	}

	var errorMsg string

	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		tz := r.FormValue("timezone")
		if tz == "" {
			errorMsg = "Please select a timezone."
		} else if h.SetTimezone == nil {
			errorMsg = "Timezone configuration is not available."
		} else if err := h.SetTimezone(pending.mattermostUser, tz); err != nil {
			log.Printf("set timezone %s for %s: %v", tz, pending.mattermostUser, err)
			errorMsg = "Error saving timezone: " + err.Error()
		} else {
			h.pendingTZSetups.Delete(token)
			pending.mmClient.PostMessage(fmt.Sprintf( //nolint:errcheck
				"✓ Timezone set to **%s**. Your daily digest will arrive at 7:00 AM in that timezone.", tz,
			))
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w,
				"<html><body><h2>Timezone set to <b>%s</b> — you can close this tab.</h2></body></html>",
				htmlEscape(tz),
			)
			return
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html>
<head>
  <meta charset="UTF-8">
  <title>Select Timezone</title>
  <style>
    body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; max-width: 480px; margin: 40px auto; padding: 0 20px; color: #333; }
    h2 { margin-bottom: 6px; }
    p.sub { color: #666; margin-top: 0; }
    label { display:block; margin-bottom: 18px; font-weight: 500; }
    select { display:block; width:100%%; margin-top:6px; padding:8px 10px; border:1px solid #ccc; border-radius:4px; font-size:15px; box-sizing:border-box; }
    button { background:#166DE0; color:#fff; border:none; border-radius:4px; padding:10px 24px; font-size:15px; cursor:pointer; }
    button:hover { background:#0f5bca; }
    .error { background:#fff0f0; border:1px solid #f5c6cb; border-radius:4px; padding:10px 14px; margin-bottom:16px; color:#c0392b; }
  </style>
</head>
<body>
  <h2>Select Your Timezone</h2>
  <p class="sub">Your daily digest will be sent at <strong>7:00 AM</strong> in the selected timezone.</p>
  %s
  <form method="POST">
    <label>Timezone
      <select name="timezone">
        <option value="">— choose one —</option>
        <optgroup label="US &amp; Canada">
          <option value="America/New_York">Eastern — New York, Toronto (UTC-5/4)</option>
          <option value="America/Chicago">Central — Chicago, Dallas (UTC-6/5)</option>
          <option value="America/Denver">Mountain — Denver, Calgary (UTC-7/6)</option>
          <option value="America/Phoenix">Mountain (no DST) — Phoenix (UTC-7)</option>
          <option value="America/Los_Angeles">Pacific — Los Angeles, Vancouver (UTC-8/7)</option>
          <option value="America/Anchorage">Alaska — Anchorage (UTC-9/8)</option>
          <option value="Pacific/Honolulu">Hawaii — Honolulu (UTC-10)</option>
        </optgroup>
        <optgroup label="Latin America">
          <option value="America/Sao_Paulo">Brasília — São Paulo (UTC-3/2)</option>
          <option value="America/Argentina/Buenos_Aires">Buenos Aires (UTC-3)</option>
          <option value="America/Mexico_City">Mexico City (UTC-6/5)</option>
          <option value="America/Bogota">Bogotá, Lima (UTC-5)</option>
        </optgroup>
        <optgroup label="Europe">
          <option value="Europe/London">London, Dublin (GMT/BST)</option>
          <option value="Europe/Paris">Paris, Berlin, Rome, Madrid (CET/CEST)</option>
          <option value="Europe/Amsterdam">Amsterdam, Brussels (CET/CEST)</option>
          <option value="Europe/Stockholm">Stockholm, Oslo, Helsinki (EET/EEST)</option>
          <option value="Europe/Athens">Athens, Bucharest (EET/EEST)</option>
          <option value="Europe/Istanbul">Istanbul (TRT, UTC+3)</option>
          <option value="Europe/Moscow">Moscow (MSK, UTC+3)</option>
        </optgroup>
        <optgroup label="Africa &amp; Middle East">
          <option value="Africa/Lagos">Lagos, Kinshasa (WAT, UTC+1)</option>
          <option value="Africa/Nairobi">Nairobi, Addis Ababa (EAT, UTC+3)</option>
          <option value="Africa/Johannesburg">Johannesburg, Cairo (SAST/EET)</option>
          <option value="Asia/Dubai">Dubai, Abu Dhabi (GST, UTC+4)</option>
        </optgroup>
        <optgroup label="Asia">
          <option value="Asia/Karachi">Karachi (PKT, UTC+5)</option>
          <option value="Asia/Kolkata">Mumbai, New Delhi (IST, UTC+5:30)</option>
          <option value="Asia/Dhaka">Dhaka (BST, UTC+6)</option>
          <option value="Asia/Bangkok">Bangkok, Jakarta (ICT, UTC+7)</option>
          <option value="Asia/Singapore">Singapore, Kuala Lumpur (SGT, UTC+8)</option>
          <option value="Asia/Shanghai">Beijing, Shanghai (CST, UTC+8)</option>
          <option value="Asia/Hong_Kong">Hong Kong (HKT, UTC+8)</option>
          <option value="Asia/Seoul">Seoul (KST, UTC+9)</option>
          <option value="Asia/Tokyo">Tokyo (JST, UTC+9)</option>
        </optgroup>
        <optgroup label="Oceania">
          <option value="Australia/Perth">Perth (AWST, UTC+8)</option>
          <option value="Australia/Adelaide">Adelaide (ACST, UTC+9:30)</option>
          <option value="Australia/Sydney">Sydney, Melbourne (AEST, UTC+10/11)</option>
          <option value="Pacific/Auckland">Auckland (NZST, UTC+12/13)</option>
        </optgroup>
        <optgroup label="UTC">
          <option value="UTC">UTC (Coordinated Universal Time)</option>
        </optgroup>
      </select>
    </label>
    <button type="submit">Save timezone</button>
  </form>
</body>
</html>`,
		errorHTML(errorMsg),
	)
}

func errorHTML(msg string) string {
	if msg == "" {
		return ""
	}
	return fmt.Sprintf(`<div class="error">%s</div>`, htmlEscape(msg))
}
