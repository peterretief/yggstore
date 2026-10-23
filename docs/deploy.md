# Developing and deploying

Everything is developed on the desktop (the group's admin machine) and
goes to the group from there. There are two kinds of thing to deploy:
**websites**, which are files the web nodes serve, and **yggstore itself**,
the program every machine runs.

## Websites

Every site is a folder in `/media/peter/storage1/YGGSITES` (see its
README). `sites.conf` says where each is published from.

| A site that is | Work on it with | It's built by |
| --- | --- | --- |
| plain files | any editor | nothing: the folder is the site |
| Markdown (has `_layout.html`) | any editor; `./sites build SITE` to look | `scripts/md.mjs`, into `.build/SITE/` |
| a project (has `package.json`, from YGGSITE-STARTER) | `npm run dev` in its folder (localhost:5173) | `SITE=… npm run build`, into `out/SITE/` |

To put it online:

```sh
./sites push SITE --dry-run   # what would change
./sites push SITE             # build if needed, then publish
./sites watch SITE            # or: publish on every save (Ctrl-C to stop)
```

For a site this machine publishes, the push copies into
`outfiles/websites/SITE`, and this node publishes it within a minute.
Every version is kept: `bin/yggstore site versions SITE`, `site rollback
SITE`. A person's site on a box goes into their folder there instead, and
their website page has **Go back to the version before**.

What a site can do today: pages, a contact form (`/_yggstore/contact`,
turned on with `yggstore site contact SITE on`), WebAssembly and a
read-only SQLite file in the browser, and group members' sign-in and
uploads. What it can't do yet: store what visitors send, or members of a
site ([site-members.md](site-members.md), [site-code.md](site-code.md)).

## yggstore itself

Work in this repo, on a branch, and test before anything goes out:

```sh
go vet ./... && go test ./...
git commit                    # a deploy builds only committed code
```

Then:

```sh
scripts/deploy.sh --status            # what each machine runs now
scripts/deploy.sh --dry-run all       # build, and say what would happen
scripts/deploy.sh web                 # the web nodes: desktop, then localmail
scripts/deploy.sh localbox-004        # one machine
scripts/deploy.sh boxes               # every other machine
scripts/deploy.sh all
scripts/deploy.sh --back localbox-004 # the binary from before, if something broke
```

- The version is `dev-COMMIT`, so `--status` says which commit each
  machine runs.
- Web nodes go one at a time, each checked before the next, so sites and
  sign-in stay up.
- Each machine keeps the binary it ran before as `….prev`; `--back` puts
  it back.
- A machine is a line in the table at the top of `scripts/deploy.sh`:
  its ssh name, CPU (amd64, arm64, armv7), where its binary is, and the
  services to restart. A new box gets a line there.
- Boxes others use (Kath's, Tim's, Alexander's): deploy there only what has
  been tried on a test machine first, and tell them if anything they see
  changes.
