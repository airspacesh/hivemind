package api

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Ref names a post by its workspace prefix and its number within that
// workspace, like e12.
type Ref struct {
	Prefix string
	ID     int64
}

// MessageRef names a message by its post and its number within that
// post's chat, like e40-m3.
type MessageRef struct {
	Post   Ref
	Number int64
}

// The numbers allow at most 18 digits, so every one fits an int64.
var (
	prefixPattern     = regexp.MustCompile(`^[a-z]+$`)
	refPattern        = regexp.MustCompile(`^([a-z]+)([1-9][0-9]{0,17})$`)
	messageRefPattern = regexp.MustCompile(
		`^([a-z]+)([1-9][0-9]{0,17})-m([1-9][0-9]{0,17})$`)
)

// ValidatePrefix checks that a workspace prefix is one or more
// lowercase letters.
func ValidatePrefix(prefix string) error {
	if !prefixPattern.MatchString(prefix) {
		return fmt.Errorf("invalid prefix %q: use one or more lowercase "+
			"letters", prefix)
	}
	return nil
}

func ParseRef(text string) (Ref, error) {
	match := refPattern.FindStringSubmatch(text)
	if match == nil {
		return Ref{}, fmt.Errorf("invalid post id %q: want a prefix "+
			"and a number, like e12", text)
	}
	id, err := strconv.ParseInt(match[2], 10, 64)
	if err != nil {
		return Ref{}, fmt.Errorf("invalid post id %q: %w", text, err)
	}
	return Ref{Prefix: match[1], ID: id}, nil
}

func (r Ref) String() string {
	return r.Prefix + strconv.FormatInt(r.ID, 10)
}

// MarshalText and UnmarshalText let a Ref travel in JSON as its text,
// through encoding.TextMarshaler and encoding.TextUnmarshaler.
func (r Ref) MarshalText() ([]byte, error) {
	if r.ID <= 0 || ValidatePrefix(r.Prefix) != nil {
		return nil, fmt.Errorf("invalid post ref %+v", r)
	}
	return []byte(r.String()), nil
}

func (r *Ref) UnmarshalText(text []byte) error {
	parsed, err := ParseRef(string(text))
	if err != nil {
		return err
	}
	*r = parsed
	return nil
}

func ParseMessageRef(text string) (MessageRef, error) {
	match := messageRefPattern.FindStringSubmatch(text)
	if match == nil {
		return MessageRef{}, fmt.Errorf("invalid message id %q: want an "+
			"post id, -m, and a number, like e40-m3", text)
	}
	id, err := strconv.ParseInt(match[2], 10, 64)
	if err != nil {
		return MessageRef{}, fmt.Errorf("invalid message id %q: %w",
			text, err)
	}
	number, err := strconv.ParseInt(match[3], 10, 64)
	if err != nil {
		return MessageRef{}, fmt.Errorf("invalid message id %q: %w",
			text, err)
	}
	return MessageRef{
		Post:   Ref{Prefix: match[1], ID: id},
		Number: number,
	}, nil
}

// ParseMessageCursor reads a chat cursor, the message read up to, as
// a message id. Unlike an id it may number 0, like e40-m0, to stand
// before the chat's first message.
func ParseMessageCursor(text string) (MessageRef, error) {
	if post, ok := strings.CutSuffix(text, "-m0"); ok {
		if ref, err := ParseRef(post); err == nil {
			return MessageRef{Post: ref}, nil
		}
	}
	return ParseMessageRef(text)
}

func (m MessageRef) String() string {
	return m.Post.String() + "-m" + strconv.FormatInt(m.Number, 10)
}
