package app

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"

	"modernc.org/sqlite"
)

func init() {
	for name, fn := range map[string]func(any) any{
		"harmonia_lower": func(v any) any {
			if v == nil {
				return ""
			}
			return strings.ToLower(fmt.Sprint(v))
		},
		"harmonia_trim": func(v any) any {
			if v == nil {
				return ""
			}
			return strings.TrimSpace(fmt.Sprint(v))
		},
	} {
		err := sqlite.RegisterDeterministicScalarFunction(name, 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) { return fn(args[0]), nil })
		if err != nil {
			panic(err)
		}
	}
}

// Column expressions come only from this whitelist; tag names and rule values
// are bound parameters, never interpolated SQL or JSON paths.
func fieldSQL(field string) (string, bool) {
	switch field {
	case "title", "artist", "album", "albumArtist", "genre", "key":
		name := field
		if field == "albumArtist" {
			name = "album_artist"
		}
		return "x.search_" + name, false
	case "path":
		return `replace(t.path, '\', '/')`, false
	case "bpm":
		return "x.search_bpm", true
	}
	for _, c := range columns(Track{}) {
		// Validate() limits rule fields; this also supports the existing sort fields.
		if strings.ReplaceAll(c.name, "_", "") == strings.ToLower(field) {
			numeric := strings.Contains("|year|duration|playCount|addedAt|modifiedAt|createdAt|lastPlayed|disc|number|bitrate|sampleRate|", "|"+field+"|")
			return "t." + quoted(c.name), numeric
		}
	}
	return "NULL", false
}

func emptySQL(expr, field string, number bool) string {
	if strings.HasPrefix(expr, "x.search_") && !number {
		return "(" + expr + "_empty=1)"
	}
	if number && field != "playCount" {
		return "(" + expr + " IS NULL OR " + expr + "=0)"
	}
	if number || field == "favorite" {
		return "(" + expr + " IS NULL)"
	}
	return "(harmonia_trim(" + expr + ")='')"
}

func foldSQL(expr string) string {
	if strings.HasPrefix(expr, "x.search_") || expr == "v.value_key" {
		return expr
	}
	return "harmonia_lower(" + expr + ")"
}

func comparisonSQL(expr, op string, val any, number bool) (string, []any) {
	if op == "contains" || op == "notContains" {
		operator := ">0"
		if op == "notContains" {
			operator = "=0"
		}
		return "(" + expr + " IS NOT NULL AND instr(" + foldSQL(expr) + ",?)" + operator + ")", []any{strings.ToLower(fmt.Sprint(val))}
	}
	operator := map[string]string{"eq": "=", "ne": "<>", "gt": ">", "gte": ">=", "lt": "<", "lte": "<="}[op]
	if operator == "" {
		return "0", nil
	}
	lhs := foldSQL(expr)
	var rhs any = strings.ToLower(fmt.Sprint(val))
	if number {
		lhs = expr
		n, ok := numeric(val)
		if !ok {
			return "0", nil
		}
		rhs = n
	}
	predicate := lhs + operator + "?"
	if op == "ne" {
		predicate = expr + " IS NULL OR " + predicate
	} else {
		predicate = expr + " IS NOT NULL AND " + predicate
	}
	return "(" + predicate + ")", []any{rhs}
}

func compileRule(r Rule) (string, []any, error) {
	if err := r.Validate(); err != nil {
		return "", nil, err
	}
	var walk func(Rule) (string, []any)
	walk = func(r Rule) (string, []any) {
		if r.Mode != "" {
			parts := []string{}
			args := []any{}
			for _, child := range r.Rules {
				p, a := walk(child)
				parts = append(parts, p)
				args = append(args, a...)
			}
			join := " AND "
			if r.Mode == "any" {
				join = " OR "
			}
			return "(" + strings.Join(parts, join) + ")", args
		}
		if strings.HasPrefix(r.Field, "tag:") {
			op := r.Op
			negative := op == "ne" || op == "notContains" || op == "isEmpty"
			if op == "ne" {
				op = "eq"
			}
			if op == "notContains" {
				op = "contains"
			}
			p, a := comparisonSQL("v.value_key", op, r.Value, false)
			if op == "isEmpty" || op == "isNotEmpty" {
				p = "v.nonempty=1"
				a = nil
			}
			args := append([]any{strings.TrimPrefix(r.Field, "tag:")}, a...)
			p = "t.id IN (SELECT v.track_id FROM track_tag_index v WHERE v.tag_name=? AND " + p + ")"
			if negative {
				p = "NOT " + p
			}
			return "(" + p + ")", args
		}
		expr, number := fieldSQL(r.Field)
		if r.Op == "isEmpty" || r.Op == "isNotEmpty" {
			p := emptySQL(expr, r.Field, number)
			if r.Op == "isNotEmpty" {
				p = "NOT " + p
			}
			return "(" + p + ")", nil
		}
		val := r.Value
		if r.Field == "path" {
			val = strings.ReplaceAll(fmt.Sprint(val), "\\", "/")
		}
		if r.Field == "favorite" {
			if b, ok := val.(bool); ok && (r.Op == "eq" || r.Op == "ne") {
				op := "="
				if r.Op == "ne" {
					op = "<>"
				}
				return expr + op + "?", []any{b}
			}
			expr = "CASE WHEN t.favorite THEN 'true' ELSE 'false' END"
		}
		return comparisonSQL(expr, r.Op, val, number)
	}
	p, a := walk(r)
	return p, a, nil
}

type contextReader struct {
	ctx context.Context
	tx  *sql.Tx
}

func (r contextReader) Query(q string, args ...any) (*sql.Rows, error) {
	return r.tx.QueryContext(r.ctx, q, args...)
}

func databasePlaylists(db sqlReader) (map[string]*Playlist, error) {
	rows, err := readModels[playlistRow](db, "playlists", "")
	if err != nil {
		return nil, err
	}
	out := map[string]*Playlist{}
	for _, p := range rows {
		out[p.ID] = &Playlist{ID: p.ID, Name: p.Name, Smart: p.Smart, Sort: p.Sort, Desc: p.Desc}
	}
	if err = loadRules(db, out); err != nil {
		return nil, err
	}
	return out, nil
}

func trackWhere(q Query) (string, []any, error) {
	where := "t.missing=0"
	args := []any{}
	if q.Rule != nil {
		p, a, err := compileRule(*q.Rule)
		if err != nil {
			return "", nil, err
		}
		where += " AND " + p
		args = append(args, a...)
	}
	if q.Search != "" {
		where += " AND instr(harmonia_lower(t.title || ' ' || t.artist_raw || ' ' || t.album_raw || ' ' || t.genre_raw),?)>0"
		args = append(args, strings.ToLower(q.Search))
	}
	return where, args, nil
}

func orderSQL(field string, desc bool) string {
	if field == "" {
		field = "addedAt"
	}
	parts := []string{}
	seen := map[string]bool{}
	for _, f := range []string{field, "artist", "album", "disc", "number", "title"} {
		if seen[f] {
			continue
		}
		seen[f] = true
		expr, number := fieldSQL(f)
		switch f {
		case "addedAt":
			expr = "x.search_added_at"
		case "disc":
			expr = "x.search_disc"
		case "number":
			expr = "x.search_number"
		}
		parts = append(parts, emptySQL(expr, f, number)+" ASC")
		if !number {
			expr = foldSQL(expr)
		}
		direction := " ASC"
		if f == field && desc {
			direction = " DESC"
		}
		parts = append(parts, expr+direction)
	}
	return strings.Join(append(parts, "x.track_id ASC"), ",")
}

const trackQueryFrom = "tracks t JOIN track_search x ON x.track_id=t.id"

// For an unfiltered list, walk the complete sort index first and fetch only
// the requested page. With predicates, let SQLite choose the selective index.
func queryFrom(q Query) string {
	field := q.Sort
	if field == "" {
		field = "addedAt"
	}
	if q.Rule == nil && q.Search == "" && (field == "addedAt" || field == "title") {
		direction := "asc"
		if q.Desc {
			direction = "desc"
		}
		return "track_search x INDEXED BY track_search_order_" + field + "_" + direction + " CROSS JOIN tracks t ON t.id=x.track_id"
	}
	return trackQueryFrom
}

type TrackPage struct {
	Items    []Track `json:"items"`
	Total    int     `json:"total"`
	Page     int     `json:"page"`
	PageSize int     `json:"pageSize"`
}

// Internal callers can explicitly request the complete ordered result.
func (s *Store) QueryTracks(ctx context.Context, q Query) ([]Track, error) {
	q.All = true
	result, err := s.QueryTrackPage(ctx, q)
	return result.Items, err
}

func (s *Store) QueryTrackPage(ctx context.Context, q Query) (TrackPage, error) {
	result := TrackPage{Page: q.Page, PageSize: q.PageSize}
	if !q.All {
		result.Page = max(1, q.Page)
		if q.PageSize != 25 && q.PageSize != 100 {
			result.PageSize = 50
		}
	}
	tx, err := s.queryDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	db := contextReader{ctx, tx}
	manual := false
	if q.PlaylistID != "" {
		ps, err := databasePlaylists(db)
		if err != nil {
			return result, err
		}
		p := ps[q.PlaylistID]
		if p == nil {
			return result, errors.New("playlist not found")
		}
		if p.Smart {
			q.Rule = p.Rule
			q.Sort = p.Sort
			q.Desc = p.Desc
		} else {
			manual = true
		}
	}
	where, args, err := trackWhere(q)
	if err != nil {
		return result, err
	}
	from := queryFrom(q)
	order := orderSQL(q.Sort, q.Desc)
	if manual {
		from = trackQueryFrom
		from += " JOIN playlist_items pi ON pi.track_id=t.id"
		where = "1" + strings.TrimPrefix(where, "t.missing=0") + " AND pi.playlist_id=?"
		args = append(args, q.PlaylistID)
		order = "pi.position"
	}
	suffix := "WHERE " + where + " ORDER BY " + order
	if !q.All {
		// Both statements share a WAL snapshot, including the playlist definition.
		countFrom := from
		if q.Rule == nil {
			countFrom = "tracks t"
			if manual {
				countFrom += " JOIN playlist_items pi ON pi.track_id=t.id"
			}
		}
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM "+countFrom+" WHERE "+where, args...).Scan(&result.Total); err != nil {
			return result, err
		}
		offset := result.Total
		if result.Page <= result.Total/result.PageSize+1 {
			offset = (result.Page - 1) * result.PageSize
		}
		suffix += " LIMIT ? OFFSET ?"
		args = append(args, result.PageSize, offset)
	}
	result.Items, err = readModels[Track](db, from, suffix, args...)
	if q.All {
		result.Total = len(result.Items)
	}
	return result, err
}

func (s *Store) SmartMemberships(ctx context.Context, ids ...string) (map[string][]string, error) {
	tx, err := s.queryDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	db := contextReader{ctx, tx}
	playlists, err := databasePlaylists(db)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	selected := make(map[string]bool, len(ids))
	for _, id := range ids {
		selected[id] = true
	}
	for id, p := range playlists {
		if !p.Smart || (len(ids) > 0 && !selected[id]) {
			continue
		}
		where, args, err := trackWhere(Query{Rule: p.Rule})
		if err != nil {
			return nil, err
		}
		rows, err := db.Query("SELECT t.id FROM "+queryFrom(Query{Rule: p.Rule, Sort: p.Sort, Desc: p.Desc})+" WHERE "+where+" ORDER BY "+orderSQL(p.Sort, p.Desc), args...)
		if err != nil {
			return nil, err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		out[id] = ids
	}
	return out, nil
}
