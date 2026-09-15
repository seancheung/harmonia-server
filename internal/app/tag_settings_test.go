package app

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestTagSeparatorSettings(t *testing.T) {
	a := testApp(t)
	original := Track{ID: "one", Album: "Compilation", AlbumArtist: "A/B", Artist: "A/B", Genre: "Rock/Pop"}
	other := original
	other.ID = "two"
	other.AlbumArtist = "A;B"
	original.AlbumID, other.AlbumID = albumKey(original), albumKey(other)
	if err := a.store.Update(func(st *State) error { st.Tracks = []Track{original, other}; return nil }); err != nil {
		t.Fatal(err)
	}
	res := request(t, a, "PUT", "/api/tag-settings", map[string]string{"separators": "//;"})
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	st := a.store.Read()
	if st.TagSeparators != "/" || st.Tracks[0].AlbumID != st.Tracks[1].AlbumID {
		t.Fatal("settings did not regroup existing albums")
	}
	if st.Tracks[0].Artist != original.Artist || st.Tracks[0].Genre != original.Genre {
		t.Fatal("raw display metadata was changed")
	}
	reopened, err := OpenStore(filepath.Join(a.config.DataDir, "harmonia.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Read().TagSeparators != "/" {
		t.Fatal("settings were not persisted")
	}
	if got := members("A/B; A / C", "/"); !reflect.DeepEqual(got, []string{"A", "B", "C"}) {
		t.Fatal(got)
	}
	res = request(t, a, "PUT", "/api/tag-settings", map[string]string{"separators": ""})
	st = a.store.Read()
	if res.Code != 200 || st.Tracks[0].AlbumID != original.AlbumID {
		t.Fatal("clearing separators must restore grouping")
	}
	res = request(t, a, "PUT", "/api/tag-settings", map[string]string{"separators": "abc"})
	if res.Code != 400 {
		t.Fatal("letter separators accepted")
	}
}
