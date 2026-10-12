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

// Bound is a point in a server's history that a read starts after or
// stops before: the posting of a post when Post is set, or of its
// message numbered Message when that is not 0, or else a time when Time
// is set, or else the revision Revision. Revision 0 comes before every
// change. A message is posted after its post, so the posting of a post
// is the start of its chat, like e40-m0.
type Bound struct {
	Post     Ref
	Message  int64
	Time     time.Time
	Revision int64
}

// String writes a bound as ParseBound reads it: a post id like e12, a
// message id like e40-m3, a time in RFC 3339, or a revision like
// @812345.
func (b Bound) String() string {
	switch {
	case b.Post.ID != 0 && b.Message != 0:
		return MessageRef{Post: b.Post, Number: b.Message}.String()
	case b.Post.ID != 0:
		return b.Post.String()
	case !b.Time.IsZero():
		return b.Time.UTC().Format(time.RFC3339Nano)
	default:
		return FormatRevision(b.Revision)
	}
}

// ParseBound reads a bound as String writes it, and a message id
// numbered 0, like e40-m0, as its post.
func ParseBound(text string) (Bound, error) {
	if strings.HasPrefix(text, "@") {
		revision, err := ParseRevision(text)
		return Bound{Revision: revision}, err
	}
	if message, err := ParseMessageCursor(text); err == nil {
		return Bound{Post: message.Post, Message: message.Number}, nil
	}
	if post, err := ParseRef(text); err == nil {
		return Bound{Post: post}, nil
	}
	if at, err := time.Parse(time.RFC3339Nano, text); err == nil {
		return Bound{Time: at}, nil
	}
	return Bound{}, fmt.Errorf("a bound is a post id like e12, a message "+
		"id like e40-m3, a revision like @812345, or an RFC 3339 time "+
		"like 2026-10-09T04:00:00Z, not %q", text)
}
