package api

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// FormatRevision writes a revision as @ and its number, like @812345.
func FormatRevision(revision int64) string {
	return "@" + strconv.FormatInt(revision, 10)
}

// ParseRevision reads a revision as FormatRevision writes it. @0 comes
// before every change.
func ParseRevision(text string) (int64, error) {
	number, ok := strings.CutPrefix(text, "@")
	revision, err := strconv.ParseInt(number, 10, 64)
	if !ok || err != nil || revision < 0 {
		return 0, fmt.Errorf("a revision is @ and a number, like @812345, "+
			"not %q", text)
	}
	return revision, nil
}

// Bound is one end of the span a listing keeps to: a revision when
// Revision is set, or else a time. A revision bounds exactly, since no
// two changes share one, where many changes can share a time.
type Bound struct {
	Time     time.Time
	Revision int64
}

// String writes a bound as ParseBound reads it: a revision as @ and its
// number, like @812345, or a time in RFC 3339.
func (b Bound) String() string {
	if b.Revision != 0 {
		return FormatRevision(b.Revision)
	}
	return b.Time.UTC().Format(time.RFC3339Nano)
}

// ParseBound reads a bound as String writes it.
func ParseBound(text string) (Bound, error) {
	if strings.HasPrefix(text, "@") {
		revision, err := ParseRevision(text)
		if err != nil {
			return Bound{}, err
		}
		if revision == 0 {
			return Bound{}, fmt.Errorf("a bound's revision is positive, "+
				"like @812345, not %q", text)
		}
		return Bound{Revision: revision}, nil
	}
	at, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return Bound{}, fmt.Errorf("a bound is a revision like @812345 or "+
			"an RFC 3339 time like 2026-10-09T04:00:00Z, not %q", text)
	}
	return Bound{Time: at}, nil
}

// ChatBound is where a read of a chat starts: after the message
// Number, or at Time when it is set. The zero bound starts before the
// first message.
type ChatBound struct {
	Time   time.Time
	Number int64
}

// String writes a chat bound as ParseChatBound reads it, as a message
// number like 3, or a time in RFC 3339.
func (b ChatBound) String() string {
	if !b.Time.IsZero() {
		return b.Time.UTC().Format(time.RFC3339Nano)
	}
	return strconv.FormatInt(b.Number, 10)
}

// ParseChatBound reads a chat bound as String writes it.
func ParseChatBound(text string) (ChatBound, error) {
	if number, err := strconv.ParseInt(text, 10, 64); err == nil {
		if number < 0 {
			return ChatBound{}, fmt.Errorf("a message number is 0 or "+
				"more, not %q", text)
		}
		return ChatBound{Number: number}, nil
	}
	at, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return ChatBound{}, fmt.Errorf("a chat bound is a message number "+
			"like 3 or an RFC 3339 time like 2026-10-09T04:00:00Z, not %q",
			text)
	}
	return ChatBound{Time: at}, nil
}
