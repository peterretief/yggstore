// Builds the site's server code: each api/NAME/ (Go, package main) into
// public/api/NAME.wasm, for WASI, so it goes up with the site. The web nodes
// run it for /api/NAME once the site's code is on (yggstore site code SITE on).
import { execFileSync } from "node:child_process";
import { copyFileSync, existsSync, mkdirSync, readdirSync, readFileSync, rmSync } from "node:fs";
import { join } from "node:path";

const at = (p) => new URL(`../${p}`, import.meta.url).pathname;
const src = at("api"), out = at("public/api");
rmSync(out, { recursive: true, force: true });
if (!existsSync(join(src, "go.mod"))) {
  console.log("api: none");
  process.exit(0);
}
const names = readdirSync(src, { withFileTypes: true })
  .filter((e) => e.isDirectory() && /^[a-z0-9][a-z0-9-]*$/.test(e.name) && existsSync(join(src, e.name, "main.go")) && readFileSync(join(src, e.name, "main.go"), "utf8").includes("package main"))
  .map((e) => e.name);
mkdirSync(out, { recursive: true });
for (const name of names) {
  try {
    execFileSync("go", ["build", "-trimpath", "-ldflags=-s -w", "-o", join(out, `${name}.wasm`), `./${name}`], {
      cwd: src, stdio: "inherit", env: { ...process.env, GOOS: "wasip1", GOARCH: "wasm", CGO_ENABLED: "0" },
    });
  } catch (e) {
    console.error(e.code === "ENOENT" ? "Go isn't installed (it's in the dev container: ./devbox)." : `api/${name} didn't build.`);
    process.exit(1);
  }
}
// The data's SQL goes up with them; the web nodes read it from the site.
for (const f of ["schema.sql", "entries.sql"]) {
  if (existsSync(join(src, f))) copyFileSync(join(src, f), join(out, f));
}
console.log(`api: ${names.map((n) => `public/api/${n}.wasm`).join(", ") || "none"}`);
