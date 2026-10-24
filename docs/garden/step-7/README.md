# yggsite starter

A starter website for the group's web nodes: plain pages built with
[Vite](https://vite.dev), and a small WebAssembly module written in Go and
built with [TinyGo](https://tinygo.org). The build is a folder of static
files that you upload as the site.

## What you need

- Node.js 22.13 or later (for its built-in SQLite)
- TinyGo 0.43 or later. TinyGo needs Go 1.25 or later; `wasm/go.mod` names
  a toolchain, so an older Go fetches the right one by itself.

## Working on it

```sh
npm install
npm run dev            # http://localhost:5173, rebuilds as you change things
```

`npm run dev` builds the WebAssembly module once at the start. After
changing `wasm/main.go`, run `npm run wasm` and reload the page.

## Putting it on your website

```sh
SITE=anna.withcare.co.za npm run build
```

This writes the site into `out/anna.withcare.co.za/`. On your files page,
open **Your websites** and upload that folder (or drag it in). Its files
go in at the site's top, in place of those with the same names; the site
shows the new version within a minute or so.

Without `SITE`, the build goes into `dist/`. Upload that inside your
site's own folder (**Your websites → anna.withcare.co.za**) instead: any
folder with an `index.html` becomes the site there.

Files you've removed from the project stay on the site until you delete
them on the files page.

### Pushing it from your computer (the box's admin)

If you look after the box, and can `ssh` to it with `sudo`, one command
builds the site and copies it into its folder on the box:

```sh
cp push.config.example.json push.config.json   # once: the site, the box, its folder
npm run push -- --dry-run                      # what would change
npm run push                                   # build and copy
npm run push -- --delete                       # also remove what the build no longer has
```

`dir` is the site's folder on the box:
`/root/yggstore-hub/users/PERSON/websites/SITE`. `push.config.json` isn't
kept in git. Leave out `--delete` if the site's owner also adds files on
their files page, or the next push removes them.

`only` (optional) lists the pages to push, such as `["register",
"members"]`; `assets/`, which they need, goes too, and nothing else on the
site is touched. Use it to add pages beside a site someone already has.
Leave it out to push the whole build.

Before replacing or removing anything already on the site, the push lists
it (the home page is marked) and asks. `--yes` skips the question. If a
push goes wrong anyway, the site's owner presses **Go back to the version
before** on their website page (or the box's admin runs `yggstore hub site
back PERSON`).

## Pages in Markdown

Write a page as `pages/NAME.md` and it becomes `/NAME/` (`pages/courses/intro.md`
becomes `/courses/intro/`). Each one is put inside `pages/_layout.html`, an
ordinary page with the site's header and scripts, where `{{title}}` and
`{{content}}` go. A page can start with `title:` (and `layout:`, another
`pages/_*.html`) between `---` lines; otherwise its first `#` heading is its
title. Links may name another page's `.md` file, and headings get ids
(`## Start {#start}`, or from their words). `npm run dev` shows them and
reloads on a change; `npm run build` writes them beside the other pages.
`pages/news.md` is an example.

## Server code

Each `api/NAME/` is a small Go program (`package main`) that answers
`/api/NAME` and `/api/NAME/…` on the site: an ordinary Go web app (the
standard library's `http.ServeMux`, or chi) served with `net/http/cgi`.
`npm run build` (and `npm run api`) builds each one into `api/NAME.wasm`
for WASI, which goes up with the site. `api/hello` is an example.

The group's web nodes run it once its publisher turns the site's code on
(`yggstore site code SITE on`; for now only sites the group's admin
publishes). Each request gets a fresh copy with no files and no network,
2 seconds and 64 MB, so it keeps nothing between requests. To try one on
your own computer, as the web nodes would run it:

```sh
npm run api
cd tools/apirun && go run . ../../public/api/hello.wasm GET "/api/hello?name=Kath"
```

`npm run dev` doesn't answer `/api/` yet.

### Data the site keeps

A handler keeps data with the `site/api/db` package:

```go
rows, err := db.Query(`SELECT title, places FROM courses WHERE id = ?`, id) // read-only SQL
res, err := db.Record("register", map[string]any{"course": id, "name": name, "email": email})
```

- `api/schema.sql` makes the tables. Its rules (`UNIQUE`, `CHECK`,
  `REFERENCES`, a trigger's `RAISE`) decide what may be written.
- `api/entries.sql` says what each kind of entry does to them, under a
  `-- kind: NAME` line, using the entry's fields (`:name`) and `:entry_id`,
  `:entry_time`, `:entry_node`, `:entry_kind`. Nothing that differs between
  machines: no `random()` or `datetime('now')`.
- A write is an entry: every web node keeps all of them and builds the
  tables by replaying them in the same order, so they agree with no
  leader. `res.Status` is `applied`, or `refused` with `res.Error` saying
  which rule; `res.Confirmed` is whether every web node has it already
  (otherwise it's held until they meet, normally within a minute).
- `api/register` is an example: registrations for a course, one per
  email, no more than its places. It shows how many places are left, never
  who registered.

The data lives on the group's web nodes. `tools/apirun` can't run a
handler that uses `db`; try those on the site itself.

## How the web nodes serve a site

They serve the files as they are, so a site here:

- **is static.** Nothing runs on the server: no API routes, no server-side
  rendering. JavaScript and WebAssembly run in the visitor's browser.
- **has a folder per page.** `/about/` is `about/index.html`. There's no
  fallback that sends every address to one page, so an app that changes
  the address in the browser should use `/#/about`-style addresses.
  A missing page shows `404.html`.
- **leaves out dot-files** such as `.htaccess`; uploads skip them.
- **is single-threaded WebAssembly.** The pages aren't served with the
  cross-origin isolation headers that threads need.
- **is held back if** it has no `index.html`, has shrunk below half its
  last size, or is over 500 MB.

Pages are cached for five minutes. Vite names its assets after their
contents, so a new build is picked up at once.

## The contact form

`contact/index.html` posts to `/_yggstore/contact`, which the web nodes
turn into a message in your mailbox. Turn the form on first, under
**Your website and messages** on your files page. Fields starting with
`_` steer it: `_subject` is the message's subject, `_next` the page to go
to afterwards, and `_gotcha` a hidden field that catches robots. A field
called `email` becomes the reply address.

## The gallery

`gallery/index.html` shows the photos and files people added to the site
(`/uploads/index.json`, which the site's box keeps), with a preview of
each. People the site's owner listed under **Who may add photos** on their
website page can sign in and add more: photos (JPEG, PNG, WebP, GIF) and
PDFs, up to 10 MB each. The browser makes each preview; the box removes
where a photo was taken before it goes on the site. Anyone can see what's
added; the owner deletes things on their files page, in the site's
`uploads` folder. It works only on a site under the group's domain held
by someone on a box (see yggstore's `docs/site-uploads.md`).

## The database

`npm run db` (part of `dev` and `build`) makes `public/data/site.db` from
`db/schema.sql` and `db/seed.sql`, with the SQLite built into Node, and
the build puts it on the site at `/data/site.db`. The events page opens it
in the browser with [sql.js](https://sql.js.org) (SQLite built to
WebAssembly) and queries it as you type; `src/db.js` has `openDb()` and
`rows(db, sql, params)` for your own pages.

It's for data you publish: events, a price list, a catalogue. Visitors
read it; they can't change it, and anything a page changes is gone on
reload. Data that visitors add and everyone sees needs code on the web
nodes, which is a proposal for now (yggstore's `docs/site-code.md`).

sql.js adds about 320 KB (gzipped) to the pages that use it, once; the
other pages don't load it.

## The WebAssembly module

`wasm/main.go` puts its functions on `window.yggsite` (here `stats` and
`slug`, used by the home page's demo), then sends a `yggsite-ready`
event. `src/wasm.js` loads it. `wasm_exec.js` is copied from TinyGo by
`npm run wasm`, since it must match the TinyGo version that built the
module.
