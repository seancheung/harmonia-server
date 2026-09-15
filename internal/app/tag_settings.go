package app

import (
	"net/http"
	"strings"
	"unicode"
)

func (a *App) tagSettings(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Separators string `json:"separators"`
	}
	if !decode(w, r, &b) {
		return
	}
	if len([]rune(b.Separators)) > 16 {
		respond(w, 400, map[string]string{"error": "Too many tag separators"})
		return
	}
	separators := ""
	for _, c := range b.Separators {
		if unicode.IsSpace(c) || unicode.IsControl(c) || unicode.IsLetter(c) || unicode.IsNumber(c) {
			respond(w, 400, map[string]string{"error": "Tag separators must be punctuation or symbols"})
			return
		}
		if c != ';' && !strings.ContainsRune(separators, c) {
			separators += string(c)
		}
	}
	if err := a.store.Update(func(st *State) error {
		st.TagSeparators = separators
		for i := range st.Tracks {
			st.Tracks[i].AlbumID = albumKey(st.Tracks[i], separators)
		}
		return nil
	}); err != nil {
		problem(w, err)
		return
	}
	respond(w, 200, map[string]string{"separators": separators})
}
