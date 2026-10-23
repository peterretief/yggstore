# yggstore CLI cheatsheet

Oct 23, 2026 · @Peter Retief

## Websites (YGGSITES)

Run these in `/media/peter/storage1/YGGSITES`. Put `./devbox` in front of any of them to run it in the container (Node, Go and TinyGo inside, nothing installed here).

| Command | What it does |
| --- | --- |
| `./sites list` | every site, and where it's published from (`sites.conf`) |
| `./sites pull SITE` or `./sites pull all` | copy the live files here (skips projects and Markdown sites, and anything with uncommitted changes) |
| `./sites push SITE --dry-run` | build if needed, then show what would change |
| `./sites push SITE` | build if needed, then publish |
| `./sites push SITE --delete` | also remove files the folder no longer has |
| `./sites watch SITE` | publish on every save; Ctrl-C stops it |
| `./sites build SITE` | build a plain Markdown site into `.build/SITE/` to look at |
| `./devbox` | a shell in the container |
| `./devbox dev SITE` | a project's live preview at http://localhost:5173 |
| `./devbox ./sites watch SITE` | publish on every save, from the container |
| `./devbox --rebuild` | rebuild the container after changing `container/Dockerfile` |

A new site: make `YGGSITES/SITE/` (named after its address) and add a line to `sites.conf`: `SITE  local  /media/peter/storage1/YGGSTORE/outfiles/websites/SITE`. Never put a project inside another site's folder; a push refuses it.

## Inside a site project

A project is a site folder with a `package.json` (made from YGGSITE-STARTER), such as `peter.withcare.co.za`. Run these in its folder, or through `./devbox`.

| Command | What it does |
| --- | --- |
| `npm run dev` | live preview at http://localhost:5173, reloads as you save |
| `SITE=peter.withcare.co.za npm run build` | build the site into `out/SITE/` (a push does this for you) |
| `npm run wasm` | rebuild the WebAssembly module after changing `wasm/main.go` |
| `npm run db` | rebuild `public/data/site.db` from `db/*.sql` |

| To change | Edit |
| --- | --- |
| a Markdown page, or a new one | `pages/NAME.md` becomes `/NAME/` |
| how Markdown pages look | `pages/_layout.html` (`{{title}}`, `{{content}}`), `src/style.css` |
| the data (events, courses, prices) | `db/schema.sql`, `db/seed.sql` |
| the WebAssembly module | `wasm/main.go` |
| an HTML page | `about/index.html` and so on, listed in `vite.config.js` |

A Markdown page can start with `title:` and `layout:` between `---` lines. Link to another page by its `.md` file; headings get ids (`## Start {#start}`).

## Sites your node publishes

Run these in `/media/peter/storage1/YGGSTORE` as `bin/yggstore …`. They cover the sites in `outfiles/websites/` (peter.withcare.co.za, ngoacademy.co.za and the others marked `local` in `sites.conf`).

| Command | What it does |
| --- | --- |
| `yggstore site list` | the sites this machine publishes |
| `yggstore site versions SITE` | its versions, newest first; `*` is the one online |
| `yggstore site rollback SITE` | put the version before back online |
| `yggstore site rollback SITE VERSION` | put that version online (the first 8 characters are enough) |
| `yggstore site contact SITE on` | turn its contact form on; messages arrive in your dashboard's Mail (`off` turns it off) |
| `yggstore site status` | which web nodes serve which version |
| `yggstore site publish FOLDER -name SITE` | publish a folder by hand (the folder in `outfiles/websites` does this by itself) |
| `yggstore site route SITE` | add its tunnel route and DNS in Cloudflare only |
| `yggstore site owner SITE NODE` | (admin) let another node publish a site on its own domain |
| `yggstore site announce` | tell the web nodes about every site again (a new web node) |
| `yggstore site remove SITE -yes` | take it offline and delete every version |

A new domain is published only once it's on Cloudflare; until then a `SITE - NOT PUBLISHED.txt` beside its folder says what to do.

## People on a box

Run these on the box, as root: from the desktop, `ssh -t pi4 'sudo yggstore hub …'` (pi4 is localbox-004). The box's dashboard does the same under **Admin → People on this box**.

| Command | What it does |
| --- | --- |
| `yggstore hub people` | everyone on the box, how they sign in, their folder |
| `yggstore hub person add NAME` | add someone and print their temporary password |
| `yggstore hub person password NAME` | a new temporary password; the old one, and the browsers it signed in, stop working |
| `yggstore hub person invite NAME` | a link for a device with Yggdrasil (the same link until used) |
| `yggstore hub person limit NAME GB DEVICES` | how much they may store and how many devices (0 = no limit) |
| `yggstore hub person suspend NAME` | shut them out for now (`resume` lets them back) |
| `yggstore hub person remove NAME` | shut them out for good; their files stay on the box |
| `yggstore hub site versions NAME` | their site's versions |
| `yggstore hub site back NAME` | put their site's version before online, and in their folder |
| `yggstore hub site back NAME VERSION` | put that version online |
| `yggstore hub site keep NAME` | drop a held-back change; the folder gets the live version again |
| `yggstore hub site anyway NAME` | publish a held-back change as it is |

They sign in at https://my.withcare.co.za with their group address (`kath@withcare.co.za`), or `NAME@BOX` without one. Give a temporary password by phone or in person, never by email; it works for a week.

## Deploying yggstore

Run these in `/media/peter/storage1/YGGSTORE`. A deploy builds only committed code, versioned `dev-COMMIT`.

1. `go vet ./... && go test ./...`
2. `git commit`
3. `scripts/deploy.sh --dry-run all` to check, then deploy.

| Command | What it does |
| --- | --- |
| `scripts/deploy.sh --status` | the version each machine runs |
| `scripts/deploy.sh --dry-run all` | build for every CPU, and say what would happen |
| `scripts/deploy.sh web` | the web nodes: desktop, then localmail, each checked before the next |
| `scripts/deploy.sh localbox-004` | one machine (any name in the table at the top of the script) |
| `scripts/deploy.sh boxes` | every machine that isn't a web node |
| `scripts/deploy.sh all` | everything |
| `scripts/deploy.sh --back localbox-004` | put back the binary it ran before the last deploy |
| `scripts/deploy.sh --dirty …` | deploy uncommitted code anyway (as `dev-COMMIT-dirty`) |

Each machine keeps its previous binary as `….prev`. A new machine is one line in the script's table: name, ssh name, CPU, binary, services.

## Checking and fixing

| Machine | ssh name | What it is | Its binary |
| --- | --- | --- | --- |
| desktop | (this machine) | web node, admin, your sites | `YGGSTORE/bin/yggstore` |
| localmail | `localmail` | web node, and Tim's box | `/usr/local/bin/yggstore` |
| localbox-004 | `pi4` | Kath's box | `/usr/local/bin/yggstore` |
| localbox-003 | `pi3` | Alexander's box | `/usr/local/bin/yggstore` |
| localbox-002 | `pi2` | storage node (Alpine) | `/mnt/data/yggstore/bin/yggstore` |

| To | Run |
| --- | --- |
| see what version a machine runs | `scripts/deploy.sh --status` |
| follow the desktop's log | `journalctl --user -u yggstore-node -f` |
| follow a box's log | `ssh pi4 'sudo journalctl -u yggstore-hub -f'` |
| undo a site change (your node) | `bin/yggstore site rollback SITE` |
| undo a site change (a person on a box) | `ssh -t pi4 'sudo yggstore hub site back NAME'` |
| undo a deploy | `scripts/deploy.sh --back MACHINE` |
| restart the desktop's node | `systemctl --user restart yggstore-node` |
| open a box's dashboard | `http://[its Yggdrasil address]:7480/admin` |

Where things live: the sites in `/media/peter/storage1/YGGSITES`, the starter in `/media/peter/storage1/YGGSITE-STARTER`, yggstore in `/media/peter/storage1/YGGSTORE` (plans in `docs/`, the deploy guide in `docs/deploy.md`).
