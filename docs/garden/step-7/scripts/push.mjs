// npm run push: builds the site and copies it into its folder on the box,
// which publishes it within a minute or so. Where it goes is in
// push.config.json (see push.config.example.json). Needs ssh and sudo on
// the box, so it's for the box's admin; everyone else uploads the built
// folder on their files page.
//
//   npm run push               add and replace files; keep the others there
//   npm run push -- --delete   also remove files the build no longer has
//   npm run push -- --dry-run  show what would change, change nothing
//   npm run push -- --yes      don't ask before replacing files on the site
//
// With "only" in push.config.json, such as ["register", "members"], only
// those pages (and assets/, which they need) go up: new pages beside a
// site someone already has, without touching the rest of it.
//
// Before replacing any file that is already on the site, it lists them and
// asks, so a push can't swap someone's home page for the starter's by
// mistake (as happened on 2026-10-22). If it does go wrong, the site's
// owner presses "Go back to the version before" on their website page.
import { execFileSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { createInterface } from "node:readline/promises";

const at = (p) => new URL(`../${p}`, import.meta.url).pathname;
if (!existsSync(at("push.config.json"))) {
  console.error("No push.config.json: copy push.config.example.json and fill it in.");
  process.exit(1);
}
const { site, host, dir, only } = JSON.parse(readFileSync(at("push.config.json"), "utf8"));
if (!site || !host || !dir) {
  console.error("push.config.json needs site, host and dir.");
  process.exit(1);
}
if (only !== undefined && (!Array.isArray(only) || only.some((p) => typeof p !== "string" || !/^[a-z0-9][a-z0-9_-]*$/i.test(p)))) {
  console.error('"only" is a list of page folders, such as ["register", "members"].');
  process.exit(1);
}
const args = process.argv.slice(2);
const known = ["--delete", "--dry-run", "--yes"];
const unknown = args.filter((a) => !known.includes(a));
if (unknown.length) {
  console.error(`Not an option: ${unknown.join(" ")}`);
  process.exit(1);
}
const dry = args.includes("--dry-run"), yes = args.includes("--yes");
const run = (cmd, a, env) => execFileSync(cmd, a, { stdio: "inherit", env: { ...process.env, ...env } });

run("npm", ["run", "build"], { SITE: site });
for (const p of only || []) {
  if (!existsSync(at(`out/${site}/${p}/index.html`))) {
    console.error(`The build has no ${p}/index.html: check "only" in push.config.json.`);
    process.exit(1);
  }
}
const filters = only ? [...["assets", ...only].map((p) => `--include=/${p}/***`), "--exclude=*"] : [];
const rsync = ["-rt", "--chmod=D755,F644", "--rsync-path=sudo rsync", ...filters, ...args.filter((a) => a === "--delete"),
  `out/${site}/`, `${host}:${dir.replace(/\/?$/, "/")}`];

// What would change: files already there that would be replaced, new
// ones, and (with --delete) ones removed.
const plan = execFileSync("rsync", ["--dry-run", "--itemize-changes", ...rsync], { encoding: "utf8" }).split("\n").filter(Boolean);
const replaced = plan.filter((l) => /^[<>]f/.test(l) && !/^[<>]f\+{9}/.test(l)).map((l) => l.slice(12));
const added = plan.filter((l) => /^[<>]f\+{9}/.test(l)).map((l) => l.slice(12));
const removed = plan.filter((l) => l.startsWith("*deleting")).map((l) => l.replace(/^\*deleting\s+/, ""));
console.log(`\n${site}: ${added.length} new, ${replaced.length} replaced, ${removed.length} removed${only ? ` (only ${["assets", ...only].join(", ")})` : ""}`);
for (const f of replaced) console.log(`  replaces ${f}${f === "index.html" ? "   <- the site's home page" : ""}`);
for (const f of removed) console.log(`  removes  ${f}`);
if (!added.length && !replaced.length && !removed.length) {
  console.log("Nothing to change.");
  process.exit(0);
}
if (dry) {
  for (const f of added) console.log(`  adds     ${f}`);
  console.log("\nDry run: nothing changed.");
  process.exit(0);
}
if ((replaced.length || removed.length) && !yes) {
  if (!process.stdin.isTTY) {
    console.error("\nThis replaces or removes files already on the site. Run it in a terminal to answer, or add --yes.");
    process.exit(1);
  }
  const rl = createInterface({ input: process.stdin, output: process.stdout });
  const home = replaced.includes("index.html") ? " That includes the site's home page." : "";
  const answer = await rl.question(`\nReplace or remove these files on ${site}?${home} [y/N] `);
  rl.close();
  if (!/^y(es)?$/i.test(answer.trim())) {
    console.log("Nothing changed.");
    process.exit(0);
  }
}
run("rsync", rsync);
console.log(`\nPushed: https://${site} shows it within a minute or so. To undo it, its owner presses "Go back to the version before" on their website page.`);
