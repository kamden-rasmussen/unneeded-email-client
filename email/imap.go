package email

import (
	"fmt"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/kamden/emailagent/config"
)

type IMAPClient struct {
	cfg config.Account
	c   *client.Client
}

func NewIMAPClient(acc config.Account) (*IMAPClient, error) {
	port := acc.Port
	if port == 0 {
		port = 993
	}
	addr := fmt.Sprintf("%s:%d", acc.Host, port)

	c, err := client.DialTLS(addr, nil)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", addr, err)
	}

	if err := c.Login(acc.Email, acc.Password); err != nil {
		c.Logout()
		return nil, fmt.Errorf("login %s: %w", acc.Name, err)
	}

	return &IMAPClient{cfg: acc, c: c}, nil
}

func (ic *IMAPClient) Name() string { return ic.cfg.Name }

func (ic *IMAPClient) FetchNew(since time.Time) ([]Email, error) {
	// read-only select to avoid marking messages as seen
	if _, err := ic.c.Select("INBOX", true); err != nil {
		return nil, fmt.Errorf("select inbox: %w", err)
	}

	criteria := imap.NewSearchCriteria()
	criteria.Since = since

	uids, err := ic.c.UidSearch(criteria)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	if len(uids) == 0 {
		return nil, nil
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(uids...)

	ch := make(chan *imap.Message, 32)
	done := make(chan error, 1)
	go func() {
		done <- ic.c.UidFetch(seqset, []imap.FetchItem{imap.FetchEnvelope, imap.FetchUid}, ch)
	}()

	var emails []Email
	for msg := range ch {
		if msg.Envelope == nil {
			continue
		}
		e := Email{
			ID:      fmt.Sprintf("imap-%s-%d", ic.cfg.Name, msg.Uid),
			Account: ic.cfg.Name,
			Subject: msg.Envelope.Subject,
			Date:    msg.Envelope.Date,
		}
		if len(msg.Envelope.From) > 0 {
			from := msg.Envelope.From[0]
			e.FromAddr = fmt.Sprintf("%s@%s", from.MailboxName, from.HostName)
			if from.PersonalName != "" {
				e.From = from.PersonalName
			} else {
				e.From = e.FromAddr
			}
		}
		emails = append(emails, e)
	}

	return emails, <-done
}

func (ic *IMAPClient) Close() error {
	return ic.c.Logout()
}
