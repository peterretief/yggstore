// Pages written in Markdown, for Vite: pages/notes.md becomes /notes/,
// pages/courses/intro.md /courses/intro/, each put in pages/_layout.html.
//
// The layout is an ordinary page to Vite (it can load /src/main.js and the
// site's CSS), with {{title}} and {{content}} where the page's title and
// text go. A page may start with
//
//   ---
//   title: Notes
//   layout: _wide.html
//   ---
//
// to set its title (otherwise its first # heading) and use another layout
// in pages/. Links may name another page's .md file
// ([intro](courses/intro.md)); headings get ids to link to
// (## Start {#start}, or from their words). In `npm run dev` a change to a
// page reloads it; in a build each page is written beside the others.
import { marked } from "marked";
import { existsSync, mkdirSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join, posix, relative } from "node:path";

const dir = "pages";

// url is where a page in pages/ is: "notes.md" -> "/notes/", "index.md" -> "/".
const url = (rel) => {
  const p = rel.replace(/\.md$/, "");
  if (p === "index") return "/";
  return "/" + (p.endsWith("/index") ? p.slice(0, -"index".length) : p + "/");
};

function list(root) {
  const out = [];
  const walk = (d) => {
    if (!existsSync(d)) return;
    for (const e of readdirSync(d, { withFileTypes: true })) {
      const p = join(d, e.name);
      if (e.name.startsWith(".") || e.name.startsWith("_")) continue;
      if (e.isDirectory()) walk(p);
      else if (e.name.endsWith(".md")) out.push(relative(join(root, dir), p).split("\\").join("/"));
    }
  };
  walk(join(root, dir));
  return out;
}

const esc = (s) => s.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);

function render(root, rel) {
  let text = readFileSync(join(root, dir, rel), "utf8");
  const meta = {};
  const fm = /^---\r?\n([\s\S]*?)\r?\n---\r?\n/.exec(text);
  if (fm) {
    for (const line of fm[1].split(/\r?\n/)) {
      const kv = /^(\w+):\s*(.*)$/.exec(line);
      if (kv) meta[kv[1]] = kv[2].trim();
    }
    text = text.slice(fm[0].length);
  }
  const renderer = new marked.Renderer();
  const link = renderer.link.bind(renderer);
  renderer.link = (tok) => {
    const m = /^([^:?#]+)\.md(#.*)?$/.exec(tok.href);
    if (m) tok = { ...tok, href: url(posix.normalize(posix.join(posix.dirname(rel), m[1] + ".md"))) + (m[2] || "") };
    return link(tok);
  };
  renderer.heading = function (tok) {
    let t = tok.text, id;
    const m = /\s*\{#([\w-]+)\}\s*$/.exec(t);
    if (m) [t, id] = [t.slice(0, m.index), m[1]];
    else id = t.toLowerCase().replace(/<[^>]*>/g, "").replace(/[^\w\s-]/g, "").trim().replace(/\s+/g, "-");
    return `<h${tok.depth}${id ? ` id="${id}"` : ""}>${this.parser.parseInline(marked.Lexer.lexInline(t))}</h${tok.depth}>\n`;
  };
  return {
    title: meta.title || (/^#\s+(.+)$/m.exec(text) || [])[1] || rel.replace(/\.md$/, ""),
    layout: meta.layout || "_layout.html",
    html: marked.parse(text, { renderer }),
  };
}

const fill = (layout, page) => layout.replaceAll("{{title}}", esc(page.title)).replace("{{content}}", () => page.html);

export default function markdown() {
  let root = process.cwd(), outDir = "";
  return {
    name: "yggsite-markdown",
    // Every layout (pages/_*.html) is built as a page, so it gets the
    // site's assets; the Markdown pages are filled into it afterwards.
    config(cfg) {
      root = cfg.root || root;
      if (!existsSync(join(root, dir))) return;
      const layouts = readdirSync(join(root, dir)).filter((f) => f.startsWith("_") && f.endsWith(".html"));
      return { build: { rollupOptions: { input: Object.fromEntries(layouts.map((f) => [`${dir}/${f}`, `${dir}/${f}`])) } } };
    },
    configResolved(c) {
      root = c.root;
      outDir = c.build.outDir;
    },
    configureServer(server) {
      server.watcher.add(join(root, dir));
      server.watcher.on("change", (f) => {
        if (f.startsWith(join(root, dir)) && (f.endsWith(".md") || f.endsWith(".html"))) server.ws.send({ type: "full-reload" });
      });
      server.middlewares.use(async (req, res, next) => {
        const path = decodeURI((req.url || "/").split("?")[0]);
        const rel = list(root).find((r) => url(r) === path || url(r) === path + "/");
        if (!rel) return next();
        try {
          const page = render(root, rel);
          const html = await server.transformIndexHtml(path, fill(readFileSync(join(root, dir, page.layout), "utf8"), page), `/${dir}/${page.layout}`);
          res.setHeader("Content-Type", "text/html; charset=utf-8");
          res.end(html);
        } catch (e) {
          next(e);
        }
      });
    },
    closeBundle() {
      const out = join(root, outDir);
      const built = join(out, dir);
      if (!existsSync(built)) return;
      let n = 0;
      for (const rel of list(root)) {
        const page = render(root, rel);
        const layout = join(built, page.layout);
        if (!existsSync(layout)) throw new Error(`${dir}/${rel}: no layout ${dir}/${page.layout}`);
        const to = join(out, url(rel), "index.html");
        if (existsSync(to)) throw new Error(`${dir}/${rel} would replace ${relative(out, to)}, a page that's already there`);
        mkdirSync(dirname(to), { recursive: true });
        writeFileSync(to, fill(readFileSync(layout, "utf8"), page));
        n++;
      }
      rmSync(built, { recursive: true, force: true }); // the layouts aren't pages themselves
      console.log(`markdown: ${n} page${n === 1 ? "" : "s"} from ${dir}/`);
    },
  };
}

// The Markdown pages' addresses, for other tools (e.g. a sitemap).
export const markdownPages = (root = process.cwd()) => list(root).map(url);
