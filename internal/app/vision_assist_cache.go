package app

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

const (
	visionAssistCacheTTL        = time.Hour
	visionAssistCacheMaxEntries = 128
)

// visionAssistCache keeps successful image descriptions only in process memory.
// It is deliberately bounded and expires automatically: cache loss safely falls
// back to a fresh vision request.
type visionAssistCache struct {
	mu      sync.Mutex
	entries map[string]visionAssistCacheEntry
	now     func() time.Time
}

type visionAssistCacheEntry struct {
	description string
	expiresAt   time.Time
}

func newVisionAssistCache() *visionAssistCache {
	return &visionAssistCache{
		entries: make(map[string]visionAssistCacheEntry),
		now:     time.Now,
	}
}

// visionAssistCacheTokenKey keeps successful image descriptions scoped to the
// calling API token. Empty token hashes are used by locally unauthenticated
// requests and deliberately share only that local scope.
func visionAssistCacheTokenKey(tokenHash, imageKey string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte("vision-assist-token-scope-v1\x00"))
	_, _ = hash.Write([]byte(tokenHash))
	_, _ = hash.Write([]byte("\x00"))
	_, _ = hash.Write([]byte(imageKey))
	return hex.EncodeToString(hash.Sum(nil))
}

func (c *visionAssistCache) Get(key string) (string, bool) {
	if c == nil || key == "" {
		return "", false
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return "", false
	}
	if !entry.expiresAt.After(now) {
		delete(c.entries, key)
		return "", false
	}
	return entry.description, true
}

func (c *visionAssistCache) Put(key, description string) {
	if c == nil || key == "" || description == "" {
		return
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeExpired(now)
	if _, exists := c.entries[key]; !exists && len(c.entries) >= visionAssistCacheMaxEntries {
		c.removeOldest()
	}
	c.entries[key] = visionAssistCacheEntry{
		description: description,
		expiresAt:   now.Add(visionAssistCacheTTL),
	}
}

func (c *visionAssistCache) removeExpired(now time.Time) {
	for key, entry := range c.entries {
		if !entry.expiresAt.After(now) {
			delete(c.entries, key)
		}
	}
}

func (c *visionAssistCache) removeOldest() {
	var oldestKey string
	var oldestExpiry time.Time
	for key, entry := range c.entries {
		if oldestKey == "" || entry.expiresAt.Before(oldestExpiry) {
			oldestKey = key
			oldestExpiry = entry.expiresAt
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}
