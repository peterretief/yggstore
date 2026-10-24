# Writes docs/garden/step-N: the garden site as it stands after each step of
# docs/howto-website.md, its code blocks taken from the how-to itself where
# they're whole files. Run it after changing the how-to:
#
#   python3 scripts/garden-steps.py [YGGSITE-STARTER folder]
#
# It needs Pillow (for the sample picture) and gofmt.
import os, re, shutil, subprocess, sys
root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
starter = sys.argv[1] if len(sys.argv) > 1 else os.path.join(os.path.dirname(root), "YGGSITE-STARTER")
out = os.path.join(root, "docs/garden")
md = open(os.path.join(root, "docs/howto-website.md")).read()
shutil.rmtree(out, ignore_errors=True)

def write(step, path, text):
    p = os.path.join(out, step, path)
    os.makedirs(os.path.dirname(p), exist_ok=True)
    open(p, "w").write(text.lstrip("\n"))

def block(after, lang):
    i = md.index(after)
    m = re.compile(r"```" + lang + r"\n(.*?)```", re.S).search(md, i)
    return m.group(1)

# Step 1 (and 2): one page.
write("step-1", "index.html", """
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
""")

# Step 3: Markdown pages in one layout.
layout = """
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{title}} · The garden</title>
  <link rel="stylesheet" href="/style.css">
</head>
<body>
  <nav>%s</nav>
  <main>
{{content}}
  </main>
</body>
</html>
"""
nav3 = '<a href="/">Home</a> <a href="/visit/">Visit</a>'
visit = """
---
title: Visit
---
# Visit

## Where {#where}

12 Oak Street, behind the hall.

## What to bring

- Gloves
- Water
"""
index_md = """
---
title: Home
---
# The garden

Open on Saturdays, 9 to 12. [How to find us](visit.md).
"""
write("step-3", "_layout.html", layout % nav3)
write("step-3", "index.md", index_md)
write("step-3", "visit.md", visit)

# Step 4 (and 5): a style and a picture.
style = """
body { font: 18px/1.6 system-ui, sans-serif; max-width: 40rem; margin: 0 auto; padding: 1rem; color: #222; background: #fdfcf8; }
nav a { margin-right: 1rem; color: #2f6b3a; }
img { max-width: 100%; height: auto; border-radius: 6px; }
@media (prefers-color-scheme: dark) { body { color: #eee; background: #1b1d1a; } nav a { color: #8fd19e; } }
"""
for f, t in [("_layout.html", layout % nav3), ("index.md", index_md.rstrip() + "\n\n![The vegetable beds in spring](/img/beds.jpg)\n"), ("visit.md", visit), ("style.css", style)]:
    write("step-4", f, t)
from PIL import Image, ImageDraw
img = Image.new("RGB", (1200, 800), (122, 92, 60))
d = ImageDraw.Draw(img)
for i in range(4):
    x = 80 + i * 270
    d.rounded_rectangle([x, 120, x + 210, 680], 18, fill=(70, 130, 60))
    for y in range(160, 660, 60):
        d.ellipse([x + 70, y, x + 140, y + 40], fill=(120, 180, 90))
os.makedirs(os.path.join(out, "step-4/img"))
img.save(os.path.join(out, "step-4/img/beds.jpg"), quality=80)

# Step 6: a project from the starter, the demo pages taken out.
s6 = os.path.join(out, "step-6")
os.makedirs(s6)
subprocess.run(f"git -C {starter} archive HEAD | tar -x -C {s6}", shell=True, check=True)
for p in ["index.html", "404.html", "about", "contact", "events", "gallery", "register", "signin", "thanks",
          "pages/news.md", "push.config.example.json", "api/register", "api/schema.sql", "api/entries.sql"]:
    q = os.path.join(s6, p)
    shutil.rmtree(q) if os.path.isdir(q) else os.remove(q)
write("step-6", "pages/_layout.html", layout % nav3)
write("step-6", "pages/visit.md", visit)
write("step-6", "style.css", style)
os.makedirs(os.path.join(s6, "public/img"))
shutil.copy(os.path.join(out, "step-4/img/beds.jpg"), os.path.join(s6, "public/img/beds.jpg"))
home = """
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>The garden</title>
  <link rel="stylesheet" href="/style.css">
  <script type="module" src="/src/days.js"></script>
</head>
<body>
  <nav>%s</nav>
  <main>
    <h1>The garden</h1>
    <p>Open on:</p>
    <ul id="days"></ul>
    <p><a href="/visit/">How to find us</a>.</p>
    <img src="/img/beds.jpg" alt="The vegetable beds in spring" width="1200" height="800">
  </main>
</body>
</html>
"""
write("step-6", "index.html", home % nav3)
write("step-6", "db/schema.sql", "CREATE TABLE days (day TEXT PRIMARY KEY, open TEXT NOT NULL);\n")
write("step-6", "db/seed.sql", "INSERT INTO days VALUES ('Saturday', '9 to 12'), ('Wednesday', '16 to 18');\n")
days = block("Then write `src/days.js`", "js")
write("step-6", "src/days.js", "\n".join(l[3:] if l.startswith("   ") else l for l in days.split("\n")).rstrip() + "\n")
vite = open(os.path.join(s6, "vite.config.js")).read()
vite6 = re.sub(r'const pages = \[.*?\];', 'const pages = ["index.html"];', vite)
write("step-6", "vite.config.js", vite6)

# Step 7: Go in the browser.
s7 = os.path.join(out, "step-7")
shutil.copytree(s6, s7)
nav7 = nav3 + ' <a href="/compost/">Compost</a>'
write("step-7", "pages/_layout.html", layout % nav7)
write("step-7", "index.html", home % nav7)
go = open(os.path.join(s7, "wasm/main.go")).read()
go = go.replace('import (\n', 'import (\n\t"math"\n', 1)
fn = block("Add the function to `wasm/main.go`", "go")
fn = "\n".join(l[3:] if l.startswith("   ") else l for l in fn.split("\n")).rstrip()
go = go.replace("func main() {", fn + "\n\nfunc main() {")
go = go.replace('"slug":  js.FuncOf(slug),', '"slug":  js.FuncOf(slug),\n\t\t"compost": js.FuncOf(compost),')
write("step-7", "wasm/main.go", go)
subprocess.run(["gofmt", "-w", os.path.join(s7, "wasm/main.go")], check=True)
compost_js = block("The page's script, `src/compost.js`", "js")
write("step-7", "src/compost.js", "\n".join(l[3:] if l.startswith("   ") else l for l in compost_js.split("\n")).rstrip() + "\n")
compost_html = block("A page for it, `compost/index.html`", "html")
write("step-7", "compost/index.html", "\n".join(l[3:] if l.startswith("   ") else l for l in compost_html.split("\n")).rstrip() + "\n")
write("step-7", "vite.config.js", vite6.replace('const pages = ["index.html"];', 'const pages = ["index.html", "compost/index.html"];'))

# Step 8 (and 9, 10): a form whose answers are kept.
s8 = os.path.join(out, "step-8")
shutil.copytree(s7, s8)
nav8 = nav7 + ' <a href="/signup/">Work days</a>'
write("step-8", "pages/_layout.html", layout % nav8)
write("step-8", "index.html", home % nav8)
c = open(os.path.join(s8, "compost/index.html")).read()
assert nav7 in c
write("step-8", "compost/index.html", c.replace(nav7, nav8))
write("step-8", "api/schema.sql", block("**1. The tables and their rules**", "sql"))
write("step-8", "api/entries.sql", block("**2. What a sign-up does**", "sql"))
write("step-8", "api/signup/main.go", block("**3. The handler**", "go"))
form = block("**4. The page**", "html")
write("step-8", "signup/index.html", """
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Work days · The garden</title>
  <link rel="stylesheet" href="/style.css">
</head>
<body>
  <nav>%s</nav>
  <main>
%s  </main>
</body>
</html>
""" % (nav8, "".join("    " + l + "\n" if l else "\n" for l in form.rstrip("\n").split("\n"))))
write("step-8", "vite.config.js", vite6.replace('const pages = ["index.html"];', 'const pages = ["index.html", "compost/index.html", "signup/index.html"];'))
write("", "README.md", """
# The garden, step by step

The site built in [Make a website on yggstore, step by step](../howto-website.md),
as it stands after each step. The steps not here don't change the folder.

| Folder | After step | What it has |
| --- | --- | --- |
| [step-1](step-1/) | 1–2 | one page, `index.html` |
| [step-3](step-3/) | 3 | Markdown pages in one layout |
| [step-4](step-4/) | 4–5 | a style and a picture |
| [step-6](step-6/) | 6 | a project: live preview, the open days from its database |
| [step-7](step-7/) | 7 | Go in the browser: the compost calculator |
| [step-8](step-8/) | 8–10 | the work-day sign-up form, kept on the web nodes |

To start again from one, in `/media/peter/storage1/YGGSITES`:

```sh
rm -r garden.withcare.co.za && cp -r ../YGGSTORE/docs/garden/step-6 garden.withcare.co.za
```

These folders are made from the how-to by `scripts/garden-steps.py`, so
they say what it says: change the how-to, then run the script again.
""")
print("ok")
