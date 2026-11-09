// Command signin is the smallest app on yggstore's app package: sign-in,
// people, groups, documents, and the data pages for its design.sql (clients,
// projects and invoices, with who may see what). A site's own app starts as
// a copy of this folder; see docs/app-patterns.md.
//
//	go run ./examples/signin -data data.db -import examples/signin/design.sql
//	go run ./examples/signin -add you@example.org -admin   # asks for a password
//	go run ./examples/signin -data data.db                 # listens on 127.0.0.1:3000
package main

import "github.com/peterretief/yggstore/app"

func main() { app.Main() }
