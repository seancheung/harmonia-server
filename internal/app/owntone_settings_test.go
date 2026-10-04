package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestOwnToneRulePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if store.Read().OwnToneRuleSet != "" {
		t.Fatal("default must be original audio")
	}
	if err = store.UpdateMetadata(func(st *State) error { st.OwnToneRuleSet = "remote"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.Read().OwnToneRuleSet != "remote" {
		t.Fatal("setting lost after restart")
	}
}

func TestOwnToneRuleIgnoresClientAndValidatesSettings(t *testing.T) {
	var mu sync.Mutex
	var items []map[string]any
	var lastURI string
	own := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch req.URL.Path {
		case "/api/queue":
			respond(w, 200, map[string]any{"items": items})
		case "/api/player":
			respond(w, 200, map[string]any{"state": "pause", "item_id": 1})
		case "/api/queue/clear":
			items = nil
			w.WriteHeader(204)
		case "/api/queue/items/add":
			lastURI = req.URL.Query().Get("uris")
			items = append(items, map[string]any{"id": 1, "uri": lastURI})
			respond(w, 200, map[string]int{"count": 1})
		default:
			w.WriteHeader(204)
		}
	}))
	defer own.Close()
	a := testApp(t)
	a.config.OwnTone = own.URL
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "song.wav"), []byte("audio"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Update(func(st *State) error {
		st.Sources = []Source{{ID: "s", Path: root}}
		st.Tracks = []Track{{ID: "song", SourceID: "s", Path: "song.wav"}}
		st.RuleSets = []RuleSet{{ID: "remote", Name: "Remote"}, {ID: "client", Name: "Client"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{"", "remote", ""} {
		res := request(t, a, "PUT", "/api/owntone-settings", map[string]any{"ruleSet": rule})
		if res.Code != 200 {
			t.Fatalf("save: %d %s", res.Code, res.Body.String())
		}
		config := request(t, a, "GET", "/api/config", nil)
		var value struct {
			Rule string `json:"ownToneRuleSet"`
		}
		if err := json.Unmarshal(config.Body.Bytes(), &value); err != nil || value.Rule != rule {
			t.Fatalf("config mismatch: %s", config.Body.String())
		}
		res = request(t, a, "POST", "/api/remote", map[string]any{"action": "start", "ids": []string{"song"}, "index": 0, "ruleSet": "client", "playing": false})
		if res.Code != 200 {
			t.Fatalf("start: %d %s", res.Code, res.Body.String())
		}
		mu.Lock()
		uri := lastURI
		mu.Unlock()
		parsed, err := url.Parse(uri)
		if err != nil || parsed.Query().Get("ruleSet") != rule {
			t.Fatalf("client leaked into remote rule: %s", uri)
		}
		a.remote.mu.Lock()
		saved := a.remote.saved.RuleSet
		a.remote.mu.Unlock()
		if saved != rule {
			t.Fatalf("wrong saved rule %q", saved)
		}
		if rule != "" {
			res = request(t, a, "DELETE", "/api/rule-sets/remote", nil)
			if res.Code == 200 {
				t.Fatal("deleted active OwnTone rule")
			}
		}
	}
	for _, body := range []map[string]any{{"ruleSet": "missing"}, {}} {
		if res := request(t, a, "PUT", "/api/owntone-settings", body); res.Code == 200 {
			t.Fatal("invalid setting accepted")
		}
	}
	if a.store.Read().OwnToneRuleSet != "" {
		t.Fatal("invalid update changed settings")
	}
}
