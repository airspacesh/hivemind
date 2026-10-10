package api

import (
	"fmt"
	"strings"
	"time"
)

// ChangeCursor marks a place in a workspace's changes by the change
// last read, which is a change to an entry, or to one of its messages
// when Message is not 0, at the time the server stamped it. The zero
// cursor comes before every change.
type ChangeCursor struct {
	At      time.Time
	Entry   Ref
	Message int64
}

func (c ChangeCursor) IsZero() bool {
	return c.Entry.ID == 0
}

// ParseChangeCursor reads a cursor written as 0, or as the changed
// entry or message and its time, like e12@2026-10-10T01:02:03.5Z or
// e12-m3@2026-10-10T01:02:03.5Z.
func ParseChangeCursor(text string) (ChangeCursor, error) {
	if text == "0" {
		return ChangeCursor{}, nil
	}
	invalid := fmt.Errorf("invalid cursor %q: want 0, or an entry or "+
		"message id and a time, like e12-m3@2026-10-10T01:02:03.5Z", text)
	changed, at, found := strings.Cut(text, "@")
	if !found {
		return ChangeCursor{}, invalid
	}
	var cursor ChangeCursor
	var err error
	if cursor.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
		return ChangeCursor{}, invalid
	}
	if strings.Contains(changed, "-") {
		message, err := ParseMessageRef(changed)
		if err != nil {
			return ChangeCursor{}, invalid
		}
		cursor.Entry, cursor.Message = message.Entry, message.Number
	} else if cursor.Entry, err = ParseRef(changed); err != nil {
		return ChangeCursor{}, invalid
	}
	return cursor, nil
}

func (c ChangeCursor) String() string {
	if c.IsZero() {
		return "0"
	}
	changed := c.Entry.String()
	if c.Message != 0 {
		changed = MessageRef{Entry: c.Entry, Number: c.Message}.String()
	}
	return changed + "@" + c.At.UTC().Format(time.RFC3339Nano)
}

// MarshalText and UnmarshalText let a ChangeCursor travel in JSON as
// its text, through encoding.TextMarshaler and
// encoding.TextUnmarshaler.
func (c ChangeCursor) MarshalText() ([]byte, error) {
	return []byte(c.String()), nil
}

func (c *ChangeCursor) UnmarshalText(text []byte) error {
	parsed, err := ParseChangeCursor(string(text))
	if err != nil {
		return err
	}
	*c = parsed
	return nil
}
