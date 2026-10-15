// Package names gives each Claude Code session an agent name, like
// glad-crane, derived from its session id.
package names

import (
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"strings"
)

// The word lists and the hash are frozen. Changing a list, its order,
// or how a name is drawn from the hash renames every agent that has
// ever written. NOTICE credits the lists' sources.
var (
	//go:embed adjectives.txt
	adjectiveList string
	//go:embed nouns.txt
	nounList string

	adjectives = strings.Split(strings.TrimSuffix(adjectiveList, "\n"), "\n")
	nouns      = strings.Split(strings.TrimSuffix(nounList, "\n"), "\n")
)

// Agent is the agent name of a Claude Code session. The SHA-256 of the
// session id picks it: its first 8 bytes, as a big-endian number,
// modulo the adjectives, and its next 8 bytes modulo the nouns.
func Agent(session string) string {
	sum := sha256.Sum256([]byte(session))
	adjective := binary.BigEndian.Uint64(sum[0:8]) % uint64(len(adjectives))
	noun := binary.BigEndian.Uint64(sum[8:16]) % uint64(len(nouns))
	return adjectives[adjective] + "-" + nouns[noun]
}
