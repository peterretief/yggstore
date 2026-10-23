// A site's handler for the tests (handlers_test.go): an ordinary Go web
// app served with net/http/cgi, built for WASI.
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cgi"
	"strings"
	"time"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"greeting": "Hello, " + r.URL.Query().Get("name"), "host": r.Host})
	})
	mux.HandleFunc("POST /api/hello/echo", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Seen-Agent", r.UserAgent())
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(strings.ToUpper(string(b))))
	})
	mux.HandleFunc("GET /api/hello/slow", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Second)
	})
	mux.HandleFunc("GET /api/hello/big", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", 3<<20)))
	})
	mux.HandleFunc("GET /api/hello/crash", func(w http.ResponseWriter, r *http.Request) {
		var m map[string]int
		m["boom"] = 1
	})
	cgi.Serve(mux)
}
