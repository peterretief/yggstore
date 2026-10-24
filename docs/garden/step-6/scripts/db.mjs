// Makes public/data/site.db from db/schema.sql and db/seed.sql, with the
// SQLite built into Node (22.13 or later), so nothing native is installed.
import { DatabaseSync } from "node:sqlite";
import { mkdirSync, readFileSync, rmSync } from "node:fs";

const at = (p) => new URL(`../${p}`, import.meta.url).pathname;
const out = at("public/data/site.db");
mkdirSync(at("public/data"), { recursive: true });
rmSync(out, { force: true });
const db = new DatabaseSync(out);
for (const f of ["db/schema.sql", "db/seed.sql"]) {
  db.exec(readFileSync(at(f), "utf8"));
}
db.exec("VACUUM");
const tables = db.prepare("SELECT name FROM sqlite_schema WHERE type = 'table'").all().map((t) => t.name);
db.close();
console.log(`db: public/data/site.db (${tables.join(", ")})`);
