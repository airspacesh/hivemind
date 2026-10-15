package api

import (
	"fmt"
	"regexp"
	"unicode"
	"unicode/utf8"
)

// Field length limits, in characters (runes). The subject is a short
// headline; body holds any longer detail and is hidden from list views.
// The caps keep posts from turning the stream into a data store.
const (
	MaxProjectLen = 256
	MaxSubjectLen = 72
	MaxBodyLen    = 2048
	MaxMessageLen = 512
	MaxKeyLen     = 64
	MaxValueLen   = 256
	MaxMetadata   = 16
	MaxLinks      = 32
	MaxRequestID  = 128

	MaxOriginUserLen          = 256
	MaxOriginHostLen          = 256
	MaxOriginDirLen           = 4096
	MaxOriginClaudeSessionLen = 128
)

// keyPattern restricts metadata keys to lowercase slugs: start with a
// letter, then letters/digits in dash-separated groups. No leading or
// trailing dash, no double dash, no uppercase. This keeps keys tidy
// and stops Jira and jira from ever being distinct keys.
var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

func checkLen(field, value string, max int) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", field)
	}
	if n := utf8.RuneCountInString(value); n > max {
		return fmt.Errorf("%s is too long: %d chars (max %d)", field, n, max)
	}
	return nil
}

func checkOneLine(field, value string) error {
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains control character U+%04X", field, r)
		}
	}
	return nil
}

func checkBody(value string) error {
	for _, r := range value {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return fmt.Errorf("body contains control character U+%04X", r)
		}
	}
	return nil
}

// validateSlug enforces the slug rules and the length cap on a name
// such as a metadata key.
func validateSlug(field, name string) error {
	if name == "" {
		return fmt.Errorf("%s is required", field)
	}
	if err := checkLen(field, name, MaxKeyLen); err != nil {
		return err
	}
	if !keyPattern.MatchString(name) {
		return fmt.Errorf(
			"invalid %s %q: use lowercase letters, digits, and "+
				"single dashes, starting with a letter (e.g. jira, build-id)",
			field, name)
	}
	return nil
}

func validateKey(key string) error {
	if key == ProjectKey {
		return fmt.Errorf("%s is not a metadata key. Every post has a "+
			"project field of its own, so set that instead", ProjectKey)
	}
	return validateSlug("metadata key", key)
}

// ValidateWorkspace checks a workspace name, which follows the
// metadata key rules.
func ValidateWorkspace(name string) error {
	return validateSlug("workspace", name)
}

// validateMeta checks one metadata pair for storage.
func validateMeta(key, value string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if value == "" {
		return fmt.Errorf(
			"metadata %s has no value. In an edit, an empty value "+
				"removes the key", key)
	}
	if err := checkOneLine("metadata value", value); err != nil {
		return err
	}
	return checkLen("metadata value", value, MaxValueLen)
}

func validateMetadata(md map[string]string) error {
	if len(md) > MaxMetadata {
		return fmt.Errorf("too many metadata pairs: %d (max %d)",
			len(md), MaxMetadata)
	}
	for key, value := range md {
		if err := validateMeta(key, value); err != nil {
			return err
		}
	}
	return nil
}

// validateMetadataEdits checks the metadata an edit sets. An empty
// value asks for the key to be removed, so it carries no value to
// check. Counting is left to the store, which is the only side that
// knows how many pairs the post ends up with.
func validateMetadataEdits(md map[string]string) error {
	for key, value := range md {
		if value == "" {
			if err := validateKey(key); err != nil {
				return err
			}
			continue
		}

		if err := validateMeta(key, value); err != nil {
			return err
		}
	}
	return nil
}

func validateProject(project string) error {
	if err := checkOneLine("project", project); err != nil {
		return err
	}
	return checkLen("project", project, MaxProjectLen)
}

// kindSuffix matches the optional kind a subject may end with, like
// " (question)", which does not count toward MaxSubjectLen.
var kindSuffix = regexp.MustCompile(` \([A-Za-z]{1,10}\)$`)

// validateSubject keeps a subject to one line: a headline of at most
// MaxSubjectLen characters, then optionally a kind suffix.
func validateSubject(subject string) error {
	if err := checkOneLine("subject", subject); err != nil {
		return err
	}
	if !utf8.ValidString(subject) {
		return fmt.Errorf("subject is not valid UTF-8")
	}
	headline := kindSuffix.ReplaceAllString(subject, "")
	if n := utf8.RuneCountInString(headline); n > MaxSubjectLen {
		return fmt.Errorf("subject is too long: %d chars (max %d, not "+
			"counting a suffix like \" (question)\"). Keep it to a "+
			"headline, and put the detail in the body", n, MaxSubjectLen)
	}
	return nil
}

func validateMessageBody(body string) error {
	if body == "" {
		return fmt.Errorf("a message needs text")
	}
	if err := checkBody(body); err != nil {
		return err
	}
	if !utf8.ValidString(body) {
		return fmt.Errorf("message is not valid UTF-8")
	}
	if n := utf8.RuneCountInString(body); n > MaxMessageLen {
		return fmt.Errorf("message is too long: %d chars (max %d). Send "+
			"it as several messages, or write it as a post", n,
			MaxMessageLen)
	}
	return nil
}

func validateRequestID(id string) error {
	if err := checkOneLine("request id", id); err != nil {
		return err
	}
	return checkLen("request id", id, MaxRequestID)
}

func (r PostMessageRequest) Validate() error {
	if err := validateMessageBody(r.Body); err != nil {
		return err
	}
	if err := validateRequestID(r.RequestID); err != nil {
		return err
	}
	return r.Origin.Validate()
}

// ValidateUser checks the user a request names, which may be empty.
func ValidateUser(user string) error {
	if err := checkOneLine("user", user); err != nil {
		return err
	}
	return checkLen("user", user, MaxOriginUserLen)
}

func validateEditor(user string) error {
	if user == "" {
		return fmt.Errorf("an edit needs the user making it")
	}
	return ValidateUser(user)
}

func (r DeleteRequest) Validate() error {
	return validateEditor(r.User)
}

func validateLink(link Link) error {
	if err := validateSlug("link name", link.Name); err != nil {
		return err
	}
	if link.Ref.ID == 0 {
		return fmt.Errorf("link %s needs a post id", link.Name)
	}
	return nil
}

func validateLinks(links []Link) error {
	for _, link := range links {
		if err := validateLink(link); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks the provenance a post carries. Every field is
// optional, and none may contain control characters.
func (o Origin) Validate() error {
	fields := []struct {
		name  string
		value string
		max   int
	}{
		{"origin user", o.User, MaxOriginUserLen},
		{"origin host", o.Host, MaxOriginHostLen},
		{"origin dir", o.Dir, MaxOriginDirLen},
		{"origin claude session", o.ClaudeSession,
			MaxOriginClaudeSessionLen},
	}
	for _, field := range fields {
		if err := checkOneLine(field.name, field.value); err != nil {
			return err
		}
		if err := checkLen(field.name, field.value, field.max); err != nil {
			return err
		}
	}
	return nil
}

func (r AddPostRequest) Validate() error {
	if r.Subject == "" {
		return fmt.Errorf("subject is required")
	}
	if r.Project == "" {
		return fmt.Errorf("a project is required")
	}
	if err := validateProject(r.Project); err != nil {
		return err
	}
	if err := validateSubject(r.Subject); err != nil {
		return err
	}
	if err := checkLen("body", r.Body, MaxBodyLen); err != nil {
		return err
	}
	if err := checkBody(r.Body); err != nil {
		return err
	}
	if err := validateMetadata(r.Metadata); err != nil {
		return err
	}
	if len(r.Links) > MaxLinks {
		return fmt.Errorf("too many links: %d (max %d)",
			len(r.Links), MaxLinks)
	}
	if err := validateLinks(r.Links); err != nil {
		return err
	}
	if r.Private && r.Origin.User == "" {
		return fmt.Errorf("a private post needs the user who owns it")
	}
	if err := validateRequestID(r.RequestID); err != nil {
		return err
	}
	return r.Origin.Validate()
}

func (r EditPostRequest) Validate() error {
	if r.EditsNothing() {
		return fmt.Errorf("nothing to edit: provide project, subject, " +
			"body, metadata, links, or whether it is private")
	}
	if err := validateLinks(r.AddLinks); err != nil {
		return err
	}
	if err := validateLinks(r.RemoveLinks); err != nil {
		return err
	}
	for _, name := range r.RemoveLinkNames {
		if err := validateSlug("link name", name); err != nil {
			return err
		}
	}
	if r.Project != nil {
		if *r.Project == "" {
			return fmt.Errorf("the project can be changed but not removed")
		}
		if err := validateProject(*r.Project); err != nil {
			return err
		}
	}
	if r.Subject != nil {
		if *r.Subject == "" {
			return fmt.Errorf("subject cannot be set to empty")
		}
		if err := validateSubject(*r.Subject); err != nil {
			return err
		}
	}
	if r.Body != nil {
		if err := checkBody(*r.Body); err != nil {
			return err
		}
		if err := checkLen("body", *r.Body, MaxBodyLen); err != nil {
			return err
		}
	}
	if err := validateMetadataEdits(r.Metadata); err != nil {
		return err
	}
	if r.ExpectedVersion != nil && *r.ExpectedVersion < 1 {
		return fmt.Errorf("the expected version must be 1 or more")
	}
	if err := validateEditor(r.Origin.User); err != nil {
		return err
	}
	return r.Origin.Validate()
}
