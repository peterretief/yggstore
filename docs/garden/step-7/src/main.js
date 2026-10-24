import "./header.js";
import { loadWasm } from "./wasm.js";

// The home page's demo: the text box's statistics, worked out by the
// WebAssembly module as you type.
const box = document.getElementById("demo-text");
if (box) {
  const out = document.getElementById("demo-out");
  loadWasm()
    .then((ygg) => {
      const show = () => {
        const s = ygg.stats(box.value);
        out.textContent = `${s.words} words, ${s.sentences} sentences, about ${s.minutes} min to read · address: /${ygg.slug(box.value.split("\n")[0]) || "…"}/`;
      };
      box.addEventListener("input", show);
      show();
    })
    .catch((e) => (out.textContent = "The WebAssembly module didn't load: " + e.message));
}
