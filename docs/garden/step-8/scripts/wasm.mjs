// Builds the TinyGo module in wasm/ into public/wasm/, with TinyGo's own
// wasm_exec.js beside it (it must come from the same TinyGo version).
import { execFileSync } from "node:child_process";
import { copyFileSync, mkdirSync } from "node:fs";
import { join } from "node:path";

const out = new URL("../public/wasm/", import.meta.url).pathname;
mkdirSync(out, { recursive: true });
// From wasm/, so its go.mod's toolchain line picks a Go that TinyGo takes.
const wasm = new URL("../wasm/", import.meta.url).pathname;
const run = (args, opts = {}) => execFileSync("tinygo", args, { encoding: "utf8", cwd: wasm, ...opts });
try {
  run(["build", "-o", join(out, "app.wasm"), "-target", "wasm", "-no-debug", "-opt", "z", "."], { stdio: "inherit" });
} catch (e) {
  console.error(e.code === "ENOENT" ? "TinyGo isn't installed: see https://tinygo.org/getting-started/install/" : "The TinyGo build failed.");
  process.exit(1);
}
copyFileSync(join(run(["env", "TINYGOROOT"]).trim(), "targets", "wasm_exec.js"), join(out, "wasm_exec.js"));
console.log("wasm: public/wasm/app.wasm");
