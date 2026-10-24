// /api/signup: sign-ups for work days.
//
//	GET  /api/signup?day=2026-11-07   places left
//	POST /api/signup                  {"day":"2026-11-07","name":"Anna","email":"anna@example.org"}
package main

import (
	"encoding/json"
	"net/http"
	"net/http/cgi"
	"strings"

	"site/api/db"
)

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/signup", func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.Query(`SELECT d.places - count(s.id) AS left FROM workdays d
			LEFT JOIN signups s ON s.day = d.id WHERE d.id = ? GROUP BY d.id`, r.URL.Query().Get("day"))
		if err != nil || len(rows) == 0 {
			reply(w, http.StatusNotFound, map[string]string{"error": "no such day"})
			return
		}
		reply(w, http.StatusOK, rows[0])
	})
	mux.HandleFunc("POST /api/signup", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Day, Name, Email string }
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil {
			reply(w, http.StatusBadRequest, map[string]string{"error": "send day, name and email"})
			return
		}
		res, err := db.Record("signup", map[string]any{"day": in.Day, "name": in.Name, "email": in.Email})
		switch {
		case err != nil:
			reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		case res.Status != "applied":
			why := "that sign-up can't be taken"
			if strings.Contains(res.Error, "full") {
				why = "the day is full"
			} else if strings.Contains(res.Error, "UNIQUE") {
				why = "you're signed up for that day already"
			}
			reply(w, http.StatusConflict, map[string]string{"error": why})
		default:
			// Not yet Confirmed: taken here, every web node has it within a minute.
			reply(w, http.StatusOK, map[string]any{"signedUp": true, "held": !res.Confirmed})
		}
	})
	cgi.Serve(mux)
}
