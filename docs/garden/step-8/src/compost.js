import { loadWasm } from "./wasm.js";

loadWasm().then((ygg) => {
  const form = document.getElementById("bed");
  const show = () => {
    const [length, width, depth] = ["length", "width", "depth"].map((n) => Number(form.elements[n].value) || 0);
    document.getElementById("bags").textContent = `${ygg.compost(length, width, depth)} bags of 40 litres`;
  };
  form.addEventListener("input", show);
  show();
});
