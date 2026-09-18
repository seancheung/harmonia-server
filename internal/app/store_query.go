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
		"harmonia_number": func(v any) any {
			n, ok := numeric(strings.TrimSpace(fmt.Sprint(v)))
			if !ok {
				return nil
			}
			return n
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
	case "artist", "album", "albumArtist", "genre":
		return map[string]string{"artist": "t.artist_raw", "album": "t.album_raw", "albumArtist": "t.album_artist_raw", "genre": "t.genre_raw"}[field], false
	case "path":
		return `replace(t.path, '\', '/')`, false
	case "bpm":
		return `(SELECT harmonia_number(v.value) FROM json_each(t.tags) j, json_each(j.value) v WHERE j.key IN ('bpm','tbpm','tempo') AND harmonia_number(v.value)>0 ORDER BY CASE j.key WHEN 'bpm' THEN 0 WHEN 'tbpm' THEN 1 ELSE 2 END, v.key LIMIT 1)`, true
	case "key":
		return `coalesce((SELECT harmonia_trim(v.value) FROM json_each(t.tags) j, json_each(j.value) v WHERE j.key IN ('initialkey','initial_key','tkey','key') AND harmonia_trim(v.value)<>'' ORDER BY CASE j.key WHEN 'initialkey' THEN 0 WHEN 'initial_key' THEN 1 WHEN 'tkey' THEN 2 ELSE 3 END, v.key LIMIT 1),'')`, false
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
	if number && field != "playCount" {
		return "(" + expr + " IS NULL OR " + expr + "=0)"
	}
	if number || field == "favorite" {
		return "(" + expr + " IS NULL)"
	}
	return "(harmonia_trim(" + expr + ")='')"
}

func comparisonSQL(expr, op string, val any, number bool) (string, []any) {
	if op == "contains" || op == "notContains" {
		operator := ">0"
		if op == "notContains" {
			operator = "=0"
		}
		return "(" + expr + " IS NOT NULL AND instr(harmonia_lower(" + expr + "),?)" + operator + ")", []any{strings.ToLower(fmt.Sprint(val))}
	}
	operator := map[string]string{"eq": "=", "ne": "<>", "gt": ">", "gte": ">=", "lt": "<", "lte": "<="}[op]
	if operator == "" {
		return "0", nil
	}
	lhs := "harmonia_lower(" + expr + ")"
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
			p, a := comparisonSQL("v.value", op, r.Value, false)
			if op == "isEmpty" || op == "isNotEmpty" {
				p = "harmonia_trim(v.value)<>''"
				a = nil
			}
			args := append([]any{strings.TrimPrefix(r.Field, "tag:")}, a...)
			p = "EXISTS (SELECT 1 FROM json_each(t.tags) j, json_each(j.value) v WHERE j.key=? AND " + p + ")"
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
		parts = append(parts, emptySQL(expr, f, number)+" ASC")
		if !number {
			expr = "harmonia_lower(" + expr + ")"
		}
		direction := " ASC"
		if f == field && desc {
			direction = " DESC"
		}
		parts = append(parts, expr+direction)
	}
	return strings.Join(append(parts, "t.id ASC"), ",")
}

func (s *Store) QueryTracks(ctx context.Context, q Query) ([]Track, error) {
	tx, err := s.queryDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	db := contextReader{ctx, tx}
	manual := false
	if q.PlaylistID != "" {
		ps, err := databasePlaylists(db)
		if err != nil {
			return nil, err
		}
		p := ps[q.PlaylistID]
		if p == nil {
			return nil, errors.New("playlist not found")
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
		return nil, err
	}
	suffix := "WHERE " + where + " ORDER BY " + orderSQL(q.Sort, q.Desc)
	if manual {
		where = strings.TrimPrefix(where, "t.missing=0")
		where = "1" + where
		suffix = "WHERE " + where + " AND t.id IN (SELECT track_id FROM playlist_items WHERE playlist_id=?) ORDER BY (SELECT position FROM playlist_items WHERE playlist_id=? AND track_id=t.id)"
		args = append(args, q.PlaylistID, q.PlaylistID)
	}
	return readModels[Track](db, "tracks t", suffix, args...)
}

func (s *Store) SmartMemberships(ctx context.Context) (map[string][]string, error) {
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
	for id, p := range playlists {
		if !p.Smart {
			continue
		}
		where, args, err := trackWhere(Query{Rule: p.Rule})
		if err != nil {
			return nil, err
		}
		rows, err := db.Query("SELECT t.id FROM tracks t WHERE "+where+" ORDER BY "+orderSQL(p.Sort, p.Desc), args...)
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
