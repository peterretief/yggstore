// Loads the TinyGo module once; resolves to window.yggsite.
let ready;
export function loadWasm() {
  ready ??= (async () => {
    const go = new window.Go(); // from /wasm/wasm_exec.js
    const started = new Promise((ok) => window.addEventListener("yggsite-ready", ok, { once: true }));
    const { instance } = await WebAssembly.instantiateStreaming(fetch("/wasm/app.wasm"), go.importObject);
    go.run(instance);
    await started;
    return window.yggsite;
  })();
  return ready;
}
