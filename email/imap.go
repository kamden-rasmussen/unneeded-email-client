package email

import (
	"fmt"
	"io"
	"strconv"
	"strings"
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
	c, err := dialIMAP(acc)
	if err != nil {
		return nil, err
	}
	return &IMAPClient{cfg: acc, c: c}, nil
}

func dialIMAP(acc config.Account) (*client.Client, error) {
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
	return c, nil
}

func (ic *IMAPClient) reconnect() error {
	if ic.c != nil {
		ic.c.Logout()
	}
	c, err := dialIMAP(ic.cfg)
	if err != nil {
		return err
	}
	ic.c = c
	return nil
}

func (ic *IMAPClient) Name() string { return ic.cfg.Name }

func (ic *IMAPClient) FetchNew(since time.Time) ([]Email, error) {
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
		done <- ic.c.UidFetch(seqset, []imap.FetchItem{imap.FetchEnvelope, imap.FetchUid, imap.FetchFlags}, ch)
	}()

	var emails []Email
	for msg := range ch {
		if msg.Envelope == nil {
			continue
		}
		uidStr := strconv.FormatUint(uint64(msg.Uid), 10)
		e := Email{
			ID:      fmt.Sprintf("imap-%s-%s", ic.cfg.Name, uidStr),
			MsgID:   uidStr,
			Account: ic.cfg.Name,
			Date:    msg.Envelope.Date,
			Subject: msg.Envelope.Subject,
			Unread:  true,
		}
		for _, flag := range msg.Flags {
			if flag == imap.SeenFlag {
				e.Unread = false
				break
			}
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

func (ic *IMAPClient) FetchBody(msgID string) (string, error) {
	if err := ic.reconnect(); err != nil {
		return "", err
	}
	if _, err := ic.c.Select("INBOX", true); err != nil {
		return "", fmt.Errorf("select inbox: %w", err)
	}
	uid, err := strconv.ParseUint(msgID, 10, 32)
	if err != nil {
		return "", fmt.Errorf("invalid uid %q: %w", msgID, err)
	}
	seqset := new(imap.SeqSet)
	seqset.AddNum(uint32(uid))

	ch := make(chan *imap.Message, 1)
	done := make(chan error, 1)
	go func() {
		done <- ic.c.UidFetch(seqset, []imap.FetchItem{imap.FetchRFC822Text}, ch)
	}()

	var body string
	for msg := range ch {
		for _, r := range msg.Body {
			b, _ := io.ReadAll(r)
			body = string(b)
		}
	}
	return body, <-done
}

func (ic *IMAPClient) Close() error {
	return ic.c.Logout()
}

func (ic *IMAPClient) Archive(msgID string) error {
	archiveMailbox := ic.cfg.ArchiveMailbox
	if archiveMailbox == "" {
		archiveMailbox = "Archive"
	}
	return ic.moveUID(msgID, archiveMailbox)
}

func (ic *IMAPClient) Delete(msgID string) error {
	trashMailbox := ic.cfg.TrashMailbox
	if trashMailbox == "" {
		trashMailbox = "Trash"
	}
	return ic.moveUID(msgID, trashMailbox)
}

func (ic *IMAPClient) MarkRead(msgID string) error {
	return ic.setSeenFlag(msgID, true)
}

func (ic *IMAPClient) MarkUnread(msgID string) error {
	return ic.setSeenFlag(msgID, false)
}

func (ic *IMAPClient) setSeenFlag(msgID string, seen bool) error {
	if err := ic.reconnect(); err != nil {
		return err
	}
	if _, err := ic.c.Select("INBOX", false); err != nil {
		return fmt.Errorf("select inbox: %w", err)
	}
	uid, err := strconv.ParseUint(msgID, 10, 32)
	if err != nil {
		return fmt.Errorf("invalid uid %q: %w", msgID, err)
	}
	seqset := new(imap.SeqSet)
	seqset.AddNum(uint32(uid))
	var op imap.FlagsOp = imap.AddFlags
	if !seen {
		op = imap.RemoveFlags
	}
	return ic.c.UidStore(seqset, imap.FormatFlagsOp(op, true), []interface{}{imap.SeenFlag}, nil)
}

func (ic *IMAPClient) MoveToLabel(msgID, mailbox string) error {
	return ic.moveUID(msgID, mailbox)
}

func (ic *IMAPClient) ListLabels() ([]Label, error) {
	if err := ic.reconnect(); err != nil {
		return nil, err
	}

	mailboxes := make(chan *imap.MailboxInfo, 20)
	done := make(chan error, 1)
	go func() {
		done <- ic.c.List("", "*", mailboxes)
	}()

	skip := map[string]bool{
		"inbox": true, "drafts": true, "sent": true,
		"sent messages": true, "deleted messages": true,
		"trash": true, "junk": true, "spam": true,
	}

	var labels []Label
	for m := range mailboxes {
		if skip[strings.ToLower(m.Name)] {
			continue
		}
		labels = append(labels, Label{ID: m.Name, Name: m.Name})
	}
	return labels, <-done
}

func (ic *IMAPClient) moveUID(msgID, destMailbox string) error {
	if err := ic.reconnect(); err != nil {
		return err
	}
	if _, err := ic.c.Select("INBOX", false); err != nil {
		return fmt.Errorf("select inbox: %w", err)
	}
	uid, err := strconv.ParseUint(msgID, 10, 32)
	if err != nil {
		return fmt.Errorf("invalid uid %q: %w", msgID, err)
	}
	seqset := new(imap.SeqSet)
	seqset.AddNum(uint32(uid))
	if err := ic.c.UidCopy(seqset, destMailbox); err != nil {
		return fmt.Errorf("copy to %s: %w", destMailbox, err)
	}
	return ic.c.UidStore(seqset, imap.FormatFlagsOp(imap.AddFlags, true), []interface{}{imap.DeletedFlag}, nil)
}
