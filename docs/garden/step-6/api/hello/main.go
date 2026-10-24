// /api/hello: an example of the site's server code. It runs on the group's
// web nodes, once per request, as an ordinary Go web app served with
// net/http/cgi (see the README). Try it: /api/hello?name=Kath
package main

import (
	"encoding/json"
	"net/http"
	"net/http/cgi"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/hello", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			name = "there"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"greeting": "Hello, " + name})
	})
	cgi.Serve(mux)
}
