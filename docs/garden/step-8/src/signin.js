// Signing in with the group's own login (OpenID Connect, with PKCE): no
// passwords here, the visitor signs in at my. with what they use for their
// files. Only people in the group can. What a page learns is who they are;
// it's kept in this browser to show, not to protect anything, since every
// file of a static site is public.

// The group's login. A site under another domain can't use it.
export const ISSUER = "https://my.withcare.co.za";
const BACK = "/signin/"; // the page the login comes back to
const KEY = "yggsite-who";

const b64url = (bytes) => btoa(String.fromCharCode(...new Uint8Array(bytes))).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
const unb64url = (s) => Uint8Array.from(atob(s.replace(/-/g, "+").replace(/_/g, "/")), (c) => c.charCodeAt(0));
const random = () => b64url(crypto.getRandomValues(new Uint8Array(32)));

// accessToken is the signed-in visitor's token for the group's web nodes
// (uploads use it), or "".
export function accessToken() {
  const w = who();
  return w && w.token && w.tokenExp * 1000 > Date.now() ? w.token : "";
}

// who is the signed-in visitor ({name, email, box}), or null.
export function who() {
  try {
    const w = JSON.parse(localStorage.getItem(KEY));
    return w && w.exp * 1000 > Date.now() ? w : null;
  } catch {
    return null;
  }
}

// signIn goes to the group's login, and comes back to this page.
export async function signIn() {
  const verifier = random(), state = random(), nonce = random();
  const challenge = b64url(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier)));
  sessionStorage.setItem(KEY, JSON.stringify({ verifier, state, nonce, back: location.pathname + location.search }));
  const q = new URLSearchParams({
    client_id: location.host, redirect_uri: location.origin + BACK, response_type: "code", scope: "openid profile email",
    state, nonce, code_challenge: challenge, code_challenge_method: "S256",
  });
  location.href = `${ISSUER}/login/authorize?${q}`;
}

// signOut forgets the visitor here, and signs this browser out of the group's
// login too (everywhere it signed in from there).
export function signOut() {
  localStorage.removeItem(KEY);
  location.href = `${ISSUER}/login/signout?` + new URLSearchParams({ post_logout_redirect_uri: location.origin + "/" });
}

// finish runs on the BACK page: it swaps the code for an ID token, checks
// the token, and gives the page to go back to.
export async function finish() {
  const q = new URLSearchParams(location.search);
  const asked = JSON.parse(sessionStorage.getItem(KEY) || "null");
  sessionStorage.removeItem(KEY);
  if (q.get("error")) throw new Error(q.get("error") === "login_required" ? "You're not signed in to the group." : q.get("error_description") || q.get("error"));
  if (!asked || q.get("state") !== asked.state) throw new Error("This sign-in didn't start here: try again.");
  if (q.get("iss") && q.get("iss") !== ISSUER) throw new Error("That came back from somewhere else.");
  const res = await fetch(`${ISSUER}/login/token`, {
    method: "POST",
    body: new URLSearchParams({ grant_type: "authorization_code", code: q.get("code") || "", redirect_uri: location.origin + BACK,
      client_id: location.host, code_verifier: asked.verifier }),
  });
  const out = await res.json().catch(() => ({}));
  if (!res.ok || !out.id_token) throw new Error(out.error_description || `The login answered ${res.status}.`);
  const c = await checked(out.id_token);
  if (c.iss !== ISSUER || c.aud !== location.host || c.nonce !== asked.nonce || c.exp * 1000 <= Date.now()) throw new Error("The login's answer isn't for this site.");
  const tokenExp = Math.floor(Date.now() / 1000) + (out.expires_in || 3600) - 60;
  localStorage.setItem(KEY, JSON.stringify({ name: c.name || c.preferred_username, email: c.email || "", box: c.box || "", exp: c.exp,
    token: out.access_token || "", tokenExp }));
  return asked.back || "/";
}

// checked verifies an ID token's signature with the login's published keys
// (ES256) and gives its claims.
async function checked(token) {
  const [head, body, sig] = token.split(".");
  const { kid, alg } = JSON.parse(new TextDecoder().decode(unb64url(head)));
  if (alg !== "ES256") throw new Error("The login signed with " + alg + ".");
  const { keys } = await (await fetch(`${ISSUER}/login/keys`)).json();
  const jwk = keys.find((k) => k.kid === kid);
  if (!jwk) throw new Error("The login's key isn't one it publishes.");
  const key = await crypto.subtle.importKey("jwk", { kty: jwk.kty, crv: jwk.crv, x: jwk.x, y: jwk.y }, { name: "ECDSA", namedCurve: "P-256" }, false, ["verify"]);
  const ok = await crypto.subtle.verify({ name: "ECDSA", hash: "SHA-256" }, key, unb64url(sig), new TextEncoder().encode(head + "." + body));
  if (!ok) throw new Error("The login's answer isn't signed by it.");
  return JSON.parse(new TextDecoder().decode(unb64url(body)));
}
