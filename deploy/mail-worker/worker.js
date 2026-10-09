// yggstore's mail Worker (see docs/mail.md). Cloudflare Email Routing hands
// it each message; it seals the message for the recipient's sharing code
// and posts it to a web node through the group's tunnel. If no web node
// takes it, the Worker fails and the sending server tries again later.
//
// Settings (wrangler.jsonc vars and secrets):
//   MAILBOXES     JSON: {"you@example.org": {"node": "...", "code": "ys1..."}}
//                 ("*@example.org" catches the domain's other addresses);
//                 `yggstore mail address` prints an entry. An address not
//                 listed here is looked up in the names the group's admin
//                 has given (asked of a web node) before the catch-all.
//   INGEST_URL    https://NAME/_yggstore/mail, NAME routed to the web nodes
//   INGEST_TOKEN  secret; the web nodes' -mail-in token file holds the same

export default {
  async email(message, env) {
    const boxes = JSON.parse(env.MAILBOXES || "{}");
    const to = message.to.toLowerCase();
    const box = boxes[to] ?? (await lookup(env, to)) ?? boxes["*@" + to.split("@").pop()];
    if (!box) {
      message.setReject("No such mailbox here");
      return;
    }
    const raw = new Uint8Array(await new Response(message.raw).arrayBuffer());
    const clean = (s) => s.replace(/[\r\n<>]/g, "");
    const added = new TextEncoder().encode(
      `Delivered-To: ${clean(to)}\r\nReturn-Path: <${clean(message.from)}>\r\n`,
    );
    const sealed = await seal(box.code, concat(added, raw));
    const res = await fetch(`${env.INGEST_URL}?node=${encodeURIComponent(box.node)}`, {
      method: "POST",
      headers: { Authorization: `Bearer ${env.INGEST_TOKEN}`, "Content-Type": "application/octet-stream" },
      body: sealed,
    });
    if (!res.ok) {
      throw new Error(`web node answered ${res.status}: ${(await res.text()).slice(0, 200)}`);
    }
  },
};

// lookup asks a web node whose address this is, from the group's names:
// {node, code}, or undefined if it is no one's. A web node that can't be
// reached fails the message, so the sender retries.
async function lookup(env, to) {
  const url = new URL(env.INGEST_URL);
  url.pathname = "/_yggstore/mailbox";
  url.search = "?to=" + encodeURIComponent(to);
  const res = await fetch(url, { headers: { Authorization: `Bearer ${env.INGEST_TOKEN}` } });
  if (res.status === 404) return undefined;
  if (!res.ok) {
    throw new Error(`web node answered ${res.status} to the lookup: ${(await res.text()).slice(0, 200)}`);
  }
  return await res.json();
}

// seal encrypts a message for a sharing code, as yggstore's mail.Seal does:
// "ygm1", an ephemeral X25519 public key, a nonce, then AES-256-GCM with a
// key from HKDF-SHA256 (salt: both public keys, info: "yggstore mail v1").
export async function seal(code, raw) {
  if (!code.startsWith("ys1")) throw new Error("not a sharing code");
  const to = fromB64url(code.slice(3));
  const subtle = crypto.subtle;
  const eph = await subtle.generateKey({ name: "X25519" }, true, ["deriveBits"]);
  const toKey = await subtle.importKey("raw", to, { name: "X25519" }, false, []);
  const shared = await subtle.deriveBits({ name: "X25519", public: toKey }, eph.privateKey, 256);
  const ephPub = new Uint8Array(await subtle.exportKey("raw", eph.publicKey));
  const hkdf = await subtle.importKey("raw", shared, "HKDF", false, ["deriveKey"]);
  const key = await subtle.deriveKey(
    { name: "HKDF", hash: "SHA-256", salt: concat(ephPub, to), info: new TextEncoder().encode("yggstore mail v1") },
    hkdf, { name: "AES-GCM", length: 256 }, false, ["encrypt"],
  );
  const nonce = crypto.getRandomValues(new Uint8Array(12));
  const header = concat(new TextEncoder().encode("ygm1"), ephPub, nonce);
  const ct = await subtle.encrypt({ name: "AES-GCM", iv: nonce, additionalData: header }, key, raw);
  return concat(header, new Uint8Array(ct));
}

function concat(...parts) {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let i = 0;
  for (const p of parts) {
    out.set(p, i);
    i += p.length;
  }
  return out;
}

function fromB64url(s) {
  const bin = atob(s.replace(/-/g, "+").replace(/_/g, "/"));
  return Uint8Array.from(bin, (c) => c.charCodeAt(0));
}
