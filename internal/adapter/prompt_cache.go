package adapter

import (
	"container/list"
	"strings"
	"sync"
	"time"
)

const (
	promptCacheMaxEntries = 256
	promptCacheMaxBytes   = 16 << 20
	promptCacheIdleTTL    = 5 * time.Minute
)

type promptEntry struct {
	key     string
	prompt  []byte
	touched time.Time
}

// PromptCache keeps a bounded, temporary normalized input prefix for stable
// sessions. Keys and prompt bytes share the byte limit; allocator/map overhead
// is additionally bounded by the entry limit. No response or credentials persist.
type PromptCache struct {
	mu                   sync.Mutex
	prompts              map[string]*list.Element
	recent               list.List
	bytes                int
	maxEntries, maxBytes int
	idleTTL              time.Duration
	now                  func() time.Time
	stop, done           chan struct{}
	closed               bool
}

func NewPromptCache() *PromptCache {
	return newPromptCache(promptCacheMaxEntries, promptCacheMaxBytes, promptCacheIdleTTL, time.Now)
}

func newPromptCache(entries, size int, ttl time.Duration, now func() time.Time) *PromptCache {
	c := &PromptCache{prompts: make(map[string]*list.Element), maxEntries: entries,
		maxBytes: size, idleTTL: ttl, now: now, stop: make(chan struct{}), done: make(chan struct{})}
	interval := min(ttl/2, 30*time.Second)
	go func() {
		defer close(c.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.mu.Lock()
				c.expireLocked(c.now())
				c.mu.Unlock()
			case <-c.stop:
				return
			}
		}
	}()
	return c
}

// Close frees prompt history and stops housekeeping. It is safe to repeat.
func (c *PromptCache) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.stop)
		clear(c.prompts)
		c.recent.Init()
		c.bytes = 0
	}
	c.mu.Unlock()
	<-c.done
}

// Stats reports retained entries and combined key/prompt bytes, without content.
func (c *PromptCache) Stats() (entries, retainedBytes int) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expireLocked(c.now())
	return len(c.prompts), c.bytes
}

func (c *PromptCache) removeLocked(element *list.Element) {
	entry := element.Value.(promptEntry)
	delete(c.prompts, entry.key)
	c.bytes -= len(entry.key) + len(entry.prompt)
	c.recent.Remove(element)
}

func (c *PromptCache) expireLocked(now time.Time) {
	for oldest := c.recent.Back(); oldest != nil; oldest = c.recent.Back() {
		if now.Sub(oldest.Value.(promptEntry).touched) < c.idleTTL {
			return
		}
		c.removeLocked(oldest)
	}
}

func (c *PromptCache) rememberLocked(key string, prompt []byte, now time.Time) {
	if previous := c.prompts[key]; previous != nil {
		c.removeLocked(previous)
	}
	size := len(key) + len(prompt)
	if c.closed || size > c.maxBytes {
		return
	}
	for len(c.prompts) >= c.maxEntries || c.bytes+size > c.maxBytes {
		c.removeLocked(c.recent.Back())
	}
	c.prompts[key] = c.recent.PushFront(promptEntry{key: key, prompt: append([]byte(nil), prompt...), touched: now})
	c.bytes += size
}

func (c *PromptCache) Remember(protocol, model, session string, request []byte) {
	if c == nil || strings.TrimSpace(session) == "" {
		return
	}
	prompt := normalizedPrompt(request)
	if len(prompt) == 0 {
		return
	}
	key := protocol + "\x00" + model + "\x00" + session
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.expireLocked(now)
	c.rememberLocked(key, prompt, now)
}

func (c *PromptCache) prefixAndRemember(protocol, model, session string, prompt []byte) int {
	if c == nil || strings.TrimSpace(session) == "" {
		return 0
	}
	key := protocol + "\x00" + model + "\x00" + session
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.expireLocked(now)
	hit := 0
	if previous := c.prompts[key]; previous != nil {
		hit = commonPrefix(previous.Value.(promptEntry).prompt, prompt)
	}
	c.rememberLocked(key, prompt, now)
	return hit
}
