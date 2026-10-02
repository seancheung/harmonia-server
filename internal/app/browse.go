package app

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

type BrowseQuery struct {
	Start     string `json:"start"`
	Section   string `json:"section"`
	Detail    string `json:"detail"`
	Preset    string `json:"preset"`
	Search    string `json:"search"`
	Rule      *Rule  `json:"rule,omitempty"`
	Sort      string `json:"sort"`
	Desc      bool   `json:"desc"`
	Page      int    `json:"page"`
	PageSize  int    `json:"pageSize"`
	Albums    bool   `json:"albums"`
	Disc      int    `json:"disc"`
	Recursive bool   `json:"recursive"`
}

type BrowseGroup struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Artist     string  `json:"artist"`
	Year       int     `json:"year"`
	Count      int     `json:"count"`
	AlbumCount int     `json:"albumCount"`
	PlayCount  int     `json:"playCount"`
	AddedAt    int64   `json:"addedAt"`
	ModifiedAt int64   `json:"modifiedAt"`
	CreatedAt  int64   `json:"createdAt"`
	Smart      bool    `json:"smart"`
	Tracks     []Track `json:"tracks"`
}

type BrowsePage struct {
	Items        []Track       `json:"items"`
	Groups       []BrowseGroup `json:"groups"`
	Folders      []string      `json:"folders"`
	Total        int           `json:"total"`
	TrackCount   int           `json:"trackCount"`
	LibraryCount int           `json:"libraryCount"`
	Page         int           `json:"page"`
	PageSize     int           `json:"pageSize"`
	Heading      string        `json:"heading"`
	Artist       string        `json:"artist"`
	Year         int           `json:"year"`
	Cover        *Track        `json:"cover,omitempty"`
	Discs        []int         `json:"discs"`
	Playlist     *Playlist     `json:"playlist,omitempty"`
	FolderExists bool          `json:"folderExists"`
}

func cleanTrack(t Track) Track { t.Lyrics = ""; t.Cover = ""; return t }
func tagHas(text, name, separators string) bool {
	for _, member := range members(text, separators) {
		if strings.EqualFold(member, name) {
			return true
		}
	}
	return false
}
func browseTracks(st State, q BrowseQuery) ([]Track, *Playlist) {
	var playlist *Playlist
	if q.Section == "playlists" && q.Detail != "" {
		for _, p := range st.Playlists {
			if p.ID == q.Detail {
				copy := p
				playlist = &copy
				break
			}
		}
	}
	byID := map[string]Track{}
	if playlist != nil && !playlist.Smart {
		for _, t := range st.Tracks {
			byID[t.ID] = t
		}
	}
	candidates := st.Tracks
	if playlist != nil && !playlist.Smart {
		candidates = []Track{}
		for _, id := range playlist.Tracks {
			if t, ok := byID[id]; ok {
				candidates = append(candidates, t)
			}
		}
	}
	result := []Track{}
	for _, t := range candidates {
		if t.Missing && !(playlist != nil && !playlist.Smart) && q.Section != "recent" {
			continue
		}
		if q.Section == "favorites" && !t.Favorite {
			continue
		}
		if q.Section == "recent" && t.LastPlayed == 0 {
			continue
		}
		if (q.Preset == "topSongs" || q.Preset == "topArtists") && t.PlayCount == 0 {
			continue
		}
		if q.Preset == "unheard" && t.PlayCount != 0 {
			continue
		}
		if q.Detail != "" {
			switch q.Section {
			case "albums":
				if t.AlbumID != q.Detail {
					continue
				}
			case "artists":
				match := tagHas(t.Artist, q.Detail, st.TagSeparators) || tagHas(t.AlbumArtist, q.Detail, st.TagSeparators)
				if q.Detail == "__unknown__" {
					match = strings.TrimSpace(t.Artist+t.AlbumArtist) == ""
				}
				if !match {
					continue
				}
			case "genres":
				if q.Detail == "__unknown__" {
					if strings.TrimSpace(t.Genre) != "" {
						continue
					}
				} else if !tagHas(t.Genre, q.Detail, st.TagSeparators) {
					continue
				}
			case "folders":
				source, folder, _ := strings.Cut(q.Detail, "|")
				if t.SourceID != source || (t.Folder != folder && !(q.Recursive && (folder == "" || strings.HasPrefix(t.Folder, folder+"/")))) {
					continue
				}
			case "playlists":
				if playlist == nil {
					continue
				}
				if playlist.Smart && playlist.Rule != nil && !playlist.Rule.Match(t) {
					continue
				}
			}
		}
		if q.Search != "" && !strings.Contains(strings.ToLower(t.Title+" "+t.Filename+" "+t.Artist+" "+t.AlbumArtist+" "+t.Album+" "+t.Genre), strings.ToLower(q.Search)) {
			continue
		}
		if q.Rule != nil && !q.Rule.Match(t) {
			continue
		}
		result = append(result, t)
	}
	field, desc := q.Sort, q.Desc
	if field == "" {
		field = "title"
	}
	if q.Section == "recent" {
		field = "lastPlayed"
		desc = true
	}
	if q.Preset == "recentlyAdded" {
		field = "addedAt"
		desc = true
	}
	if q.Preset == "topSongs" {
		field = "playCount"
		desc = true
	}
	if q.Section == "albums" && q.Detail != "" && field == "title" {
		field = "disc"
	}
	if playlist != nil && playlist.Smart {
		field = playlist.Sort
		desc = playlist.Desc
	}
	if playlist == nil || playlist.Smart {
		sortTracks(result, field, desc)
	}
	if q.Preset == "topSongs" {
		sort.SliceStable(result, func(i, j int) bool {
			if result[i].PlayCount != result[j].PlayCount {
				return result[i].PlayCount > result[j].PlayCount
			}
			if result[i].LastPlayed != result[j].LastPlayed {
				return result[i].LastPlayed > result[j].LastPlayed
			}
			return result[i].ID < result[j].ID
		})
	}
	return result, playlist
}
func makeGroup(id, name string, tracks []Track) BrowseGroup {
	g := BrowseGroup{ID: id, Name: name, Tracks: []Track{}, Count: len(tracks)}
	albums := map[string]bool{}
	for _, t := range tracks {
		albums[t.AlbumID] = true
		g.PlayCount += t.PlayCount
		if g.AddedAt == 0 || t.AddedAt < g.AddedAt {
			g.AddedAt = t.AddedAt
		}
		if len(g.Tracks) < 4 {
			g.Tracks = append(g.Tracks, cleanTrack(t))
		}
	}
	g.AlbumCount = len(albums)
	if len(tracks) > 0 {
		t := tracks[0]
		g.Artist = t.AlbumArtist
		if g.Artist == "" {
			g.Artist = t.Artist
		}
		g.Year = t.Year
	}
	return g
}
func browseGroups(st State, tracks []Track, kind string, q BrowseQuery) []BrowseGroup {
	groups := []BrowseGroup{}
	if kind == "playlists" {
		for _, p := range st.Playlists {
			if q.Search != "" && !strings.Contains(strings.ToLower(p.Name), strings.ToLower(q.Search)) {
				continue
			}
			items, _ := browseTracks(st, BrowseQuery{Section: "playlists", Detail: p.ID})
			g := makeGroup(p.ID, p.Name, items)
			g.Smart = p.Smart
			groups = append(groups, g)
		}
	} else if kind == "folders" {
		for _, source := range st.Sources {
			if q.Search != "" && !strings.Contains(strings.ToLower(source.Name), strings.ToLower(q.Search)) {
				continue
			}
			items, _ := browseTracks(st, BrowseQuery{Section: "folders", Detail: source.ID + "|", Recursive: true})
			g := makeGroup(source.ID+"|", source.Name, items)
			g.ModifiedAt = source.FolderTimes[""].ModifiedAt
			g.CreatedAt = source.FolderTimes[""].CreatedAt
			groups = append(groups, g)
		}
	} else {
		grouped := map[string][]Track{}
		names := map[string]string{}
		for _, t := range tracks {
			keys := []string{}
			switch kind {
			case "albums":
				if t.AlbumID != "" {
					keys = []string{t.AlbumID}
					names[t.AlbumID] = t.Album
				}
			case "artists":
				keys = members(t.Artist, st.TagSeparators)
				if q.Preset != "topArtists" {
					keys = append(keys, members(t.AlbumArtist, st.TagSeparators)...)
				}
			case "genres":
				keys = members(t.Genre, st.TagSeparators)
			}
			if len(keys) == 0 && kind != "albums" {
				keys = []string{"__unknown__"}
			}
			seen := map[string]bool{}
			for _, name := range keys {
				key := name
				if kind != "albums" {
					key = strings.ToLower(name)
					if _, ok := names[key]; !ok {
						names[key] = name
					}
				}
				if !seen[key] {
					grouped[key] = append(grouped[key], t)
					seen[key] = true
				}
			}
		}
		for id, items := range grouped {
			group := makeGroup(id, names[id], items)
			searchable := group.Name
			if kind == "albums" {
				searchable += " " + group.Artist
			}
			if q.Search != "" && !strings.Contains(strings.ToLower(searchable), strings.ToLower(q.Search)) {
				continue
			}
			groups = append(groups, group)
		}
	}
	field := q.Sort
	if q.Preset == "topArtists" {
		field = "playCount"
		q.Desc = true
	}
	sort.SliceStable(groups, func(i, j int) bool {
		a, b := groups[i], groups[j]
		var av, bv int64
		switch field {
		case "year":
			av, bv = int64(a.Year), int64(b.Year)
		case "count":
			av, bv = int64(a.Count), int64(b.Count)
		case "albumCount":
			av, bv = int64(a.AlbumCount), int64(b.AlbumCount)
		case "playCount":
			av, bv = int64(a.PlayCount), int64(b.PlayCount)
		case "addedAt":
			av, bv = a.AddedAt, b.AddedAt
		case "modifiedAt":
			av, bv = a.ModifiedAt, b.ModifiedAt
		case "createdAt":
			av, bv = a.CreatedAt, b.CreatedAt
		}
		if av != bv {
			if av == 0 || bv == 0 {
				return av != 0
			}
			if q.Desc {
				return av > bv
			}
			return av < bv
		}
		an, bn := strings.ToLower(a.Name), strings.ToLower(b.Name)
		if field == "artist" {
			an, bn = strings.ToLower(a.Artist), strings.ToLower(b.Artist)
		}
		if an != bn {
			if q.Desc && (field == "title" || field == "filename" || field == "artist") {
				return an > bn
			}
			return an < bn
		}
		return a.ID < b.ID
	})
	return groups
}
func buildBrowse(st State, q BrowseQuery) BrowsePage {
	if q.PageSize == 0 {
		q.PageSize = 50
	}
	q.PageSize = max(1, min(100, q.PageSize))
	q.Page = max(1, q.Page)
	out := BrowsePage{Items: []Track{}, Groups: []BrowseGroup{}, Folders: []string{}, Discs: []int{}, Page: q.Page, PageSize: q.PageSize, FolderExists: true}
	for _, t := range st.Tracks {
		if !t.Missing {
			out.LibraryCount++
		}
	}
	groupMode := q.Detail == "" && (q.Section == "albums" || q.Section == "artists" || q.Section == "genres" || q.Section == "playlists" || q.Section == "folders") || q.Albums && (q.Section == "artists" || q.Section == "genres")
	trackQuery := q
	if groupMode {
		trackQuery.Search = ""
	}
	tracks, playlist := browseTracks(st, trackQuery)
	if playlist != nil {
		p := *playlist
		p.Tracks = []string{}
		out.Playlist = &p
		out.Heading = p.Name
	}
	if q.Detail != "" && q.Section != "playlists" {
		out.Heading = q.Detail
	}
	if (q.Section == "artists" || q.Section == "genres") && q.Detail != "" && len(tracks) > 0 {
		text := tracks[0].Artist + ";" + tracks[0].AlbumArtist
		if q.Section == "genres" {
			text = tracks[0].Genre
		}
		for _, name := range members(text, st.TagSeparators) {
			if strings.EqualFold(name, q.Detail) {
				out.Heading = name
				break
			}
		}
	}
	if q.Section == "albums" && q.Detail != "" && len(tracks) > 0 {
		first := cleanTrack(tracks[0])
		out.Cover = &first
		out.Heading = first.Album
		out.Artist = first.AlbumArtist
		if out.Artist == "" {
			out.Artist = first.Artist
		}
		out.Year = first.Year
		discs := map[int]bool{}
		for _, t := range tracks {
			discs[max(1, t.Disc)] = true
		}
		for d := range discs {
			out.Discs = append(out.Discs, d)
		}
		sort.Ints(out.Discs)
		if q.Disc > 0 {
			filtered := []Track{}
			for _, t := range tracks {
				if max(1, t.Disc) == q.Disc {
					filtered = append(filtered, t)
				}
			}
			tracks = filtered
		}
	}
	out.TrackCount = len(tracks)

	start := (q.Page - 1) * q.PageSize
	if groupMode {
		kind := q.Section
		if q.Albums && q.Detail != "" {
			kind = "albums"
		}
		groups := browseGroups(st, tracks, kind, q)
		out.Total = len(groups)
		start = min(start, len(groups))
		out.Groups = groups[start:min(len(groups), start+q.PageSize)]
	} else {
		folders := []string{}
		if q.Section == "folders" && q.Detail != "" {
			source, folder, _ := strings.Cut(q.Detail, "|")
			found := map[string]bool{}
			out.FolderExists = folder == ""
			for _, s := range st.Sources {
				if s.ID == source {
					out.Heading = s.Name
				}
			}
			if folder != "" {
				parts := strings.Split(folder, "/")
				out.Heading = parts[len(parts)-1]
			}
			for _, t := range st.Tracks {
				if t.Missing || t.SourceID != source {
					continue
				}
				if t.Folder == folder || folder == "" || strings.HasPrefix(t.Folder, folder+"/") {
					out.FolderExists = true
					suffix := t.Folder
					if folder != "" {
						suffix = strings.TrimPrefix(suffix, folder+"/")
					}
					if t.Folder != folder && suffix != "" {
						found[strings.Split(suffix, "/")[0]] = true
					}
				}
			}
			for name := range found {
				if q.Search == "" || strings.Contains(strings.ToLower(name), strings.ToLower(q.Search)) {
					folders = append(folders, name)
				}
			}
			times := map[string]FileTimes{}
			for _, sourceEntry := range st.Sources {
				if sourceEntry.ID == source {
					times = sourceEntry.FolderTimes
				}
			}
			sort.Slice(folders, func(i, j int) bool {
				prefix := ""
				if folder != "" {
					prefix = folder + "/"
				}
				a, b := times[prefix+folders[i]], times[prefix+folders[j]]
				var av, bv int64
				if q.Sort == "modifiedAt" {
					av, bv = a.ModifiedAt, b.ModifiedAt
				}
				if q.Sort == "createdAt" {
					av, bv = a.CreatedAt, b.CreatedAt
				}
				if av != bv {
					if av == 0 || bv == 0 {
						return av != 0
					}
					if q.Desc {
						return av > bv
					}
					return av < bv
				}
				an, bn := strings.ToLower(folders[i]), strings.ToLower(folders[j])
				if an == bn {
					return folders[i] < folders[j]
				}
				if q.Desc && q.Sort != "modifiedAt" && q.Sort != "createdAt" {
					return an > bn
				}
				return an < bn
			})
		}
		out.Total = len(tracks) + len(folders)
		for i := start; i < min(start+q.PageSize, out.Total); i++ {
			if i < len(folders) {
				out.Folders = append(out.Folders, folders[i])
			} else {
				out.Items = append(out.Items, cleanTrack(tracks[i-len(folders)]))
			}
		}
	}
	return out
}
func pageJSON(w http.ResponseWriter, r *http.Request, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		problem(w, err)
		return
	}
	tag := fmt.Sprintf("\"%x\"", sha256.Sum256(data))
	w.Header().Set("ETag", tag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if r.Header.Get("If-None-Match") == tag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}
func decodeBrowse(r *http.Request) (BrowseQuery, error) {
	var q BrowseQuery
	if err := json.Unmarshal([]byte(r.URL.Query().Get("query")), &q); err != nil {
		return q, err
	}
	switch q.Section {
	case "songs", "favorites", "recent", "albums", "artists", "genres", "playlists", "folders":
	default:
		return q, fmt.Errorf("unknown browse section")
	}
	if q.Rule != nil {
		if err := q.Rule.Validate(); err != nil {
			return q, err
		}
	}
	if q.Page < 0 || q.Page > 1000000 || q.PageSize < 0 || q.PageSize > 100 {
		return q, fmt.Errorf("invalid pagination")
	}
	return q, nil
}
func (a *App) browse(w http.ResponseWriter, r *http.Request) {
	q, err := decodeBrowse(r)
	if err != nil {
		respond(w, 400, map[string]string{"error": err.Error()})
		return
	}
	a.servePage(w, r, func(st State) any { return buildBrowse(st, q) })
}
func (a *App) home(w http.ResponseWriter, r *http.Request) { a.servePage(w, r, homeData) }
func homeData(st State) any {
	recent := buildBrowse(st, BrowseQuery{Section: "songs", Preset: "recentlyAdded", PageSize: 8})
	frequent := buildBrowse(st, BrowseQuery{Section: "songs", Preset: "topSongs", PageSize: 8})
	unheard, _ := browseTracks(st, BrowseQuery{Section: "songs", Preset: "unheard"})
	// Stable pseudo-random selection keeps ETags useful until the candidate set changes.
	sort.Slice(unheard, func(i, j int) bool {
		a, b := sha256.Sum256([]byte(unheard[i].ID)), sha256.Sum256([]byte(unheard[j].ID))
		return string(a[:]) < string(b[:])
	})
	unheard = unheard[:min(8, len(unheard))]
	for i := range unheard {
		unheard[i] = cleanTrack(unheard[i])
	}
	artists := buildBrowse(st, BrowseQuery{Section: "artists", Preset: "topArtists", PageSize: 8})
	return map[string]any{"recent": recent.Items, "frequent": frequent.Items, "unheard": unheard, "artists": artists.Groups, "total": recent.LibraryCount}
}
func (a *App) browseQueue(w http.ResponseWriter, r *http.Request) {
	q, err := decodeBrowse(r)
	if err != nil {
		respond(w, 400, map[string]string{"error": err.Error()})
		return
	}
	q.Albums = false
	tracks, _ := browseTracks(a.store.snapshot(), q)
	ids := []string{}
	for _, t := range tracks {
		if !t.Missing && (q.Disc == 0 || max(1, t.Disc) == q.Disc) {
			ids = append(ids, t.ID)
		}
	}
	start := 0
	if q.Start != "" {
		start = -1
		for i, id := range ids {
			if id == q.Start {
				start = i
				break
			}
		}
	}
	if start < 0 {
		respond(w, 404, map[string]string{"error": "Track is no longer in this selection"})
		return
	}
	ids = ids[start:min(len(ids), start+100)]
	respond(w, 200, map[string]any{"ids": ids, "limit": 100})
}
