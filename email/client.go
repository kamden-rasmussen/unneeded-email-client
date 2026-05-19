package email

import (
	"strings"
	"time"
)

type Email struct {
	ID         string
	Account    string
	From       string
	FromAddr   string
	Subject    string
	Preview    string
	Date       time.Time
	Category   string
	VIP        bool
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
