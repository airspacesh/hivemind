package names

import (
	"regexp"
	"testing"
)

// A session keeps its name for good, so any change to the lists or the
// hash that would rename agents fails here.
func TestSessionsKeepTheirNames(t *testing.T) {
	pinned := map[string]string{
		"lead":                                 "graceful-cello",
		"0b6e5f0c-3c1a-4d7e-9a43-6f2d8e1b5c27": "caramel-tungsten",
	}
	for session, want := range pinned {
		if got := Agent(session); got != want {
			t.Errorf("Agent(%q) = %q, want %q", session, got, want)
		}
	}
}

func TestWordsAreDistinctLowercaseLetters(t *testing.T) {
	letters := regexp.MustCompile(`^[a-z]+$`)
	seen := map[string]string{}
	for list, words := range map[string][]string{
		"adjectives": adjectives, "nouns": nouns,
	} {
		if len(words) == 0 {
			t.Errorf("%s is empty", list)
		}
		for _, word := range words {
			if !letters.MatchString(word) {
				t.Errorf("%s holds %q, which is not only a-z", list, word)
			}
			if other, ok := seen[word]; ok {
				t.Errorf("%q is in %s and %s", word, other, list)
			}
			seen[word] = list
		}
	}
}
