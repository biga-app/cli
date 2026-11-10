package broadcast

import "sync"

// plainCacheBytes bounds the plaintext kept to count lines in an encrypted
// session, where the server cannot read content to count them.
const plainCacheBytes = 32 << 20

// plainCache remembers the last text of recently synced files, oldest dropped
// first. A file too large to share the budget is not kept.
type plainCache struct {
	mu    sync.Mutex
	limit int
	size  int
	order []string
	text  map[string]string
}

func newPlainCache(limit int) *plainCache {
	return &plainCache{limit: limit, text: make(map[string]string)}
}

func (c *plainCache) get(path string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	text, ok := c.text[path]
	return text, ok
}

func (c *plainCache) put(path, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked(path)
	if len(text) > c.limit/4 {
		return
	}
	c.text[path] = text
	c.order = append(c.order, path)
	c.size += len(text)
	for c.size > c.limit && len(c.order) > 0 {
		c.dropLocked(c.order[0])
	}
}

func (c *plainCache) drop(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked(path)
}

func (c *plainCache) dropLocked(path string) {
	text, ok := c.text[path]
	if !ok {
		return
	}
	delete(c.text, path)
	c.size -= len(text)
	for i, p := range c.order {
		if p == path {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}
