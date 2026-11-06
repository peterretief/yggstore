# Local apps: a public site with a private part on your own machine

Status: approved 2026-11-06; being built.

A site's pages are published to the group, so they stay up whatever happens
to your machine. Its dynamic part, starting with who may sign in, runs on
your own machine, and its data never leaves it. The first user is the
group's admin, on one of their own sites; other developers come after.

```
visitor --https--> web node --- /, /css, /images   served from the group
                      |
                      +-- /app/*  --Yggdrasil-->  your machine: 127.0.0.1:3000
```

## How it differs from site apps

| | [Site apps](site-code.md) | Local apps |
| --- | --- | --- |
| Code runs | on every web node, as WebAssembly | on your machine, as a Go program |
| Data lives | a copy on every web node | on your machine only |
| With your machine off | keeps working | the local part is down |
| Good for | forms, comments, shared data | sign-in, private records, clients' areas |

## What you do

1. Run your Go program on your machine, on a local port. (The relay only
   speaks HTTP, so another language would work, but Go is what's supported.)
2. Add a hidden `.yggstore.json` to the site's folder. It's read when
   publishing and isn't published:

   ```json
   { "local": { "/app/": "http://127.0.0.1:3000" } }
   ```

3. Publish as usual. `yggstore site dev` serves both halves while you work.

## How it works

- **Publishing** tells the web nodes the site's local paths, with the
  site's other settings. They belong to the site's publisher: only that
  node can receive them.
- **A request** under a local path goes from the web node to the
  publisher's node over Yggdrasil, as requests to a person's box do today
  ([web access](web-access.md)). No public IP or open port is needed.
- **On your machine,** a small relay in the node takes these requests only
  from the group's web nodes, and only for your local paths, and passes them
  to your program. It names the site and the visitor's address in headers,
  and removes any such headers the visitor sent.
- **Your machine off:** the web node answers with the site's `_offline.html`,
  or a short "back soon" page. The rest of the site carries on.

## Sign-in, the first use

[`examples/signin`](../examples/signin/main.go) is a starting point, with
`/app/login`, `/app/logout` and `/app/me`: one file to build and run, using
the Go SQLite library yggstore already has. Accounts are in a SQLite file on your machine,
passwords are hashed, wrong tries are limited, and the session is a
`Secure`, `HttpOnly` cookie for the site's own address. Your own programs
can use it, or replace it.

**Your data, the same way.** Write your tables in a `.sql` file and import it
with `-data clients.db -import design.sql`. Edit the file and import again
whenever you like: a mistake in it changes nothing, rows are kept, a copy of
the data file is kept first, and anything that would be removed needs `-drop`.
With `-data`, the starting point shows a diagram of the tables at `/app/data/`,
and a form and a report for each table, all worked out from the tables
themselves: foreign keys are the relationships and become drop-downs, and
column types, `NOT NULL` and `CHECK (x IN (...))` set each field. Reports
search, sort, add up, group and download as CSV. Everyone signed in can see
and change everything here.

**Anything private must come from your machine.** Pages published to the
group are public: a sign-in form in front of a published page protects
nothing, because anyone can ask a web node for it. So a client's private
page is served by your program, under `/app/`, after it checks who's asking.
This is the part most easily got wrong, and the starting point shows how.

## What to know

- **"Local" means stored locally, not hidden in transit.** Traffic is
  encrypted between machines, but Cloudflare and the web node see requests
  as they pass, passwords included, as for any site on them.
- **Your program faces the internet.** Only your listed paths reach it, but it
  must treat every request as hostile.
- **One machine is one point of failure.** With `-copy-to` and a folder in the
  outbox's `files/`, the starting point makes a clean copy of its data each
  day, which the group stores encrypted, keeping earlier days as versions.
  The recovery copy carries what's needed to get it back, so losing the
  machine loses a day of data at most. To carry on from another machine, name it
  beforehand ([a second machine](#a-second-machine)). Otherwise an admin has to
  move the site there, and if the lost machine is the admin's, nobody can yet.
  `-copy-also` keeps `.yggstore.json` with the daily copy, as
  `SITE.yggstore.json`.
- **Limits:** 10 MB a request, 30 seconds an answer and 5 local paths a site
  to begin with.
- **Sites on a box don't get this.** Only a machine's own publisher reads
  `.yggstore.json`, and a local path's program must be on that machine:
  a person's site can't reach programs on the box it lives on.

## Your data on more than one machine

Status: approved 2026-11-06. Nothing is built.

Today a local app's data is on one machine. This plan lets you run the program
on two or more machines of your own, each working by itself when the others
can't be reached, and catching up when they can. So the private part of a site
stays up with one machine off.

It reuses how [site data](site-code.md) works: every write is an entry in a log
kept for good, the machines share their logs, and each one builds its tables by
replaying the entries in one order (time, machine, number). It stays SQLite and
Go, and the design file, diagram, forms and reports stay as they are.

- **Your machines only.** Site data goes to every web node, which is wrong for
  private data. A local app has its own list of machines, chosen by you, and
  entries travel between them over Yggdrasil, encrypted with a key only those
  machines hold. A web node can pass requests to them but never holds the data.
- **Row ids are random.** Two machines offline would both make client number 4,
  so a new row gets a random id. The design file says `id TEXT PRIMARY KEY`, and
  the form fills it in. This has to be settled before real data goes in:
  data with numbered rows would have to be moved over.
- **Forms write entries.** Saving a form adds an entry (this table, this row,
  these fields) instead of changing the table. No SQL has to be written for it,
  as a site's `entries.sql` does: the design says what each table holds.
- **Two edits to one record:** the later one wins, field by field, so edits to
  different fields both stay. A deleted row stays deleted.
- **Rules are checked at replay.** The design's rules (`UNIQUE`, `CHECK`, foreign
  keys) apply the same way on every machine, so an entry that breaks one after
  the merge is refused everywhere. For example, two machines each take the same
  invoice number. A refused entry is listed on a page with what it tried to do,
  so a person can put it right. It's never just dropped.
- **Sign-in too.** Accounts are entries like any others. Sessions and wrong tries
  stay on each machine, so someone sent to another machine signs in again.
- **The design travels with the log.** An import is an entry as well, so every
  machine changes its tables at the same point in the order. Otherwise an entry
  could arrive for a column one machine doesn't have yet.
- **Which machine answers:** the site names other machines for its local paths
  and the web node tries the next if one doesn't answer, so a visitor needn't
  notice. The "back soon" page shows only when none answers. (Built: see
  [a second machine](#a-second-machine).)
- **The daily copy stays.** Sync passes a mistake to every machine within
  minutes. The daily versions are how you go back.

**What to know:** a machine can be behind, so someone may see an older answer
until it catches up, and a save that looks done can be refused at the merge.
Times come from each machine's clock, so a machine with a wrong clock can win
when it shouldn't. The log only grows, and replaying gets slower with it, so it
will need snapshots, which aren't built for site data yet either.

**Build order:**

1. Random ids in the design and forms, with a way to move numbered rows over
   (built: change the ids and what refers to them to `TEXT` and import; `-import`
   notes what's left to change).
2. Entries and replay on one machine, with forms writing entries (built:
   `-machine NAME`; the data file then holds its log, in `_sync_` tables).
3. The refused list (built: `/app/data/_refused`).
4. Sharing the log between your machines, encrypted.
5. The web node trying a second machine (built).
6. Try it with the desktop and one box, by pulling the plug on each in turn.

**Still to decide:** whether a client's account works on every machine from the
start, and how a new machine is added safely (it needs the key, so probably by
hand at first, as a new box is).

## A second machine

A site can name machines that answer its local paths when the publisher
doesn't, with no admin needed at the time. In `.yggstore.json`:

```json
{ "local": { "/app/": "http://127.0.0.1:3741" }, "machines": ["localbox-004"] }
```

On that machine, say where the path goes, as the user its node runs as
(`sudo -H` for a box's root node):

```
sudo -H yggstore site local peter.withcare.co.za /app/ http://127.0.0.1:3741
```

The web node asks the publisher first, then the others in order. A machine with
no program running says so and the next is asked, and one that's off is tried
last for a minute. A form is only sent on if it can't have reached a program,
so nothing is saved twice.

**Until the data syncs, keep the program on the second machine stopped.** Each
machine has its own data file, so with both running, saves would land on
whichever answered and never meet. A stopped program costs nothing: the web node
passes over it. If the publisher's machine dies, restore the latest copy on the
second machine and start the program, and visitors reach it from then on.
Before starting the publisher's machine again, take the data back to it:
it's asked first, and would answer from its older file.

## Build order

1. Local paths in `.yggstore.json`, passed to the web nodes when publishing.
2. The web node's forwarding, and the relay on the node, with the offline
   page.
3. The sign-in starting point, with a private page.
4. `site dev` for both halves.
5. Used for real on one of the admin's sites.
6. A how-to, then offer it to other developers.

## Decided

- `peter.withcare.co.za` goes first.
- Go only, and sign-in on your own machine. The group's login could be an
  option later.
