// Opens the site's database in the browser with sql.js (SQLite built to
// WebAssembly). It's a copy: changes stay in this page and are gone on reload.
import initSqlJs from "sql.js";
import wasmUrl from "sql.js/dist/sql-wasm.wasm?url";

let ready;
export function openDb() {
  ready ??= (async () => {
    const [SQL, res] = await Promise.all([initSqlJs({ locateFile: () => wasmUrl }), fetch("/data/site.db")]);
    if (!res.ok) throw new Error(`the database didn't load (${res.status})`);
    return new SQL.Database(new Uint8Array(await res.arrayBuffer()));
  })();
  return ready;
}

// rows runs a query and gives its rows as objects.
export function rows(db, sql, params = []) {
  const st = db.prepare(sql);
  try {
    st.bind(params);
    const out = [];
    while (st.step()) out.push(st.getAsObject());
    return out;
  } finally {
    st.free();
  }
}
