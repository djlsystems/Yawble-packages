package main

import (
	"context"
	"time"
)

// provider is one way into a mailbox: IMAP and SMTP, Microsoft Graph or the Gmail API. Every
// command is written once against it. A provider's errors are already the run's failure words
// (a failure), so the commands pass them on as they are.
type provider interface {
	// name is how the run's words call the provider: "IMAP", "Microsoft Graph", "Gmail".
	name() string
	list(ctx context.Context, q query) ([]summary, string, error)
	read(ctx context.Context, id string) (*fullMessage, error)
	setRead(ctx context.Context, ids []string, read bool) error
	// move answers each moved message's new id where the provider gives the message one.
	move(ctx context.Context, ids []string, folder string) (map[string]string, error)
	label(ctx context.Context, ids []string, label string) error
	draft(ctx context.Context, o outgoing) (string, error)
	send(ctx context.Context, o outgoing) (string, error)
	// watchStart sets a new position at now and answers what the look-back finds.
	watchStart(ctx context.Context, w watchSpec) (position, []summary, error)
	// watchNext answers the messages added since pos, oldest first, at most w.max of them, and
	// the position after the last one answered. note is a sentence for the run's output, such as
	// a position the provider no longer knows being started again from now.
	watchNext(ctx context.Context, w watchSpec, pos position) (next position, msgs []summary, note string, err error)
	close()
}

// summary is one message as list, search and the watch show it. It never holds the body.
type summary struct {
	ID      string `json:"id"`
	Thread  string `json:"thread,omitempty"`
	From    string `json:"from"`
	Subject string `json:"subject"`
	Date    string `json:"date"`
	Unread  bool   `json:"-"`
	Snippet string `json:"snippet,omitempty"`
}

type attachment struct {
	Name string
	Type string
	Size int64
}

// fullMessage is one message as read shows it.
type fullMessage struct {
	ID, Thread                  string
	Folders                     []string
	From, To, Cc, Date, Subject string
	Text                        string // the text/plain body, when there is one
	HTML                        string // the text/html body, used when there is no text one
	Attachments                 []attachment
	// The headers a reply threads by, as the message carries them: its Message-ID, References
	// and Reply-To. Graph, which threads a reply itself, fills only ReplyTo.
	MessageID, References, ReplyTo string
}

// query is the portable query list and search take. Raw, when set, is the provider's own query
// language passed through as written, and the other fields but Max are unset.
type query struct {
	Folder  string
	Unread  bool
	From    string
	Subject string
	Since   time.Time
	Raw     string
	Max     int
	// AnyFolder is a search that named no folder: Gmail and Graph look in every folder, IMAP,
	// which can search only one at a time, in the Inbox.
	AnyFolder bool
}

// outgoing is one email, every recipient already checked against the allowlist: Body is the
// text as written, HTML the part made from it when the format is html ("" for text only).
//
// A reply also carries what threads it: InReplyTo and References for the message's headers,
// Thread for the Gmail API's threadId, and ReplyOf (with ReplyAll) for Graph's createReply.
type outgoing struct {
	From          string
	To, Cc, Bcc   []string
	Subject, Body string
	HTML          string

	InReplyTo, References string
	Thread                string
	ReplyOf               string
	ReplyAll              bool
}

// watchSpec is what one check of the watch looks at.
type watchSpec struct {
	folder     string
	unreadOnly bool
	lookback   int // days
	max        int
}

// position is where the watch has read up to in one folder of one mailbox: one of its parts is
// set, by provider.
type position struct {
	IMAP  *imapPosition  `json:"imap,omitempty"`
	Graph *graphPosition `json:"graph,omitempty"`
	Gmail *gmailPosition `json:"gmail,omitempty"`
}

type imapPosition struct {
	UIDValidity uint32 `json:"uidValidity"`
	LastUID     uint32 `json:"lastUid"`
}

type graphPosition struct {
	// Link is the next delta request: a nextLink while a round is unfinished, else the deltaLink.
	Link string `json:"link"`
	// Since is when the watch started looking: a changed message received before it is not new.
	Since time.Time `json:"since"`
	// Recent holds a short hash of the last messages announced, so a message changed after it was
	// announced (read, flagged) is not announced again.
	Recent []string `json:"recent,omitempty"`
}

type gmailPosition struct {
	HistoryID string `json:"historyId"`
	// Taken names the messages already listed from the record after HistoryID, when that record
	// added more than one check may list.
	Taken []string `json:"taken,omitempty"`
}
