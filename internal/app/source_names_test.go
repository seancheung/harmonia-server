package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestSourceNamesUnique(t *testing.T) {
	a := testApp(t)
	create := func(name, path string) Source {
		t.Helper()
		res := request(t, a, http.MethodPost, "/api/sources", Source{Name: name, Path: path})
		if res.Code != http.StatusOK {
			t.Fatalf("create source: %d %s", res.Code, res.Body.String())
		}
		var source Source
		if err := json.Unmarshal(res.Body.Bytes(), &source); err != nil {
			t.Fatal(err)
		}
		return source
	}
	first := create("Music", t.TempDir())
	second := create("Other", t.TempDir())
	for _, name := range []string{"Music", "music", "  MUSIC  "} {
		for _, method := range []string{http.MethodPost, http.MethodPut} {
			path, root := "/api/sources", t.TempDir()
			if method == http.MethodPut {
				path += "/" + second.ID
				root = second.Path
			}
			res := request(t, a, method, path, Source{Name: name, Path: root})
			if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "sourceNameExists") {
				t.Fatalf("%s duplicate %q: %d %s", method, name, res.Code, res.Body.String())
			}
		}
	}
	first.Name = " music "
	if res := request(t, a, http.MethodPut, "/api/sources/"+first.ID, first); res.Code != http.StatusOK {
		t.Fatalf("same source rename: %d %s", res.Code, res.Body.String())
	}
	second.Name = "Archive"
	if res := request(t, a, http.MethodPut, "/api/sources/"+second.ID, second); res.Code != http.StatusOK {
		t.Fatalf("unique rename: %d %s", res.Code, res.Body.String())
	}
}
