// A shop's handler for the tests (handlers_test.go): members and bookings
// in the site's data.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/cgi"
)

func main() {
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, v any, err error) {
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			v = map[string]string{"error": err.Error()}
		}
		json.NewEncoder(w).Encode(v)
	}
	record := func(kind string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var f map[string]any
			json.NewDecoder(r.Body).Decode(&f)
			res, err := Record(kind, f)
			reply(w, res, err)
		}
	}
	mux.HandleFunc("POST /api/shop/join", record("join"))
	mux.HandleFunc("POST /api/shop/book", record("book"))
	mux.HandleFunc("GET /api/shop/members", func(w http.ResponseWriter, r *http.Request) {
		rows, err := Query("SELECT email, name FROM members ORDER BY email")
		reply(w, rows, err)
	})
	mux.HandleFunc("GET /api/shop/bookings", func(w http.ResponseWriter, r *http.Request) {
		rows, err := Query("SELECT email FROM bookings WHERE course = ? ORDER BY id", r.URL.Query().Get("course"))
		reply(w, rows, err)
	})
	mux.HandleFunc("GET /api/shop/write", func(w http.ResponseWriter, r *http.Request) {
		_, err := Query("DELETE FROM members")
		reply(w, map[string]bool{"wrote": err == nil}, nil)
	})
	cgi.Serve(mux)
}
