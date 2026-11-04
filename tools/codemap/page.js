const DATA = JSON.parse(document.getElementById("data").textContent);
const PKGS = DATA.pkgs;
const $ = id => document.getElementById(id);
const esc = s => String(s ?? "").replace(/[&<>"']/g, c => ({"&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"}[c]));
const num = n => n.toLocaleString("en");
$("module").textContent = DATA.module;

// Dark unless the viewer chose light.
const setTheme = t => { document.documentElement.dataset.theme = t; $("theme").textContent = t === "dark" ? "Light" : "Dark"; };
try { if (localStorage.getItem("codemap-theme") === "light") setTheme("light"); } catch {}
$("theme").addEventListener("click", () => {
  const t = document.documentElement.dataset.theme === "dark" ? "light" : "dark";
  setTheme(t);
  try { localStorage.setItem("codemap-theme", t); } catch {}
});

// Areas colour the packages. yggstore's are named by what they're for;
// any other module's are its top-level folders.
const PALETTE = ["store", "net", "web", "mail", "people", "app", "util"];
let AREAS = [
  ["store", "Storing pieces", ["erasure", "manifest", "files", "localstore", "lease", "challenge", "cryptofile", "repair", "server", "client"]],
  ["net", "Network and messages", ["yggnet", "mesh", "transport", "peers", "msg", "syncthing"]],
  ["web", "Websites and sign-in", ["site", "websites", "sitemove", "light", "passkey", "password", "gateway"]],
  ["mail", "Mail", ["mail", "mailbridge"]],
  ["people", "Boxes and people", ["hub", "workhere", "workcopy", "recovery", "devices", "contacts", "share", "invite", "outbox"]],
  ["app", "Dashboard and command", ["dashboard", "ygg", "cmd/yggstore"]],
  ["util", "Small helpers and tools", ["atomicfile", "filelock", "tools/codemap"]],
];
if (DATA.module !== "github.com/peterretief/yggstore") {
  const tops = {};
  for (const p of PKGS) (tops[p.dir.split("/")[0] || "module root"] ||= []).push(p.path);
  const groups = Object.entries(tops).sort((a, b) => b[1].length - a[1].length);
  const rest = groups.slice(PALETTE.length - 1).flatMap(g => g[1]);
  AREAS = groups.slice(0, PALETTE.length - (rest.length ? 1 : 0)).map(([top, list], i) => [PALETTE[i], top, list]);
  if (rest.length) AREAS.push([PALETTE[PALETTE.length - 1], "other folders", rest]);
}
const areaOf = {};
for (const [k, , list] of AREAS) for (const p of list) areaOf[p] = k;
const color = p => `var(--a-${areaOf[p] || "util"})`;

const byPath = Object.fromEntries(PKGS.map(p => [p.path, p]));
for (const p of PKGS) {
  p.nfn = p.files.reduce((a, f) => a + f.funcs.length, 0);
  p.lines = p.files.reduce((a, f) => a + f.lines, 0);
  p.usedBy = PKGS.filter(q => q.imports.includes(p.path)).map(q => q.path);
}
const total = {pk: PKGS.length, files: PKGS.reduce((a, p) => a + p.files.length, 0), fns: PKGS.reduce((a, p) => a + p.nfn, 0), lines: PKGS.reduce((a, p) => a + p.lines, 0)};
$("stats").innerHTML = [[total.pk, "packages"], [total.files, "files"], [total.fns, "functions and methods"], [total.lines, "lines of Go"]]
  .map(([n, l]) => `<div><b>${num(n)}</b> <span>${l}</span></div>`).join("");
$("legend").innerHTML = AREAS.map(([k, label]) => `<span><i style="background:var(--a-${k})"></i>${label}</span>`).join("");

// --- The map: a package's row is one above the highest row it builds on.
const layer = {};
const depth = p => {
  if (layer[p] !== undefined) return layer[p];
  layer[p] = 0; // guards a cycle, which Go doesn't allow anyway
  const d = Math.max(-1, ...byPath[p].imports.filter(i => byPath[i]).map(depth)) + 1;
  return (layer[p] = d);
};
PKGS.forEach(p => depth(p.path));
const rows = [];
for (const p of PKGS) (rows[layer[p.path]] ||= []).push(p.path);
const W = p => Math.max(64, 16 + p.length * 7.4) + Math.min(60, byPath[p].nfn / 4);
const ROWH = 62, PADX = 24, GAP = 10, TOP = 10;
const pos = {};
// Order each row by where the packages it uses sit, so lines cross less.
rows.forEach((row, i) => {
  if (i > 0) row.sort((a, b) => {
    const c = p => { const xs = byPath[p].imports.filter(x => pos[x]).map(x => pos[x].cx); return xs.length ? xs.reduce((s, x) => s + x, 0) / xs.length : 0; };
    return c(a) - c(b);
  });
  let x = PADX;
  for (const p of row) { const w = W(p); pos[p] = {x, w, cx: x + w / 2}; x += w + GAP; }
  row.width = x - GAP + PADX;
});
const width = Math.max(...rows.map(r => r.width), 600);
const height = TOP + rows.length * ROWH;
rows.forEach((row, i) => {
  const shift = (width - row.width) / 2;
  for (const p of row) { pos[p].x += shift; pos[p].cx += shift; pos[p].y = height - (i + 1) * ROWH + 14; }
});
const svg = $("map");
svg.setAttribute("viewBox", `0 0 ${width} ${height}`);
// A link already implied by others (A uses B, B uses C, so A reaches C) is
// drawn only when A or C is picked, which keeps the map readable.
const reachMemo = {};
const reach = p => reachMemo[p] ||= new Set(byPath[p].imports.filter(i => byPath[i]).flatMap(i => [i, ...reach(i)]));
const implied = (p, t) => byPath[p].imports.some(u => u !== t && byPath[u] && reach(u).has(t));
let edges = "", nodes = "";
for (const p of PKGS) for (const t of p.imports) {
  if (!pos[t]) continue;
  const a = pos[p.path], b = pos[t];
  const y1 = a.y + 30, y2 = b.y, my = (y1 + y2) / 2;
  edges += `<path class="edge${implied(p.path, t) ? " implied" : ""}" data-from="${esc(p.path)}" data-to="${esc(t)}" d="M${a.cx},${y1} C${a.cx},${my} ${b.cx},${my} ${b.cx},${y2}"/>`;
}
for (const p of PKGS) {
  const q = pos[p.path];
  nodes += `<g class="node" tabindex="0" role="button" data-p="${esc(p.path)}" aria-label="${esc(p.path)}, ${p.nfn} functions">
    <rect x="${q.x}" y="${q.y}" width="${q.w}" height="30" rx="7" style="stroke:${color(p.path)}"/>
    <rect x="${q.x}" y="${q.y}" width="5" height="30" rx="2" style="fill:${color(p.path)};stroke:none"/>
    <text x="${q.cx + 2}" y="${q.y + 19}" text-anchor="middle">${esc(p.path.replace("cmd/", ""))}</text>
    <title>${esc(p.path)}: ${p.nfn} functions, ${num(p.lines)} lines${p.doc ? "\n" + p.doc : ""}</title></g>`;
}
svg.innerHTML = `<g>${edges}</g><g>${nodes}</g>`;

const HINT = $("hint").innerHTML;
function highlight(p) {
  const near = new Set(p ? [p, ...byPath[p].imports, ...byPath[p].usedBy] : []);
  svg.querySelectorAll(".node").forEach(n => {
    n.classList.toggle("on", n.dataset.p === p);
    n.classList.toggle("dim", !!p && !near.has(n.dataset.p));
  });
  svg.querySelectorAll(".edge").forEach(e => {
    e.classList.toggle("down", e.dataset.from === p);
    e.classList.toggle("up", e.dataset.to === p);
  });
  if (!p) $("hint").innerHTML = HINT;
  else {
    const k = byPath[p];
    $("hint").innerHTML = `<b>${esc(p)}</b> <em class="d">uses ${k.imports.length}</em> and <em class="u">is used by ${k.usedBy.length}</em> other packages.`;
  }
}
svg.addEventListener("click", ev => {
  const n = ev.target.closest(".node");
  if (n) select(n.dataset.p, true); else highlight("");
});
svg.addEventListener("keydown", ev => { const n = ev.target.closest(".node"); if (n && (ev.key === "Enter" || ev.key === " ")) { ev.preventDefault(); select(n.dataset.p, true); } });

// --- The browser.
// Open on the package at the top of the map: the module's main command.
let curPkg = rows[rows.length - 1][0], curFile = "", query = "", exportedOnly = false;
const sig = (f, p) => `<span class="r">${f.r ? "(" + esc(f.r) + ") " : ""}</span><span class="${f.e ? "x" : ""}">${esc(f.n)}</span>`;
const maxLen = Math.max(...PKGS.flatMap(p => p.files.flatMap(f => f.funcs.map(x => x.s))));
const fnRow = (f, p, file, showWhere) => `<div class="fn">
  <div class="sig">${sig(f, p)}${showWhere ? ` <span class="r">· ${esc(p)}/${esc(file)}</span>` : ""}</div>
  <div class="meta">${f.s} line${f.s === 1 ? "" : "s"}${DATA.source ? ` · <a href="${esc(DATA.source + (byPath[p].dir ? byPath[p].dir + "/" : "") + file)}#L${f.l}" target="_blank" rel="noopener">source</a>` : ""}</div>
  ${f.d ? `<div class="doc">${esc(f.d)}</div>` : ""}
  <div class="bar" style="width:${Math.max(2, 100 * f.s / Math.min(maxLen, 200))}%;max-width:100%"></div></div>`;

function renderPkgs() {
  const list = [...PKGS].sort((a, b) => b.nfn - a.nfn);
  $("pkgs").innerHTML = `<div class="colhead">Packages, largest first</div>` + list.map(p =>
    `<button class="item${p.path === curPkg ? " on" : ""}" data-pkg="${esc(p.path)}"><span class="dot" style="background:${color(p.path)}"></span>
      <span class="n">${esc(p.path)}</span><span class="c">${p.nfn}</span></button>`).join("");
}
function renderFiles() {
  const p = byPath[curPkg];
  const uses = list => list.length ? list.map(x => `<a data-go="${esc(x)}">${esc(x)}</a>`).join(", ") : "nothing else in yggstore";
  $("files").innerHTML = `<div class="colhead">${esc(curPkg)}</div>
    <div class="pdoc"><b>${esc(curPkg)}</b>${esc(p.doc || "")}
      <div class="uses">Uses: ${uses(p.imports)}</div><div class="uses">Used by: ${uses(p.usedBy)}</div></div>
    <button class="item${curFile === "" ? " on" : ""}" data-file=""><span class="n">all ${p.files.length} files</span><span class="c">${p.nfn}</span></button>` +
    p.files.map(f => `<button class="item${f.name === curFile ? " on" : ""}" data-file="${esc(f.name)}"><span class="n">${esc(f.name)}</span>
      <span class="c">${f.funcs.length}</span></button>`).join("");
}
function renderFns() {
  let out = "", n = 0;
  const ok = f => (!exportedOnly || f.e) && (!query || (f.n + " " + (f.r || "") + " " + (f.d || "")).toLowerCase().includes(query));
  if (query) {
    for (const p of PKGS) for (const file of p.files) for (const f of file.funcs) if (ok(f) && n < 300) { out += fnRow(f, p.path, file.name, true); n++; }
    $("fns").innerHTML = `<div class="colhead">Matching “${esc(query)}” in every package${n >= 300 ? ": the first 300" : ""}</div>` + (out || `<div class="empty">No function matches. Try part of a name, such as “invite”.</div>`);
    return;
  }
  const p = byPath[curPkg];
  for (const file of p.files) {
    if (curFile && file.name !== curFile) continue;
    const fs = file.funcs.filter(ok);
    if (!fs.length) continue;
    if (!curFile) out += `<div class="colhead" style="position:static">${esc(file.name)} · ${num(file.lines)} lines</div>`;
    out += fs.map(f => fnRow(f, curPkg, file.name, false)).join("");
    n += fs.length;
  }
  $("fns").innerHTML = (curFile ? `<div class="colhead">${esc(curFile)} · ${n} functions</div>` : "") + (out || `<div class="empty">No exported functions here.</div>`);
}
function select(p, scroll, quiet) {
  curPkg = p; curFile = "";
  renderPkgs(); renderFiles(); renderFns();
  if (!quiet) highlight(p);
  if (scroll) $("files").scrollTop = 0;
}
$("pkgs").addEventListener("click", ev => { const b = ev.target.closest("[data-pkg]"); if (b) select(b.dataset.pkg); });
$("files").addEventListener("click", ev => {
  const g = ev.target.closest("[data-go]"); if (g) { select(g.dataset.go, true); return; }
  const b = ev.target.closest("[data-file]"); if (b) { curFile = b.dataset.file; renderFiles(); renderFns(); }
});
$("q").addEventListener("input", ev => { query = ev.target.value.trim().toLowerCase(); renderFns(); });
$("exported").addEventListener("change", ev => { exportedOnly = ev.target.checked; renderFns(); });
$("foot").textContent = `From ${DATA.module}${DATA.rev ? " at commit " + DATA.rev : ""}. Bold names are exported${DATA.source ? "; “source” opens the line on GitHub" : ""}. Made with codemap (github.com/peterretief/yggstore/tools/codemap).`;
select(curPkg, false, true); // the map opens whole, with nothing picked
