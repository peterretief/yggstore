import "./header.js";
import { accessToken, signIn, who } from "./signin.js";

// The gallery: what people added (uploads/index.json, which the site's box
// keeps), and for the people the site's owner listed, a form to add more.
// The box takes photos (JPEG, PNG, WebP, GIF) and PDFs, up to 10 MB each.
const UPLOAD = "/_yggstore/upload";
const MAX = 10 << 20;
const TYPES = ["image/jpeg", "image/png", "image/webp", "image/gif", "application/pdf"];
const $ = (id) => document.getElementById(id);
const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);
const day = (t) => new Date(t * 1000).toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric" });

let shown = 0;
async function list() {
  const res = await fetch("/uploads/index.json", { cache: "no-store" }).catch(() => null);
  const ups = res && res.ok ? (await res.json()).uploads || [] : [];
  shown = ups.length;
  $("grid").innerHTML = ups.length
    ? ups.map((u) => `<figure><a href="/uploads/${encodeURIComponent(u.file)}" target="_blank" rel="noopener">
        ${u.preview ? `<img src="/uploads/${encodeURIComponent(u.preview)}" alt="${esc(u.caption || u.name)}" loading="lazy">`
          : `<span class="file">${u.type === "application/pdf" ? "PDF" : "File"}</span>`}</a>
        <figcaption>${u.caption ? esc(u.caption) + "<br>" : ""}<span class="muted">${esc(u.by)} · ${day(u.at)}</span></figcaption></figure>`).join("")
    : `<p class="muted">Nothing here yet.</p>`;
  return ups.length;
}

// preview draws a photo small, as a JPEG, so the box never has to.
async function preview(file) {
  if (!file.type.startsWith("image/")) return null;
  try {
    const img = await createImageBitmap(file, { imageOrientation: "from-image" });
    const k = Math.min(1, 360 / Math.max(img.width, img.height));
    const c = Object.assign(document.createElement("canvas"), { width: Math.round(img.width * k), height: Math.round(img.height * k) });
    c.getContext("2d").drawImage(img, 0, 0, c.width, c.height);
    return await new Promise((ok) => c.toBlob(ok, "image/jpeg", 0.8));
  } catch {
    return null; // a photo the browser can't draw: an icon instead
  }
}

async function upload(ev) {
  ev.preventDefault();
  const files = [...$("files").files];
  if (!files.length) return;
  $("send").disabled = true;
  const say = (t) => ($("upMsg").textContent = t);
  let sent = 0;
  for (const [i, f] of files.entries()) {
    const of = files.length > 1 ? ` (${i + 1} of ${files.length})` : "";
    if (!TYPES.includes(f.type)) { say(`${f.name} isn't a photo or a PDF: skipped.`); continue; }
    if (f.size > MAX) { say(`${f.name} is larger than 10 MB: skipped.`); continue; }
    say(`Sending ${f.name}${of}…`);
    const form = new FormData();
    form.append("caption", $("caption").value);
    form.append("file", f, f.name);
    const p = await preview(f);
    if (p) form.append("preview", p, "preview.jpg");
    const res = await fetch(UPLOAD, { method: "POST", headers: { Authorization: "Bearer " + accessToken() }, body: form });
    const out = await res.json().catch(() => ({}));
    if (!res.ok) { say(out.error || `That didn't work (${res.status}).`); $("send").disabled = false; return; }
    sent++;
  }
  $("send").disabled = false;
  $("files").value = $("caption").value = "";
  if (!sent) return;
  say(`Sent ${sent === 1 ? "it" : sent + " of them"}. ${sent === 1 ? "It appears" : "They appear"} here within a minute or two.`);
  // The box files it, then the site is published again: look now and then.
  const before = shown;
  for (let n = 0; n < 24 && shown <= before; n++) {
    await new Promise((ok) => setTimeout(ok, 10000));
    await list();
  }
}

async function form() {
  const w = who();
  if (!w) { $("addOut").hidden = false; return; }
  const res = await fetch(UPLOAD, { headers: { Authorization: "Bearer " + accessToken() }, cache: "no-store" }).catch(() => null);
  const st = res && res.ok ? await res.json() : {};
  if (!st.on) return; // the site takes no uploads
  if (!st.signed_in) { $("addAgain").hidden = false; return; } // the token ran out
  if (st.may) $("add").hidden = false;
  else $("addAsk").hidden = false;
}

$("upload").addEventListener("submit", upload);
document.querySelectorAll("[data-signin]").forEach((a) => a.addEventListener("click", (e) => { e.preventDefault(); signIn(); }));
list();
form();
