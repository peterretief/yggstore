# Make a website on yggstore, step by step

Oct 23, 2026 · @Peter Retief

## What you need

A website is a folder in `YGGSITES`, named after its address; `./sites push` puts it online. Everything runs on this desktop in the dev container (`./devbox`), so nothing is installed here or on any yggstore machine.

The examples build one site, `garden.withcare.co.za`, a step at a time. Use your own name in its place.

| Step | What the site can do after it |
| --- | --- |
| 1–2 | One page, live |
| 3–5 | Pages written in Markdown, a shared look, published as you save |
| 6 | A live preview while you work, and data shown from the site's own database |
| 7 | Go code that runs in the visitor's browser |
| 8–9 | A form whose answers are kept, on every web node |
| 10 | Back to any earlier version |

All commands run from `/media/peter/storage1/YGGSITES`.

Stuck? The garden as it stands after each step is in [docs/garden](garden/README.md): step 1, 3, 4, 6, 7 and 8 (the other steps don't change the folder). To start again from one, put it in place of your folder: `rm -r garden.withcare.co.za && cp -r ../YGGSTORE/docs/garden/step-6 garden.withcare.co.za` (commit your own first if you want to keep it).

## Step 1: a folder and a first page

Make the folder, then write `index.html`, the page the site opens on. A site needs one to be published.

```sh
mkdir garden.withcare.co.za
```

`garden.withcare.co.za/index.html`:

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>The garden</title>
</head>
<body>
  <h1>The garden</h1>
  <p>Open on Saturdays, 9 to 12.</p>
</body>
</html>
```

Open the file in a browser to look at it. Then tell `sites.conf` where the site is published from: one line, site, host, folder. `local` means this desktop, whose dashboard publishes `outfiles/websites`.

```
garden.withcare.co.za   local   /media/peter/storage1/YGGSTORE/outfiles/websites/garden.withcare.co.za
```

For a site on a person's box, the host is the box and the folder is in their files, as on Kath's line:

```
kath.withcare.co.za     localbox-004   /root/yggstore-hub/users/Kath/websites/kath.withcare.co.za
```

The garden after this step: [garden/step-1](garden/step-1/).

## Step 2: publish it

Check what would go up, then publish:

```sh
./sites push garden.withcare.co.za --dry-run
./sites push garden.withcare.co.za
```

```
sending incremental file list
index.html
Pushed: https://garden.withcare.co.za shows it within a minute or so.
```

Open `https://garden.withcare.co.za/` a minute later. A name under withcare.co.za needs nothing else. A domain of its own (`garden.org.za`) must be added to Cloudflare first; until then a `garden.org.za - NOT PUBLISHED.txt` note beside its folder in `outfiles/websites` says what to do.

Commit it, so git keeps the history too:

```sh
git add garden.withcare.co.za sites.conf && git commit -m "garden: first page"
```

## Step 3: pages in Markdown, with one layout

A folder with a `_layout.html` is a Markdown site: write each page as a `.md` file, and the layout goes around every one of them. Replace `index.html` with `index.md`.

`garden.withcare.co.za/_layout.html`, the page around each one; `{{title}}` and `{{content}}` are filled in:

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{title}} · The garden</title>
  <link rel="stylesheet" href="/style.css">
</head>
<body>
  <nav><a href="/">Home</a> <a href="/visit/">Visit</a></nav>
  <main>
{{content}}
  </main>
</body>
</html>
```

`garden.withcare.co.za/index.md`, the home page:

```markdown
---
title: Home
---
# The garden

Open on Saturdays, 9 to 12. [How to find us](visit.md).
```

`garden.withcare.co.za/visit.md` becomes `/visit/`:

```markdown
---
title: Visit
---
# Visit

## Where {#where}

12 Oak Street, behind the hall.

## What to bring

- Gloves
- Water
```

| You write | You get |
| --- | --- |
| `index.md` | `/` |
| `visit.md` | `/visit/` |
| `courses/intro.md` | `/courses/intro/` |
| `[text](visit.md)` | a link to `/visit/` |
| `## Where {#where}` | a heading you can link to as `/visit/#where` |

Look at it before publishing: `./sites build garden.withcare.co.za` writes the pages into `.build/garden.withcare.co.za/`. A push builds it for you. Files whose names start with `_` or `.`, and `README.md`, are never published.

The garden after this step: [garden/step-3](garden/step-3/).

## Step 4: styles and images

CSS, images and other files go up exactly as they are, at the same path. The layout above already links `/style.css`.

`garden.withcare.co.za/style.css`:

```css
body { font: 18px/1.6 system-ui, sans-serif; max-width: 40rem; margin: 0 auto; padding: 1rem; color: #222; background: #fdfcf8; }
nav a { margin-right: 1rem; color: #2f6b3a; }
img { max-width: 100%; height: auto; border-radius: 6px; }
@media (prefers-color-scheme: dark) { body { color: #eee; background: #1b1d1a; } nav a { color: #8fd19e; } }
```

Put pictures in a folder, say `img/`, and use them from any page:

```markdown
![The vegetable beds in spring](/img/beds.jpg)
```

In HTML: `<img src="/img/beds.jpg" alt="The vegetable beds in spring" width="1200" height="800">`.

- Start addresses with `/`, so they work from every page, `/visit/` included.
- Shrink photos before adding them: 1600 pixels wide is plenty, and a site holds at most 500 MB.
- Write `alt` text that says what the picture shows.

The garden after this step: [garden/step-4](garden/step-4/).

## Step 5: publish as you save

Link the folder to the live site. Each change is published a couple of seconds after you save:

```sh
./devbox ./sites watch garden.withcare.co.za
```

```
Watching garden.withcare.co.za: each change is published to https://garden.withcare.co.za/ within a minute. Ctrl-C to stop.

17:40:12: publishing garden.withcare.co.za…
  Pushed: https://garden.withcare.co.za shows it within a minute or so.
```

- Ctrl-C stops it.
- Add `--delete` to also remove pages you deleted from the folder. Without it, a deleted page stays online.
- At most one new version a minute goes online; every version is kept (step 10).
- Commit in git now and then, as a record of what you changed.

## Step 6: a project, with a live preview

When a site needs more than pages (a list from a database, a form, code), make it a project from the starter. A project is a site folder with a `package.json`; a push builds it first.

1. Turn the garden into a project. Move your Markdown into `pages/`, copy the starter in, move the pictures into `public/`, and take out the starter's demo site:

   ```sh
   cd garden.withcare.co.za
   mkdir pages && mv visit.md _layout.html pages/ && rm index.md
   git -C ../../YGGSITE-STARTER archive HEAD | tar -x --skip-old-files
   mkdir -p public && mv img public/
   rm -r index.html 404.html about contact events gallery register signin thanks pages/news.md \
     push.config.example.json api/register api/schema.sql api/entries.sql
   cd ..
   ```

   - `--skip-old-files` keeps your layout where the starter has its own.
   - In a project, only files in `public/` go up exactly as they are, so pictures go in `public/img/` and are still `/img/beds.jpg` on the pages. Left in `img/`, they show in the preview but not on the live site.
   - The `rm` takes out the starter's demo: its pages and its sample server code, so only the garden is left. They're still in `YGGSITE-STARTER` if you want one back.

   In a project the home page is HTML, so its words move from `index.md` into a new `garden.withcare.co.za/index.html`:

   ```html
   <!doctype html>
   <html lang="en">
   <head>
     <meta charset="utf-8">
     <meta name="viewport" content="width=device-width, initial-scale=1">
     <title>The garden</title>
     <link rel="stylesheet" href="/style.css">
   </head>
   <body>
     <nav><a href="/">Home</a> <a href="/visit/">Visit</a></nav>
     <main>
       <h1>The garden</h1>
       <p>Open on Saturdays, 9 to 12. <a href="/visit/">How to find us</a>.</p>
     </main>
   </body>
   </html>
   ```

   The HTML pages are listed in `vite.config.js`; for now there's one. Change its `pages` line to:

   ```js
   const pages = ["index.html"];
   ```

   The menu is in `pages/_layout.html` for every Markdown page, and in each HTML page, which has its own copy. A new page's link goes in each.

   Commit it: `git add garden.withcare.co.za && git commit -m "garden: a project"`.
2. Open the preview, then go to http://localhost:5173:

   ```sh
   ./devbox dev garden.withcare.co.za
   ```

   It reloads as you save. Ctrl-C stops it.
3. Show data from the site's own database. Replace what's in `db/schema.sql` (the starter's events) with the table, and what's in `db/seed.sql` with its rows:

   ```sql
   CREATE TABLE days (day TEXT PRIMARY KEY, open TEXT NOT NULL);
   INSERT INTO days VALUES ('Saturday', '9 to 12'), ('Wednesday', '16 to 18');
   ```

   Then on the home page, `index.html`: put `<ul id="days"></ul>` in `<main>` where the list should go, and `<script type="module" src="/src/days.js"></script>` in `<head>`, under the stylesheet. Any HTML page will do, but not a Markdown one: a script in a `.md` page works in the preview and is missing from the live site. Then write `src/days.js`:

   ```js
   import { openDb, rows } from "./db.js";
   
   openDb().then((db) => {
     for (const d of rows(db, "SELECT * FROM days")) {
       const li = document.createElement("li");
       li.textContent = `${d.day}: ${d.open}`;
       document.getElementById("days").append(li);
     }
   });
   ```

   The database is built into `public/data/site.db` and read in the visitor's browser. Visitors can't change it; you change the SQL and publish again. Restart the preview after changing the SQL.
4. Publish as before: `./devbox ./sites push garden.withcare.co.za`, or `watch`.

| In the project | What it's for |
| --- | --- |
| `pages/*.md`, `pages/_layout.html` | Markdown pages and their layout |
| `about/index.html` and so on | HTML pages, listed in `vite.config.js` |
| `src/` | the pages' JavaScript and `style.css` |
| `db/schema.sql`, `db/seed.sql` | the published, read-only database |
| `wasm/main.go` | Go that runs in the browser (step 7) |
| `api/` | code that runs on the web nodes (step 8) |

The garden after this step: [garden/step-6](garden/step-6/).

## Step 7: Go in the browser

Code that works something out, such as a calculator, can be written in Go. TinyGo builds it into a small WebAssembly module, `public/wasm/app.wasm`, which runs in the visitor's browser. The starter's `wasm/main.go` already has two functions, `stats` and `slug`; this adds a third, for how much compost a bed needs.

1. Add the function to `wasm/main.go`, with `"math"` in its `import` list:

   ```go
   // compost is how many 40-litre bags fill a bed: its length and width in
   // metres, and the depth in centimetres.
   func compost(this js.Value, args []js.Value) any {
   	if len(args) < 3 {
   		return 0
   	}
   	litres := args[0].Float() * args[1].Float() * args[2].Float() * 10
   	return int(math.Ceil(litres / 40))
   }
   ```

   Then put it on `window.yggsite`, in `main()` beside the other two:

   ```go
   "compost": js.FuncOf(compost),
   ```
2. A page for it, `compost/index.html`. TinyGo's loader, `/wasm/wasm_exec.js`, comes before the page's own script:

   ```html
   <!doctype html>
   <html lang="en">
   <head>
     <meta charset="utf-8">
     <meta name="viewport" content="width=device-width, initial-scale=1">
     <title>Compost · The garden</title>
     <link rel="stylesheet" href="/style.css">
     <script src="/wasm/wasm_exec.js"></script>
     <script type="module" src="/src/compost.js"></script>
   </head>
   <body>
     <nav><a href="/">Home</a> <a href="/visit/">Visit</a> <a href="/compost/">Compost</a></nav>
     <main>
       <h1>How much compost?</h1>
       <form id="bed">
         <label>Length (m) <input name="length" type="number" value="2" min="0" step="0.1"></label>
         <label>Width (m) <input name="width" type="number" value="1" min="0" step="0.1"></label>
         <label>Depth (cm) <input name="depth" type="number" value="5" min="0"></label>
       </form>
       <p id="bags"></p>
     </main>
   </body>
   </html>
   ```

   Add `"compost/index.html"` to the `pages` list in `vite.config.js`, and the Compost link to the menus in `index.html` and `pages/_layout.html`.
3. The page's script, `src/compost.js`. `loadWasm()` (in the starter's `src/wasm.js`) loads the module once and gives back its functions:

   ```js
   import { loadWasm } from "./wasm.js";

   loadWasm().then((ygg) => {
     const form = document.getElementById("bed");
     const show = () => {
       const [length, width, depth] = ["length", "width", "depth"].map((n) => Number(form.elements[n].value) || 0);
       document.getElementById("bags").textContent = `${ygg.compost(length, width, depth)} bags of 40 litres`;
     };
     form.addEventListener("input", show);
     show();
   });
   ```
4. Rebuild the module, then reload the preview at http://localhost:5173/compost/:

   ```sh
   ./devbox bash -c "cd garden.withcare.co.za && npm run wasm"
   ```

   A bed 2 m by 1 m, 5 cm deep, needs 100 litres: 3 bags. Restarting the preview (`./devbox dev`) and every push rebuild the module too.

- The module is about 80 KB, and visitors download it once.
- Keep the Go to numbers and strings in, numbers and strings out: in the browser it can't reach files or the network. Use JavaScript for the page itself.
- Go that runs in the browser can be changed by anyone who wants to. Anything that must be right, such as a price someone pays, is checked again by code on the web nodes (step 8).

The garden after this step: [garden/step-7](garden/step-7/).

## Step 8: a form whose answers are kept

A sign-up form for work days: the page sends the form to `/api/signup`, a small Go program on the web nodes checks it and records it, and every web node keeps the same list. Four files, all in the project.

**1. The tables and their rules**, `api/schema.sql`. A rule that fails turns the sign-up down, the same way on every web node:

```sql
CREATE TABLE workdays (id TEXT PRIMARY KEY, places INTEGER NOT NULL);
INSERT INTO workdays VALUES ('2026-11-07', 10), ('2026-11-21', 10);

CREATE TABLE signups (
  id    TEXT PRIMARY KEY,
  day   TEXT NOT NULL REFERENCES workdays,
  name  TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
  email TEXT NOT NULL CHECK (email LIKE '%_@_%'),
  UNIQUE (day, email)
);

CREATE TRIGGER day_full BEFORE INSERT ON signups
WHEN (SELECT count(*) FROM signups WHERE day = NEW.day) >= (SELECT places FROM workdays WHERE id = NEW.day)
BEGIN SELECT RAISE(ABORT, 'the day is full'); END;
```

**2. What a sign-up does**, `api/entries.sql`. `:day`, `:name` and `:email` are the sign-up's fields; `:entry_id` is its own id:

```sql
-- kind: signup
INSERT INTO signups (id, day, name, email)
VALUES (:entry_id, :day, trim(:name), lower(trim(:email)));
```

**3. The handler**, `api/signup/main.go`, an ordinary Go web app:

```go
// /api/signup: sign-ups for work days.
//
//	GET  /api/signup?day=2026-11-07   places left
//	POST /api/signup                  {"day":"2026-11-07","name":"Anna","email":"anna@example.org"}
package main

import (
	"encoding/json"
	"net/http"
	"net/http/cgi"
	"strings"

	"site/api/db"
)

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/signup", func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.Query(`SELECT d.places - count(s.id) AS left FROM workdays d
			LEFT JOIN signups s ON s.day = d.id WHERE d.id = ? GROUP BY d.id`, r.URL.Query().Get("day"))
		if err != nil || len(rows) == 0 {
			reply(w, http.StatusNotFound, map[string]string{"error": "no such day"})
			return
		}
		reply(w, http.StatusOK, rows[0])
	})
	mux.HandleFunc("POST /api/signup", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Day, Name, Email string }
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil {
			reply(w, http.StatusBadRequest, map[string]string{"error": "send day, name and email"})
			return
		}
		res, err := db.Record("signup", map[string]any{"day": in.Day, "name": in.Name, "email": in.Email})
		switch {
		case err != nil:
			reply(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		case res.Status != "applied":
			why := "that sign-up can't be taken"
			if strings.Contains(res.Error, "full") {
				why = "the day is full"
			} else if strings.Contains(res.Error, "UNIQUE") {
				why = "you're signed up for that day already"
			}
			reply(w, http.StatusConflict, map[string]string{"error": why})
		default:
			// Not yet Confirmed: taken here, every web node has it within a minute.
			reply(w, http.StatusOK, map[string]any{"signedUp": true, "held": !res.Confirmed})
		}
	})
	cgi.Serve(mux)
}
```

`db.Query` reads (it can't change anything); `db.Record` adds an entry of a kind from `entries.sql`. The starter's `api/register` is the same thing for courses.

**4. The page**, `signup/index.html` (add `"signup/index.html"` to the `pages` list in `vite.config.js`):

```html
<h1>Sign up for a work day</h1>
<form id="signup">
  <label>Day <select name="day"><option>2026-11-07</option><option>2026-11-21</option></select></label>
  <label>Your name <input name="name" required></label>
  <label>Your email <input name="email" type="email" required></label>
  <button>Sign up</button>
</form>
<p id="said" role="status"></p>
<script type="module">
  const form = document.getElementById("signup");
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const r = await fetch("/api/signup", { method: "POST", body: JSON.stringify(Object.fromEntries(new FormData(form))) });
    const out = await r.json();
    document.getElementById("said").textContent = r.ok ? "Thank you, you're on the list." : out.error;
  });
</script>
```

A push builds `api/signup/` into `api/signup.wasm` and takes the two SQL files with it. None of the three is handed out to visitors. The form only works on the live site, once the code is on (step 9): the preview doesn't answer `/api/`.

Need only an email of each answer, no list kept? Post the form to `/_yggstore/contact` instead and turn it on with `bin/yggstore site contact garden.withcare.co.za on`; answers then arrive in the dashboard's Mail.

The garden after this step: [garden/step-8](garden/step-8/).

## Step 9: turn the code on

A site's `api/` code runs only once it's switched on, once per site. Until then `/api/signup` answers 404, or 403 with a note.

**A site this desktop publishes** (`local` in `sites.conf`): one command, in `/media/peter/storage1/YGGSTORE`:

```sh
bin/yggstore site code garden.withcare.co.za on
```

**A site on a person's box** (Kath's, say) takes two switches. The box turns its person's code on, and you, as the group's admin, allow it:

```sh
ssh -t pi4 'sudo yggstore hub site code Kath on'          # the box publishes it with code on
bin/yggstore site allow-code kath.withcare.co.za on       # you allow it, on the desktop
```

Both are already on for `kath.withcare.co.za`, so her apps only need a push.

Check it from anywhere:

```sh
curl "https://garden.withcare.co.za/api/signup?day=2026-11-07"
```

```
{"left":10}
```

| If you see | It means |
| --- | --- |
| `404` | code is off, or the site has no `api/signup.wasm` (push again) |
| `403` "waits for the group's admin" | the box has it on, `allow-code` hasn't been run |
| `409` with a reason | a rule in `schema.sql` turned it down (full, signed up already) |
| `504` | the handler took longer than 2 seconds |
| `429` | more than 120 requests a minute from one visitor |

To try a handler without data on the desktop first: `./devbox bash -c "cd garden.withcare.co.za && npm run api && cd tools/apirun && go run . ../../public/api/hello.wasm GET /api/hello"`. Handlers that use `db` only run on the site.

## Step 10: go back to an earlier version

Every push is kept as a version in the group, so a bad change is undone in one command. Data the site keeps (step 8) is not undone: sign-ups stay.

**A site this desktop publishes**, in `/media/peter/storage1/YGGSTORE`:

```sh
bin/yggstore site versions garden.withcare.co.za          # newest first; * is the one online
bin/yggstore site rollback garden.withcare.co.za          # the version before
bin/yggstore site rollback garden.withcare.co.za 3db703cd # or that one
```

**A site on a person's box:**

```sh
ssh -t pi4 'sudo yggstore hub site versions Kath'
ssh -t pi4 'sudo yggstore hub site back Kath'
```

Then make the folder in YGGSITES match, or the next push puts the bad change back: `git checkout -- garden.withcare.co.za` (or `git revert` the commit), then push again when it's right.

## Quick reference

| To | Run (in YGGSITES unless noted) |
| --- | --- |
| start a site | `mkdir SITE`, write `index.html`, add its line to `sites.conf` |
| see what would go up | `./sites push SITE --dry-run` |
| publish | `./sites push SITE` |
| publish as you save | `./devbox ./sites watch SITE` (Ctrl-C stops it) |
| look at a Markdown site | `./sites build SITE`, then open `.build/SITE/` |
| preview a project | `./devbox dev SITE`, then http://localhost:5173 |
| rebuild the Go in the browser | `./devbox bash -c "cd SITE && npm run wasm"` |
| turn code on (your site) | `bin/yggstore site code SITE on`, in YGGSTORE |
| turn code on (a box's site) | `ssh -t pi4 'sudo yggstore hub site code NAME on'`, then `bin/yggstore site allow-code SITE on` |
| a contact form to your Mail | `bin/yggstore site contact SITE on`, in YGGSTORE |
| undo a change (your site) | `bin/yggstore site rollback SITE`, in YGGSTORE |
| undo a change (a box's site) | `ssh -t pi4 'sudo yggstore hub site back NAME'` |
| a shell with Node, Go and TinyGo | `./devbox` |

More: the [CLI cheatsheet](cli-cheatsheet.md) and [Build a site app on yggstore](howto-site-app.md), which goes further into Go in the browser and stored data.
