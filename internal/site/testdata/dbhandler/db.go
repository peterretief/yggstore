// The site's data, as a handler reaches it: a copy of the starter's
// api/db package (YGGSITE-STARTER), in package main for the tests.
package main

import (
	"encoding/json"
	"errors"
	"unsafe"
)

//go:wasmimport yggstore db_query
func dbQuery(req unsafe.Pointer, n uint32, out unsafe.Pointer, room uint32) int32

//go:wasmimport yggstore db_record
func dbRecord(req unsafe.Pointer, n uint32, out unsafe.Pointer, room uint32) int32

func callDB(f func(unsafe.Pointer, uint32, unsafe.Pointer, uint32) int32, req any) ([]byte, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 64<<10)
	for {
		n := f(unsafe.Pointer(&b[0]), uint32(len(b)), unsafe.Pointer(&out[0]), uint32(len(out)))
		if n >= 0 {
			return out[:n], nil
		}
		out = make([]byte, -n)
	}
}

// Query runs a read-only SQL query on the site's data.
func Query(sql string, args ...any) ([]map[string]any, error) {
	if args == nil {
		args = []any{}
	}
	b, err := callDB(dbQuery, map[string]any{"sql": sql, "args": args})
	if err != nil {
		return nil, err
	}
	var r struct {
		Columns []string `json:"columns"`
		Rows    [][]any  `json:"rows"`
		Error   string   `json:"error"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if r.Error != "" {
		return nil, errors.New(r.Error)
	}
	out := make([]map[string]any, len(r.Rows))
	for i, row := range r.Rows {
		out[i] = map[string]any{}
		for j, c := range r.Columns {
			out[i][c] = row[j]
		}
	}
	return out, nil
}

// Result is what came of recording an entry.
type Result struct {
	ID        string `json:"id"`
	Status    string `json:"status"` // applied or refused
	Error     string `json:"error"`
	Confirmed bool   `json:"confirmed"` // every web node has it; otherwise held
}

// Record writes an entry of a kind the site's api/entries.sql has.
func Record(kind string, fields map[string]any) (Result, error) {
	var r Result
	b, err := callDB(dbRecord, map[string]any{"kind": kind, "fields": fields})
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, err
	}
	if r.ID == "" && r.Error != "" {
		return r, errors.New(r.Error)
	}
	return r, nil
}
