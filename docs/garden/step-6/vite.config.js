import { defineConfig } from "vite";
import markdown from "./scripts/markdown.mjs";

// Every page is its own folder with an index.html (about/index.html is
// /about/), because the web nodes serve files as they are: there is no
// fallback that sends every address to one page.
const pages = ["index.html"];

// SITE=name.withcare.co.za npm run build writes the site into a folder of
// that name, ready to upload on "Your websites". Without it, dist/.
const site = process.env.SITE;

export default defineConfig({
  base: "/",
  // No fallback to the home page for an unknown address, as on the web nodes.
  appType: "mpa",
  // The site's own builds (out/, from ./sites push or watch) aren't pages
  // to reload the preview for.
  server: { watch: { ignored: ["**/out/**", "**/dist/**"] } },
  // Pages written in Markdown, in pages/ (see scripts/markdown.mjs).
  plugins: [markdown()],
  build: {
    outDir: site ? `out/${site}` : "dist",
    emptyOutDir: true,
    rollupOptions: { input: Object.fromEntries(pages.map((p) => [p.replace(/\/?index\.html$|\.html$/, "") || "home", p])) },
  },
});
