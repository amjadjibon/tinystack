package proxy

import (
	"container/list"
	"crypto/sha256"
	"encoding/json"
	"time"
)

const authorizerCacheSize = 1024

// AuthorizerCache stores validated Lambda responses for one stage. Its owner
// serializes access and evaluates the cached policy for each incoming ARN.
type AuthorizerCache struct {
	entries map[[sha256.Size]byte]*list.Element
	recency list.List
}

type cachedAuthorization struct {
	expires time.Time
	payload []byte
	key     [sha256.Size]byte
}

func AuthorizerCacheKey(deployment, authorizer string, sources []string) [sha256.Size]byte {
	input, _ := json.Marshal(struct {
		Deployment, Authorizer string
		Sources                []string
	}{deployment, authorizer, sources})
	return sha256.Sum256(input)
}

func (c *AuthorizerCache) Get(key [sha256.Size]byte, now time.Time) []byte {
	element := c.entries[key]
	if element == nil {
		return nil
	}

	entry := element.Value.(cachedAuthorization)
	if now.Before(entry.expires) {
		c.recency.MoveToFront(element)
		return entry.payload
	}

	c.recency.Remove(element)
	delete(c.entries, key)
	return nil
}

func (c *AuthorizerCache) Put(key [sha256.Size]byte, payload []byte, now time.Time, ttl time.Duration) {
	// Keep the first fresh result if concurrent requests missed the same entry.
	if c.Get(key, now) != nil {
		return
	}

	if len(c.entries) == authorizerCacheSize {
		removed := c.recency.Remove(c.recency.Back()).(cachedAuthorization)
		delete(c.entries, removed.key)
	}

	if c.entries == nil {
		c.entries = make(map[[sha256.Size]byte]*list.Element)
	}

	c.entries[key] = c.recency.PushFront(cachedAuthorization{key: key, payload: payload, expires: now.Add(ttl)})
}
