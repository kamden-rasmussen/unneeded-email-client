package email

import (
	"strings"
	"time"
)

type Email struct {
	ID         string
	MsgID      string // account-native message ID (Gmail hex ID or IMAP UID string)
	Account    string
	User       string // Mattermost username of the owner
	From       string
	FromAddr   string
	Subject    string
	Preview    string
	Date       time.Time
	Category   string
	VIP        bool
	Unread     bool
	Number     int    // position in digest (1-based)
	Suggestion *Label // auto-suggested destination label, nil if none
}

type Client interface {
	Name() string
	FetchNew(since time.Time) ([]Email, error)
	Close() error
}

// SenderDomain extracts the hostname from an email address.
func SenderDomain(addr string) string {
	at := strings.LastIndex(addr, "@")
	if at < 0 || at >= len(addr)-1 {
		return ""
	}
	return strings.ToLower(addr[at+1:])
}

// MatchesSender reports whether fromAddr matches a filter sender pattern (domain or full address).
func MatchesSender(fromAddr, pattern string) bool {
	from := strings.ToLower(fromAddr)
	pat := strings.ToLower(strings.TrimPrefix(pattern, "@"))
	return from == pat ||
		strings.HasSuffix(from, "@"+pat) ||
		strings.HasSuffix(from, "."+pat)
}
