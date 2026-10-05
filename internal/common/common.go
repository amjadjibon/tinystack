package common

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"maps"
	"time"
)

const (
	AlphabetLowerHex     = "abcdefghijklmnopqrstuvwxyz0123456789"
	AlphabetUpperHex     = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	AlphabetAlphanumeric = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
)

// Value returns the value pointed to by p if p is non-nil, or the zero value of T.
func Value[T any](p *T) (v T) {
	if p != nil {
		return *p
	}
	return v
}

// OrElse returns p if p is non-nil, or fallback.
func OrElse[T any](p, fallback *T) *T {
	if p != nil {
		return p
	}
	return fallback
}

// Clone deep-copies a value through JSON marshaling and unmarshaling.
// If v is nil, it returns nil.
func Clone[T any](v *T) *T {
	if v == nil {
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	out := new(T)
	_ = json.Unmarshal(data, out)
	return out
}

// CloneTags returns a copy of tags. If tags is nil, it returns an empty non-nil map.
func CloneTags(tags map[string]string) map[string]string {
	out := make(map[string]string, len(tags))
	maps.Copy(out, tags)
	return out
}

// RandomID returns a random string of length n sampled from the given alphabet,
// defaulting to AlphabetLowerHex.
func RandomID(n int, alphabets ...string) string {
	alphabet := AlphabetLowerHex
	if len(alphabets) > 0 && alphabets[0] != "" {
		alphabet = alphabets[0]
	}
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// Seconds returns a pointer to Unix seconds (float64) of t.
func Seconds(t time.Time) *float64 {
	return new(float64(t.Unix()))
}

// SecondsMilli returns a pointer to Unix seconds with millisecond precision of t.
func SecondsMilli(t time.Time) *float64 {
	return new(float64(t.UnixMilli()) / 1000)
}

// Unix returns Unix seconds with millisecond precision of t.
func Unix(t time.Time) float64 {
	return float64(t.UnixMilli()) / 1000
}

// ErrorsOr returns err if err is non-nil, or fallback.
func ErrorsOr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

// Page returns at most limit items after the one whose ID the token holds, and the token for the rest, if any.
// A token is the base64 of the last returned item's ID; one whose item is gone resumes at the first greater ID, so
// items should be in ascending ID order. A malformed token fails with invalid.
func Page[T any](items []T, id func(T) string, token *string, limit int, invalid error) (
	selected []T, next *string, err error,
) {
	start := 0
	if token != nil {
		after, decodeErr := base64.RawURLEncoding.DecodeString(*token)
		if decodeErr != nil || len(after) == 0 {
			return nil, nil, invalid
		}

		start = len(items)
		for i, item := range items {
			if id(item) == string(after) {
				start = i + 1
				break
			}

			if id(item) > string(after) && start == len(items) {
				start = i
			}
		}
	}

	items = items[start:]
	if len(items) <= limit {
		return items, nil, nil
	}

	return items[:limit], new(base64.RawURLEncoding.EncodeToString([]byte(id(items[limit-1])))), nil
}
