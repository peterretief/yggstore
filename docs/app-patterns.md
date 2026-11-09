# Apps on yggstore: patterns for developers

Status: approved 2026-11-09; step 1 being built.

A developer in the group builds a site with a private part for a client: people
sign in, see what's theirs, fill in forms, read reports, get documents. Today
that exists only in one site (peter.withcare.co.za's `app/`), built for it. This
plan makes it something any developer can start from and keep up to date, and
gives everyone the same small set of patterns, so one developer can pick up
another's app and know where to look.

It builds on [local apps](local-apps.md): pages are published to the group,
and the private part runs on the developer's own machine, with its data there.

## The way of working

| Step | What the developer does | What they get |
| --- | --- | --- |
| 1. Wireframe | Draw the tables in drawDB, and sketch the pages on paper | A picture to agree with the client |
| 2. SQL | Export `design.sql`, and add who may do what as comments in it | One file that says everything about the data |
| 3. Forms | Nothing: `./dev` shows them | A form for each table, from the design |
| 4. Reports | A `CREATE VIEW` for each report the client asks for | Reports that search, sort, total and download |
| 5. Own pages | Go, for what doesn't fit the above | The same sign-in and permission checks |
| 6. Live | `./save`, then `./deploy` | Pages on the group, the private part on their machine |

Steps 2 to 4 need no code at all. The SQL file is the one place a reviewer
reads to know what an app holds and who can see it.

## 1. Permissions: the pattern

**Default: nobody but an admin sees anything.** Everything else is a rule that
gives access, and a table with no rule is for admins only. A mistake then
hides too much, never too little.

**Roles** are fixed and few, so every app means the same by them:

| Role | Sees | Changes |
| --- | --- | --- |
| admin | everything | everything, and People |
| staff | what the rules give staff | what the rules give staff |
| client | their own rows, through `login` (as now) | what the rules give clients |

**Rules go in `design.sql`**, as comments inside the table they are about.
Inside, not above it: SQLite keeps a table's own comments with its design, so
they travel with it, and drops the ones before it (tried 2026-11-09).

```sql
CREATE TABLE projects (
  -- allow: staff read, staff write
  -- allow: client read own
  id TEXT PRIMARY KEY,
  title TEXT NOT NULL,            -- label: Project name
  client_id TEXT NOT NULL REFERENCES clients(id),
  budget REAL,                    -- hide from: client
  notes LONGTEXT                  -- help: Anything the client wants us to know
);
```

- `own` means through `login`, as clients' rows work now: the row, or one it
  links to through foreign keys, names the person.
- Clients only read, for now (see Decided).
- `hide from: client` leaves a column out of their forms, reports and downloads.
- `label:` and `help:` change how a field reads, and nothing else.

**Groups** (built in the site already) share documents, and a table can be
shared the same way with a `group_id` column: everyone in the row's group sees
it, as with `login`.

**One check, everywhere.** A single function decides, for a person, an action
(read, write, delete) and a table, row or column, and every page asks it:
forms, reports, downloads and the developer's own pages. There's no second
way to read the data that forgets to check.

**Tested for every app, not just ours.** A test the starter brings walks every
page as nobody, as a client and as staff, and fails if anything answers that
the rules don't allow. A developer who adds a page gets it tested without
writing a test.

## 2. Reports: views

A report the client asks for, such as "unpaid invoices by client", is a
`CREATE VIEW` in `design.sql`, with an `allow:` rule like a table. It gets the
same page as a table: search, sort, totals, groups, CSV. A view with a `login`
column can be given to clients, and shows each client only their rows. A view
without one is for admins and staff.

## 3. Shared parts, imported, not copied

The private part becomes a Go package, `yggstore/app`, which a developer's own
`main.go` imports: sign-in, people, groups, roles, documents, forms, reports,
the log and the daily copy into yggstore. Their own pages sit beside it, and
call the same checks.

Copying doesn't work: after two days, the site's copy and the starter already
differ by hundreds of lines. Imported, a fix to sign-in reaches every app at
its next build, and a developer's own code stays small.

## 4. Starting an app

A public template repository on GitHub holds the pages (as the site starter
does now), `app/` with a short `main.go`, a sample `design.sql`, and `./dev`,
`./save` and `./deploy`. A developer presses "Use this template", which makes
their own private repo, as Peter's site has.

`./dev` and `./deploy` move into yggstore itself (`yggstore site dev`,
`yggstore site deploy`), so they work on any member's machine, not just in
Peter's YGGSITES folder.

## 5. Security a developer can count on

- Passwords hashed, wrong tries limited, sessions in `HttpOnly`, `Secure`
  cookies for the site's `/app/` only.
- Downloads always as files, never shown as pages.
- No SQL from a form or address reaches the database: names come only from the
  design, values only as parameters.
- An app's data never leaves its machine, except encrypted into yggstore.
- What a developer must still do: treat their own pages' input as hostile, and
  start each one with the check.

## Build order

1. The permission check, the rules in `design.sql`, and staff (in Peter's site,
   with the walk-every-page test).
2. Views as reports.
3. The package: move the site's `app/` into `yggstore/app`, and the site
   imports it.
4. `yggstore site dev` and `yggstore site deploy`.
5. The template repo, and a how-to; then one other developer tries it.

## Decided

- **Roles are fixed:** admin, staff and client, the same in every app.
- **Clients only read** to begin with. Writing their own rows (`client write
  own`, limited to listed columns) comes when an app needs it.
- **One developer at a time** while it settles, starting with Peter's own site.
