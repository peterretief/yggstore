# Websites that do things: code and data on the web nodes

Status: proposed on 2026-10-22; on 2026-10-23 planned together with the
starter (one project, two halves; see [one project](#one-project-two-halves)),
with Kath's site as the first real use. Nothing of the server half is built.

## What we want

Start from the people, not the technology.

1. **Sites that do things, not only show things.** Today a site in the
   group can show pages and take a contact form. People also want a site
   that answers: a booking request, a guestbook, a price worked out from
   what a visitor picks, a sign's text changed from one place (Tim makes
   electronic signs, [signs.md](signs.md)).
2. **The owner never touches code.** Someone like Kath or Tim uses the
   site and changes its words; the code is written once, by a developer.
3. **Code is its own project, apart from the display.** The HTML only
   shows things. The logic lives beside it, built and tested on a
   developer's machine, and goes up with the site as one folder
   ([the starter](#a-starter-to-build-on)).
4. **No box is the only way in.** A site, its code and its data keep
   working when any one machine is off, the owner's box included (see
   [data that lasts](#data-that-lasts)).
5. **Nothing outside the group.** No hosting company, database service or
   cloud function. The data stays in the group, encrypted.
6. **Safe for the people running it.** Web nodes are members' own
   machines; running someone else's code must not put them at risk.

Not aims, at least to start: busy public sites, payments, or anything
that needs every write checked against every other at the same instant
(see [what it can't do](#what-it-cant-do)).

## The shape of it

| Part | What it is | Where it runs |
| --- | --- | --- |
| Display | HTML, CSS, images, a little JavaScript | the visitor's browser |
| Code | WebAssembly handlers, one per action | every web node, for each request |
| Data | records a handler keeps | a copy on every web node, kept in step by the group |

Each part is built from things yggstore already has: sites served by
every web node ([sites.md](sites.md)), topics that catch a node up after
it was off ([messaging.md](messaging.md)), and storage split in shards
across the group ([repair.md](repair.md)).

## Code: WebAssembly handlers

A handler is a `.wasm` file in the site's `api/` folder. A request for
`/api/NAME`, or `/api/NAME/anything`, runs `api/NAME.wasm`. Everything
else is served as files, as now, and the `.wasm` files in `api/` aren't
handed out.

WebAssembly rather than CGI itself, because:

- **the same file runs on every web node**, whatever its machine, so a
  site still has no server of its own;
- **it's sandboxed:** a handler sees only what the web node hands it,
  with no files, network or other sites;
- **it's any language that builds to WASI:** Go (`GOOS=wasip1`), TinyGo
  (`-target wasip1`), Rust (`wasm32-wasip1`), C, Zig.

The web nodes run it with [wazero](https://wazero.io), a WebAssembly
runtime written in Go without cgo, so yggstore stays one binary.

The interface is CGI's, as [WAGI](https://github.com/deislabs/wagi) uses
it: the request's method, path, query and headers in environment
variables (`REQUEST_METHOD`, `PATH_INFO`, `QUERY_STRING`, `CONTENT_TYPE`,
`HTTP_…`), its body on stdin, and the reply on stdout as headers, a blank
line, then the body (`Status: 404` sets the status). In Go:

```go
package main

import (
	"fmt"
	"os"
)

func main() {
	name := os.Getenv("QUERY_STRING") // /api/hello?Anna
	fmt.Print("Content-Type: application/json\n\n")
	fmt.Printf("{\"greeting\": \"Hello, %s\"}\n", name)
}
```

```sh
tinygo build -target wasip1 -o api/hello.wasm ./hello
```

The page stays display only: a form posts to `/api/...`, or a few lines
of JavaScript `fetch` it and put the answer on the page.

### Limits

Each request gets a fresh instance, so one visitor's request can't see
another's. Each one is stopped after about 2 seconds and gets about 64 MB
of memory. Requests and replies are held to 64 KB to start with, as the
contact form's are. Visitors and sites are held to a rate, as the contact
form is (`contactPerIP`, `contactPerSite`). A compiled module is kept per
site version, so it isn't compiled again for each request.

### Who may use it

Code is a bigger step than files, so it's off until the group's admin
turns it on for a site, as names are approved now. A web node's owner can
also refuse to run code at all; the other web nodes serve those sites'
handlers.

## Data that lasts

Every web node serves every site, so a site's data can't live on one of
them, and putting it on the owner's box would stop the site when the box
is off (aim 4). Instead each web node keeps its own copy, and the copies
are kept in step by messages, merging the way the group's machines already
come and go.

- **A write is a message.** A handler's write goes out on the site's data
  topic (`site/NAME/data`). Topics already deliver to nodes that were off
  once they're back, fill gaps from the publisher, and give a new follower
  the last 30 days.
- **Each web node keeps a local copy**, a SQLite file built from the
  messages. Reads are local, so they're quick and work when the web node
  is cut off from the rest.
- **Writes merge without a leader.** Records are CRDTs: values that merge
  to the same result whatever order the messages come in. Counters add
  up, sets and lists combine, and a plain value goes to the latest write
  (by time, then by node, so every copy agrees). Nobody has to be up for
  someone else to write.
- **Snapshots are stored in the group.** Now and then a web node stores
  the site's data as an item, sharded like any file, so it outlives any
  machine and the 30 days of messages. A new web node starts from the
  latest snapshot and the messages since.

A handler sees a small API, not the messages: `get`, `put`, `delete`,
`add` (to a counter or a set) and `list` (by key prefix), as host
functions it imports from the web node.

### Keeping it private

Topics are for the group, not secret from it: any member can follow one,
and messages are readable on the nodes that receive them
([messaging.md](messaging.md)). A site's data shouldn't be readable by
every member, so:

- **each site has a data key.** The site's owner makes it when code is
  turned on for the site, and seals it for each web node's sharing code,
  as a contact form's messages are sealed for the owner's code today;
- **every write is encrypted with it** before it goes on the topic, and
  so is every snapshot. Members who aren't web nodes see only sealed
  messages;
- **when a web node leaves the group, the owner makes a new key**, seals
  it for the web nodes that remain, and the next snapshot is written with
  it. What the departed node already had, it keeps; what's written after,
  it can't read.

The web nodes themselves can read the data, as they can read the sites
they serve. That's the same trust as today, and it's why web nodes are
machines the group chooses.

### Big records

A message holds up to 64 KB. A record bigger than that, such as a photo
someone uploads, is stored as an item in the group, sharded like any
file, and the message carries its stub instead, the way bigger messages
are sent already ([messaging.md](messaging.md)). The stub is sealed with
the site's data key like the rest, so only the web nodes can open it.

### What it can't do

With no leader there is no "only one of these": two web nodes can both
take the last place on a course, or both give out the same name. Most
sites don't need that. One that does sends those writes to a single
place, the owner's box, as the contact form does now, and accepts that
they wait while the box is off. A handler would say so per write
(`put` versus `claim`), so the rest of the site stays always on.

## One project, two halves

The starter (`YGGSITE-STARTER`: Vite pages on Node, a TinyGo module in the
browser, built 2026-10-22) and the handlers are one project, built by one
`npm run build` into one folder that goes up as the site:

| In the project | Built with | Ends up as | Runs |
| --- | --- | --- | --- |
| a folder per page (`about/`, `contact/`…), `src/` | Vite, on Node | `about/index.html`, `assets/…` | the visitor's browser |
| `wasm/` (Go) | TinyGo `-target wasm` | `assets/site-….wasm` | the visitor's browser |
| `api/NAME/` (Go) | TinyGo `-target wasip1` | `api/NAME.wasm` | the web nodes, per request |

- **Go on both sides.** The same Go package (say, working out a price, or
  checking a form) can be built into the browser module, so the page answers
  at once, and into a handler, so the answer is checked where nobody can
  change it.
- **The browser module is only a file** to the web nodes, as now. The
  handlers are the new part.
- **Trying it on your own computer:** `npm run dev` gives the pages, as
  now, and passes `/api/…` to `yggstore site dev out/SITE`, which runs the
  handlers with the same code the web nodes use. What works there works
  on the group.

## Publishing without breaking a live site

On 2026-10-22 a push replaced Kath's page with the starter's, and the
box's shrink guard then held back the fix; getting her page back took a
rollback by hand. So before anything goes on her site:

- **A new page is a new folder.** The first things on Kath's site are
  added beside her page (`/booking/`, say). Her `index.html` is never
  replaced unless she asks for it. `npm run push` adds and changes only the
  folders the project has, and says which before it copies.
- **A preview first.** Every version a box publishes can be seen before
  it's the site, at a preview address the owner and admin open after
  signing in. Only "make this the site" puts it live.
- **One step back.** "Go back to the version before" on the person's
  website page, and `yggstore hub site rollback NAME` for the admin, so no
  one needs the publisher's folder by hand. The shrink guard asks the owner
  instead of holding a site back silently.

## First real use: Kath's site

Kath uses `kath.withcare.co.za` now: one page and a contact form. The
first thing added must be something she wants and uses in one sitting,
tested on her laptop with her (see the build order). A form that hands on
fits stage 2 below and needs no stored data: for example a sign-up for a
course or workshop, with the details checked by a handler and sent to her
mail, sealed for her, as the contact form's messages are. What she wants
is the first question below.

## Stages

0. **Publishing that can't break a site:** new pages as new folders, the
   preview, one step back ([above](#publishing-without-breaking-a-live-site)).
   Nothing else is built until this works on Kath's site.
1. **Handlers that need no data:** checks, conversions, calculations, a
   page built from a query. This is wazero in the web nodes, the CGI
   interface, the limits, the admin's switch per site, and `yggstore site
   dev` for the developer. The starter gets `api/hello`.
2. **Handing on:** a handler sends a message to the site's owner, sealed
   for them, as the contact form does (a booking request, an order).
3. **Data that lasts:** the data topic, the CRDT records, local SQLite,
   snapshots, the data key and big records. The starter gets a guestbook.

Each stage is useful by itself, and each needs the one before. Each is
tried on a test site, then on Kath's, with her.

## Questions

1. **What does Kath want her site to do?** Answered 2026-10-23:
   registration, and course material for members only. That is built into
   the web nodes for any site ([site-members.md](site-members.md)), not as
   a handler; her first handler comes after it. Tim's signs may need
   stage 3.
2. **WASI preview 1 or the component model?** Preview 1 is what TinyGo
   and Go build today and what the CGI interface needs; the component
   model (`wasi:http`) is where WASI is heading, but fewer tools build it.
3. **How long are messages kept?** 30 days of catch-up is enough if
   snapshots are taken often; how often, and how many are kept?
4. **Who makes the data key** when the owner is a person on a box rather
   than a node? Probably their box, which holds their sharing key now.
