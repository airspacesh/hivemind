package store

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"

	"github.com/airspacesh/hivemind/internal/api"
)

// ErrRequestReused is a request whose id already named a request that
// asked for something else.
var ErrRequestReused = errors.New("request id already used")

// requestDigest fingerprints what a request asks the store to create,
// so a repeat of it can be told from another request under the same id.
func requestDigest(content any) int64 {
	encoded, err := json.Marshal(content)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

// postDigest is the requestDigest of a post's content. It leaves out
// the origin, so a request repeated from another directory or session
// is still a repeat.
func postDigest(req api.AddPostRequest) int64 {
	req.Origin = api.Origin{}
	req.RequestID = ""
	return requestDigest(req)
}

// messageDigest is the requestDigest of a message's text.
func messageDigest(req api.PostMessageRequest) int64 {
	return requestDigest(req.Body)
}
