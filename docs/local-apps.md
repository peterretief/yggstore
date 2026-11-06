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
- **One machine is one point of failure.** Running the program on a second
  machine, with its data kept in step, is for later.
- **Limits:** 10 MB a request, 30 seconds an answer and 5 local paths a site
  to begin with.
- **Sites on a box don't get this.** Only a machine's own publisher reads
  `.yggstore.json`, and a local path's program must be on that machine:
  a person's site can't reach programs on the box it lives on.

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
