package email

// Label represents a Gmail label / folder.
type Label struct {
	ID   string
	Name string
}

type Actioner interface {
	Archive(msgID string) error
	MarkRead(msgID string) error
	MoveToLabel(msgID, labelID string) error
	ListLabels() ([]Label, error)
}

// Suggester can infer a destination label for a sender domain by querying
// existing organized mail. GmailClient implements this; IMAP does not.
type Suggester interface {
	InferLabel(senderDomain string) (*Label, error)
}
