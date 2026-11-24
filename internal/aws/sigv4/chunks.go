package sigv4

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

const (
	StreamingPayload        = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	StreamingPayloadTrailer = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
)

// ChunkVerifier verifies one SigV4 stream chained to an already verified authorization header.
type ChunkVerifier struct {
	scope    string
	date     string
	previous string
	key      []byte
}

func NewChunkVerifier(r *http.Request, a *Authorization, secret string) (*ChunkVerifier, error) {
	if a.Algorithm != Algorithm || a.Expires != 0 {
		return nil, ErrMalformed
	}

	at, err := requestTime(r, a)
	if err != nil {
		return nil, err
	}

	return &ChunkVerifier{
		key: SigningKey(secret, a.Date, a.Region, a.Service), scope: a.scope(),
		date: at.Format(timeFormat), previous: a.Signature,
	}, nil
}

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Chunk validates the digest of one decoded chunk, including the final zero-length chunk.
func (v *ChunkVerifier) Chunk(digest []byte, signature string) bool {
	return v.check("AWS4-HMAC-SHA256-PAYLOAD", emptySHA256+"\n"+hex.EncodeToString(digest), signature)
}

// Trailer validates canonical trailing headers against the final chunk signature.
func (v *ChunkVerifier) Trailer(canonical, signature string) bool {
	digest := sha256.Sum256([]byte(canonical))
	return v.check("AWS4-HMAC-SHA256-TRAILER", hex.EncodeToString(digest[:]), signature)
}

func (v *ChunkVerifier) check(kind, digest, signature string) bool {
	input := strings.Join([]string{kind, v.date, v.scope, v.previous, digest}, "\n")
	expected := hex.EncodeToString(HMACSHA256(v.key, input))
	if !hmac.Equal([]byte(signature), []byte(expected)) {
		return false
	}

	v.previous = signature
	return true
}
