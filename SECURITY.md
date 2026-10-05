# Security

yggstore is experimental and **has not had an independent security review**.
It is not yet suitable as the only copy of important or sensitive data.

## What it relies on

- Files are encrypted with AES-256-GCM, one random key per file, before
  they leave your machine. Shards held by others are ciphertext.
- The key is in the stub (`.ystub`). Anyone with a stub who can reach the
  group can read that file; keep stubs private.
- Shared items (`.ysend`) are sealed with X25519 + HKDF-SHA256 + AES-GCM for
  one recipient's sharing code, and prove which sharing code sent them.
- Nodes only answer callers whose Yggdrasil address is in the group's member
  list; Yggdrasil addresses are derived from public keys.
- The dashboard listens on localhost only and refuses requests from other
  web pages.

## Reporting a problem

Please report vulnerabilities privately through GitHub's
**Security → Report a vulnerability** on this repository rather than in a
public issue. Include what you found and how to reproduce it. You should get
a reply within two weeks.
