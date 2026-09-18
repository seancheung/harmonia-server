package app

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"unicode"
)

// Scalar model fields map to ordinary SQL columns. JSON is limited to variable
// metadata maps/configuration arrays, never a track, playlist or whole library.
type column struct {
	index int
	name  string
	json  bool
}

func columns(model any) []column {
	typ := reflect.TypeOf(model)
	out := []column{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if tag == "-" || tag == "" {
			continue
		}
		name := ""
		for _, r := range tag {
			if unicode.IsUpper(r) {
				name += "_"
			}
			name += string(unicode.ToLower(r))
		}
		if typ == reflect.TypeOf(Track{}) {
			switch name {
			case "artist", "album", "album_artist", "genre":
				name += "_raw"
			}
		}
		c := column{index: i, name: name}
		switch f.Type.Kind() {
		case reflect.Map, reflect.Slice:
			c.json = true
		}
		out = append(out, c)
	}
	return out
}
func quoted(name string) string { return `"` + name + `"` }
func upsertModel(tx *sql.Tx, table string, model any) error {
	cols, marks, updates := []string{}, []string{}, []string{}
	args := []any{}
	value := reflect.ValueOf(model)
	for _, c := range columns(model) {
		cols = append(cols, quoted(c.name))
		marks = append(marks, "?")
		if c.name != "id" {
			updates = append(updates, quoted(c.name)+"=excluded."+quoted(c.name))
		}
		field := value.Field(c.index)
		var arg any = field.Interface()
		if c.json {
			data, err := json.Marshal(arg)
			if err != nil {
				return err
			}
			arg = string(data)
		}
		if (c.name == "album_id" || c.name == "source_id") && field.Kind() == reflect.String && field.String() == "" {
			arg = nil
		}
		args = append(args, arg)
	}
	_, err := tx.Exec("INSERT INTO "+table+" ("+strings.Join(cols, ",")+") VALUES ("+strings.Join(marks, ",")+") ON CONFLICT(id) DO UPDATE SET "+strings.Join(updates, ","), args...)
	return err
}

type sqlReader interface {
	Query(string, ...any) (*sql.Rows, error)
}

func readModels[T any](db sqlReader, table, suffix string, params ...any) ([]T, error) {
	var model T
	cs := columns(model)
	names := []string{}
	for _, c := range cs {
		names = append(names, quoted(c.name))
	}
	rows, err := db.Query("SELECT "+strings.Join(names, ",")+" FROM "+table+" "+suffix, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var item T
		dest := reflect.ValueOf(&item).Elem()
		args := make([]any, len(cs))
		raw := make([]any, len(cs))
		for i := range args {
			args[i] = &raw[i]
		}
		if err := rows.Scan(args...); err != nil {
			return nil, err
		}
		for i, c := range cs {
			if raw[i] == nil {
				continue
			}
			f := dest.Field(c.index)
			if c.json {
				if err := json.Unmarshal([]byte(fmt.Sprint(raw[i])), f.Addr().Interface()); err != nil {
					return nil, err
				}
				continue
			}
			switch f.Kind() {
			case reflect.String:
				f.SetString(fmt.Sprint(raw[i]))
			case reflect.Bool:
				f.SetBool(raw[i].(int64) != 0)
			case reflect.Int, reflect.Int64:
				f.SetInt(raw[i].(int64))
			case reflect.Float64:
				switch n := raw[i].(type) {
				case float64:
					f.SetFloat(n)
				case int64:
					f.SetFloat(float64(n))
				}
			case reflect.Pointer:
				f.Set(reflect.New(f.Type().Elem()))
				switch n := raw[i].(type) {
				case float64:
					f.Elem().SetFloat(n)
				case int64:
					f.Elem().SetFloat(float64(n))
				}
			}
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

type playlistRow struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Smart bool   `json:"smart"`
	Sort  string `json:"sort"`
	Desc  bool   `json:"desc"`
}

//go:embed schema.sql
var schemaSQL string

func databaseSchema() string { return schemaSQL }
