import "./header.js";
import { openDb, rows } from "./db.js";

// The events page: the database's events, filtered as you type.
const list = document.getElementById("events");
const find = document.getElementById("find");
const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);
const day = (d) => new Date(d + "T12:00").toLocaleDateString(undefined, { weekday: "short", day: "numeric", month: "long" });

openDb()
  .then((db) => {
    const show = () => {
      const q = `%${find.value.trim()}%`;
      const found = rows(db, `SELECT day, title, place, notes FROM events
        WHERE title LIKE ? OR place LIKE ? OR notes LIKE ? ORDER BY day`, [q, q, q]);
      list.innerHTML = found.length
        ? found.map((e) => `<li><b>${esc(day(e.day))}</b> · ${esc(e.title)}${e.place ? ` <span class="muted">at ${esc(e.place)}</span>` : ""}
            ${e.notes ? `<br><span class="muted">${esc(e.notes)}</span>` : ""}</li>`).join("")
        : `<li class="muted">Nothing matches.</li>`;
    };
    find.addEventListener("input", show);
    show();
  })
  .catch((e) => (list.innerHTML = `<li class="muted">${esc(e.message)}</li>`));
