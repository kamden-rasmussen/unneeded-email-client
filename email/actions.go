package email

// Label represents a Gmail label / folder.
type Label struct {
	ID   string
	Name string
}

type Actioner interface {
	Archive(msgID string) error
	Delete(msgID string) error
	MarkRead(msgID string) error
	MarkUnread(msgID string) error
	MoveToLabel(msgID, labelID string) error
	ListLabels() ([]Label, error)
}

// FilterCreator can create server-side email filters.
// GmailClient implements this; IMAP does not.
type FilterCreator interface {
	CreateSenderFilter(from string, addLabels, removeLabels []string) error
}

// Suggester can infer a destination label for a sender domain by querying
// existing organized mail. GmailClient implements this; IMAP does not.
type Suggester interface {
	InferLabel(senderDomain string) (*Label, error)
}

// Counter can report the total number of messages in the inbox.
// GmailClient implements this; IMAP does not.
type Counter interface {
	InboxCount() (int, error)
}

// Paginator supports fetching a page of emails by inbox offset.
// GmailClient implements this; IMAP does not.
type Paginator interface {
	FetchFrom(offset, limit int) ([]Email, error)
}

// BodyFetcher can retrieve the full body text of a message.
// Both GmailClient and IMAPClient implement this.
type BodyFetcher interface {
	FetchBody(msgID string) (string, error)
}
