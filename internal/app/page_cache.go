package app

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

type pageEntry struct {
	revision, etag string
	data           []byte
}
type pageCache struct {
	mu      sync.Mutex
	entries map[string]pageEntry
	order   []string
	size    int
}

// Cached pages are bounded independently of library size. The HTTP middleware
// authenticates each request before cached representations can be returned.
func (a *App) servePage(w http.ResponseWriter, r *http.Request, build func(State) any) {
	a.store.mu.RLock()
	state, revision := a.store.state, a.store.pageVersion()
	a.store.mu.RUnlock()
	key := r.URL.RequestURI()
	a.pages.mu.Lock()
	entry, ok := a.pages.entries[key]
	a.pages.mu.Unlock()
	if !ok || entry.revision != revision {
		data, err := json.Marshal(build(state))
		if err != nil {
			problem(w, err)
			return
		}
		entry = pageEntry{revision: revision, etag: fmt.Sprintf("\"%x\"", sha256.Sum256(data)), data: data}
		a.pages.mu.Lock()
		if a.pages.entries == nil {
			a.pages.entries = map[string]pageEntry{}
		}
		if old, exists := a.pages.entries[key]; exists {
			a.pages.size -= len(old.data)
		} else {
			a.pages.order = append(a.pages.order, key)
		}
		a.pages.entries[key] = entry
		a.pages.size += len(data)
		for len(a.pages.order) > 120 || a.pages.size > 20_000_000 {
			old := a.pages.order[0]
			a.pages.order = a.pages.order[1:]
			a.pages.size -= len(a.pages.entries[old].data)
			delete(a.pages.entries, old)
		}
		a.pages.mu.Unlock()
	}
	w.Header().Set("ETag", entry.etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if r.Header.Get("If-None-Match") == entry.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(entry.data)
}

// Called while holding the store read or write lock.
func (s *Store) pageVersion() string { return fmt.Sprintf("%s:%d", s.pageEpoch, s.pageRevision) }
