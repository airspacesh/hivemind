package api

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

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
		return "@" + strconv.FormatInt(b.Revision, 10)
	}
	return b.Time.UTC().Format(time.RFC3339Nano)
}

// ParseBound reads a bound as String writes it.
func ParseBound(text string) (Bound, error) {
	if number, ok := strings.CutPrefix(text, "@"); ok {
		revision, err := strconv.ParseInt(number, 10, 64)
		if err != nil || revision < 1 {
			return Bound{}, fmt.Errorf(
				"a revision is @ and a positive number, like @812345, "+
					"not %q", text)
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
