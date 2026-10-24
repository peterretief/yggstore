# Build a site app on yggstore

Oct 23, 2026 · @Peter Retief

## What you'll build

A small course site, `courses.withcare.co.za` in the examples (use your own name): a list of courses from a database, a price worked out in Go as the visitor picks options, Markdown pages, and a registration form. You build it on the desktop in the dev container, and it goes to the group as plain files; nothing is installed on any yggstore machine.

| Part | Tech | Runs | Live today? |
| --- | --- | --- | --- |
| Pages | Vite (HTML), Markdown | visitor's browser | yes |
| Published data | SQLite, read with sql.js | visitor's browser (read-only) | yes |
| Logic | Go, built with TinyGo to WebAssembly | visitor's browser | yes |
| Forms | the contact form, `/_yggstore/contact` | the web nodes, to your Mail | yes |
| Server code | Go `http.Handler` (net/http, chi), built for WASI | the web nodes, per request | yes, for sites your admin node publishes |
| Stored data | SQLite on every web node, `db.Query` and `db.Record` | the web nodes | yes, for sites your admin node publishes |

Everything is built in the container on your desktop; the web nodes only serve the files and take the form, and the browser runs the pages, the database and the Go module.

Steps 1 to 6 give a working, live app in the browser. Steps 7 and 8 add code that runs on the group's web nodes and data they keep, live since 23 October for sites your admin node publishes.

## Step 1: make the project

A project is a folder in YGGSITES named after the site's address, copied from the starter.

1. Copy the starter (its committed files only, so no `.git` or `node_modules`):

   ```sh
   cd /media/peter/storage1/YGGSITES
   mkdir courses.withcare.co.za
   git -C ../YGGSITE-STARTER archive HEAD | tar -x -C courses.withcare.co.za
   rm courses.withcare.co.za/push.config.example.json
   ```
2. Link it to where it's published: add this line to `sites.conf`.

   ```
   courses.withcare.co.za  local  /media/peter/storage1/YGGSTORE/outfiles/websites/courses.withcare.co.za
   ```
3. Open the live preview in the container, then go to http://localhost:5173:

   ```sh
   ./devbox dev courses.withcare.co.za
   ```

   The first run installs the project's packages. The preview reloads as you save. Leave it running in its own terminal for the next steps.
4. Commit the starting point: `git add courses.withcare.co.za sites.conf && git commit -m "courses: from the starter"`.

What's in the folder: a folder per page (`about/index.html` is `/about/`), `pages/` for Markdown, `db/` for the data, `wasm/` for the Go module, `src/` for the JavaScript and CSS, and `vite.config.js`, which lists the HTML pages.

## Step 2: pages

Write text pages in Markdown, and pages that do things in HTML.

**A Markdown page:** make `pages/how-it-works.md`; it becomes `/how-it-works/`, inside `pages/_layout.html`.

```markdown
---
title: How it works
---
# How it works

Pick a course, see the price, and [register](/register/).

## Who teaches {#teachers}

Kath and the team. See [the courses](/courses/), or [our story](about-us.md).
```

- Links can name another Markdown page's `.md` file; it becomes that page's address.
- Headings get ids to link to: `{#teachers}`, or made from their words.
- Change the look of every Markdown page in `pages/_layout.html` (`{{title}}`, `{{content}}`) and `src/style.css`.

**An HTML page:** make `courses/index.html` (copy `about/index.html` to start), then add it to the `pages` list in `vite.config.js`:

```js
const pages = ["index.html", "courses/index.html", "about/index.html", /* … */ "404.html"];
```

Add both to the menu: the `<nav>` in each HTML page and in `pages/_layout.html`. Replace "Your name" on every page with the site's name.

## Step 3: the data, in SQLite

The courses live in a SQLite file built from SQL in the project, published with the site and read in the visitor's browser. Visitors can't change it; you change it by editing the SQL and publishing again.

1. Add a table to `db/schema.sql`:

   ```sql
   CREATE TABLE courses (
     id     TEXT PRIMARY KEY,         -- 'intro', used in addresses
     title  TEXT NOT NULL,
     starts TEXT NOT NULL,            -- 2026-11-05
     weeks  INTEGER NOT NULL,
     price  INTEGER NOT NULL          -- rand per week
   );
   ```
2. Add its rows to `db/seed.sql`:

   ```sql
   INSERT INTO courses VALUES
     ('intro', 'Running an NGO: the basics', '2026-11-05', 4, 250),
     ('funding', 'Finding funding', '2026-11-19', 6, 300);
   ```
3. Show them on `courses/index.html`: a `<ul id="courses"></ul>` and `<script type="module" src="/src/courses.js"></script>`, with `src/courses.js`:

   ```js
   import "./header.js";
   import { openDb, rows } from "./db.js";
   
   const list = document.getElementById("courses");
   openDb().then((db) => {
     for (const c of rows(db, "SELECT * FROM courses ORDER BY starts")) {
       const li = document.createElement("li");
       li.textContent = `${c.title}, from ${c.starts}, ${c.weeks} weeks`;
       list.append(li);
     }
   });
   ```

`npm run db` rebuilds the file (the preview and every build do it too); restart the preview after changing the SQL. `src/events.js` is a fuller example, with a search box.

## Step 4: logic in Go, in the browser

The price is worked out by Go code, built with TinyGo into `public/wasm/app.wasm` and run in the visitor's browser. The module puts its functions on `window.yggsite`.

1. Add a function to `wasm/main.go`, and register it in `main()` beside `stats` and `slug`:

   ```go
   // price is a course's cost: weeks × rand per week × people, 10% off for 3 or more.
   func price(this js.Value, args []js.Value) any {
   	if len(args) < 3 {
   		return 0
   	}
   	weeks, perWeek, people := args[0].Int(), args[1].Int(), args[2].Int()
   	total := weeks * perWeek * people
   	if people >= 3 {
   		total = total * 9 / 10
   	}
   	return total
   }
   ```

   ```go
   "price": js.FuncOf(price),
   ```
2. Rebuild the module, then reload the preview:

   ```sh
   ./devbox bash -c "cd courses.withcare.co.za && npm run wasm"
   ```
3. Use it from the page. `courses/index.html` needs TinyGo's loader before the page's script: `<script src="/wasm/wasm_exec.js"></script>`. Then in `src/courses.js`:

   ```js
   import { loadWasm } from "./wasm.js";
   
   loadWasm().then((ygg) => {
     const people = document.getElementById("people"); // <input id="people" type="number" value="1" min="1">
     const show = (c) => `R ${ygg.price(c.weeks, c.price, Number(people.value) || 1)}`;
     // … each course's price is show(course)
   });
   ```

The module is about 80 KB. Keep the Go pure (numbers and strings in and out): it can't reach the network or files. Anything a visitor mustn't be able to change, such as a final price, has to be checked again on the server (step 7).

## Step 5: a registration form

A form that posts to `/_yggstore/contact` reaches you as an email in the dashboard's Mail, sealed for you on the way. The starter's `register/index.html` is one to start from.

```html
<form method="post" action="/_yggstore/contact">
  <label>Your name <input name="name" required></label>
  <label>Your email <input name="email" type="email" required></label>
  <label>Course <select name="course" id="course"></select></label>
  <label>People <input name="people" type="number" value="1" min="1"></label>
  <input type="hidden" name="_subject" value="Course registration">
  <input type="hidden" name="_next" value="/thanks/">
  <input name="_gotcha" style="display:none" tabindex="-1" autocomplete="off">
  <button>Register</button>
</form>
```

- Fill the `<select>` from the database, as in step 3, and show the price beside it with `ygg.price`, as in step 4.
- Any field names, up to 30; they arrive in order. `email` becomes the reply-to address.
- `_subject` is the email's subject, `_next` the page shown afterwards, `_gotcha` a hidden trap: a post that fills it is dropped as a robot's.
- Limits: 5 messages per sender in a short time, 100 per site a day.
- It only works on the live site: in the preview, sending it gives "page not found", since only the web nodes answer `/_yggstore/contact`.

Turn the form on once the site is published (step 6): `bin/yggstore site contact courses.withcare.co.za on`, in `/media/peter/storage1/YGGSTORE`.

This collects registrations in your Mail. To keep them on the site instead, with rules such as one per email and a limit on places, use stored data (step 8).

## Step 6: publish, check, roll back

Run these in `/media/peter/storage1/YGGSITES`.

1. See what would go up, then publish:

   ```sh
   ./devbox ./sites push courses.withcare.co.za --dry-run
   ./devbox ./sites push courses.withcare.co.za
   ```

   The push builds the site (pages, Markdown, the database, the Go module) into `out/courses.withcare.co.za/` and copies it into the folder your node publishes from. A new name under withcare.co.za goes online within a minute or so; a domain of its own waits until it's on Cloudflare.
2. Or keep it linked while you work: `./devbox ./sites watch courses.withcare.co.za` publishes on every save (at most one new version a minute). Ctrl-C stops it.
3. Check it live: open `https://courses.withcare.co.za/`, then send the registration form once and look for it in the dashboard's Mail.
4. Roll back if a change was wrong (in `/media/peter/storage1/YGGSTORE`):

   ```sh
   bin/yggstore site versions courses.withcare.co.za     # * is the one online
   bin/yggstore site rollback courses.withcare.co.za     # the version before
   ```
5. Commit the project in YGGSITES, so git has its history as well as the group.

A push refuses a site with another project inside its folder, since it would publish that project's source.

## Step 7: server code on the web nodes

A handler is an ordinary Go web app (the standard library's router, or chi) served with `net/http/cgi` and built for WASI. Each `api/NAME/` in the project answers `/api/NAME` and `/api/NAME/…` on the live site. Every request runs in a fresh sandbox on a web node: no files, no network, no other site, 2 seconds and 64 MB, so nothing is kept in memory between requests (step 8 keeps data).

1. Write `api/hello/main.go` (the starter has this one):

   ```go
   package main
   
   import (
   	"encoding/json"
   	"net/http"
   	"net/http/cgi"
   )
   
   func main() {
   	mux := http.NewServeMux()
   	mux.HandleFunc("GET /api/hello", func(w http.ResponseWriter, r *http.Request) {
   		w.Header().Set("Content-Type", "application/json")
   		json.NewEncoder(w).Encode(map[string]string{"greeting": "Hello, " + r.URL.Query().Get("name")})
   	})
   	cgi.Serve(mux)
   }
   ```

   The project's `api/go.mod` is the module (`site/api`); add chi or another package there with `go get` if you want one.
2. Try it on the desktop first, the way the web nodes run it:

   ```sh
   ./devbox bash -c "cd courses.withcare.co.za && npm run api && cd tools/apirun && go run . ../../public/api/hello.wasm GET '/api/hello?name=Kath'"
   ```
3. Publish as usual (step 6): `npm run build` builds each `api/NAME/` into `api/NAME.wasm` beside the pages.
4. Turn the site's code on, once, in `/media/peter/storage1/YGGSTORE`:

   ```sh
   bin/yggstore site code courses.withcare.co.za on
   ```
5. Check it: `curl "https://courses.withcare.co.za/api/hello?name=Kath"` answers `{"greeting":"Hello, Kath"}`. On 23 October this answered on `peter.withcare.co.za` in about 0.13 seconds, from both web nodes.

| If the handler | The visitor gets |
| --- | --- |
| takes longer than 2 seconds | 504 |
| crashes, or replies with more than 1 MB | 502; the next request runs as usual |
| gets more than 120 requests a minute from one visitor | 429 |
| belongs to a site your admin node doesn't publish | 403, until you can approve other sites |

The `.wasm` files themselves are never handed out. `npm run dev` doesn't answer `/api/` yet, so try handlers with `tools/apirun` or on the live site.

## Step 8: data the site keeps

Registrations, members, bookings and the owner's edits are kept by the web nodes, in SQLite, with the site's own rules. A write is an **entry**; every web node keeps all of them and builds the tables by replaying them in one order (time, web node, number), so the copies agree with no leader, even after being cut off from each other.

1. The tables and their rules, in `api/schema.sql`:

   ```sql
   CREATE TABLE courses (id TEXT PRIMARY KEY, title TEXT NOT NULL, places INTEGER NOT NULL);
   INSERT INTO courses VALUES ('intro', 'Running an NGO: the basics', 12);
   
   CREATE TABLE registrations (
     id TEXT PRIMARY KEY, course TEXT NOT NULL REFERENCES courses,
     name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
     email TEXT NOT NULL CHECK (email LIKE '%_@_%'), at INTEGER NOT NULL,
     UNIQUE (course, email)
   );
   
   CREATE TRIGGER course_full BEFORE INSERT ON registrations
   WHEN (SELECT count(*) FROM registrations WHERE course = NEW.course) >= (SELECT places FROM courses WHERE id = NEW.course)
   BEGIN SELECT RAISE(ABORT, 'the course is full'); END;
   ```
2. What each kind of entry does, in `api/entries.sql`. Use the entry's fields (`:name`) and `:entry_id`, `:entry_time` (unix ms), `:entry_node`, `:entry_kind`. While entries are replayed, `datetime('now')` and `CURRENT_TIMESTAMP` give the entry's own time and `random()` is drawn from the entry, so every web node gets the same; `'localtime'` turns the entry down (keep times in UTC):

   ```sql
   -- kind: register
   INSERT INTO registrations (id, course, name, email, at)
   VALUES (:entry_id, :course, trim(:name), lower(trim(:email)), :entry_time);
   ```
3. Use it from a handler with the `site/api/db` package (in the starter):

   ```go
   rows, err := db.Query(`SELECT places FROM courses WHERE id = ?`, course) // read-only SQL
   res, err := db.Record("register", map[string]any{"course": course, "name": name, "email": email})
   // res.Status: "applied", or "refused" with res.Error naming the rule
   // res.Confirmed: every web node has it; otherwise it's held for now
   ```

   The starter's `api/register` is a whole example: it takes registrations and says how many places are left, never who registered.
4. Publish (step 6). The site's code must be on (step 7). `api/schema.sql` and `api/entries.sql` go up with the site but aren't handed out.

Tried live on 23 October, on `peter.withcare.co.za`:

| Request | Answer |
| --- | --- |
| `GET /api/register?course=intro` | `{"left":12, …}` |
| `POST /api/register` with a name and email | `{"registered":true}` |
| the same email again | 409, "that email address is registered for this course already" |
| `GET` again, at each web node | `{"left":11, …}` from both |

Things to know:

- **Rules decide, the same everywhere.** An entry that breaks a `UNIQUE`, `CHECK`, `REFERENCES` or a trigger's `RAISE` is turned down on every web node, and kept in the log, marked refused.
- **Held, then confirmed.** If a web node is out of reach, an entry is held until they meet (within a minute). Two web nodes cut off at once can both take the last place; when they meet, the earlier one wins on both, and the other is refused. So say "held" to a visitor until `Confirmed` is true.
- **Changing the SQL** in a new version rebuilds the tables from every entry with it, so make a new column or rule work for old entries too.
- **Where it lives:** on every web node, with the entries passed directly between them over Yggdrasil, not where other members can read them. Copies kept in the group, for when both web nodes are lost, are still to come; so is a page for the owner to see who registered.
- **Trying it locally:** `tools/apirun` can't run handlers that use `db`; try those on the site.
