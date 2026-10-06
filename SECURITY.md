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

## Who has to trust whom

- **Admin nodes are trusted completely.** An admin can push a new member
  list to every node, and so add a node of its own choosing (which then
  receives shards and can call every node). It can also change or remove
  any website on the group. Keep admin nodes few, and their keys safe.
- **Members** can't read your files without the stub, but they can see
  metadata: how many shards you store, their sizes, and when. They see
  every message published on a topic, and messages are kept unencrypted on
  the nodes that hold them (Yggdrasil encrypts them in transit).
- **A member can lose or withhold shards.** Erasure coding (any 4 of 6
  shards rebuild a chunk) and storage challenges limit the damage, but a
  group where most members collude can make a file unrecoverable.
- **Anyone holding a stub** can read that file while they can reach the
  group, and the stub's key doesn't change for later readers. Treat a stub
  like the file itself.
- **Gateway customers trust the gateway's operator.** The gateway encrypts
  their objects and keeps the keys (in `objects/`), so whoever runs it can
  read what customers store; the members' boxes only see ciphertext.
- **Websites** are public by design: web nodes keep plain copies of them.
- **Email** is only as private as email is. Cloudflare sees each incoming
  message before the mail Worker seals it for the recipient; after that the
  web nodes and members see ciphertext. Anyone with the Worker's token can
  put messages in a member's mailbox, so keep it as secret as a password.
- **Yggdrasil** is trusted for transport: node identities and the encryption
  between nodes come from it.
- **Everything on a member's machine is that member.** Nodes identify callers
  by their Yggdrasil address, which every program on the machine shares; on
  an admin's machine, any program can push member lists. Run nodes on
  machines you control, and keep untrusted software off admin machines.
- **The dashboard has no login.** It refuses web pages, but any user or
  program on the same machine can use it (and so restore, share and delete
  your files, and on an admin node make invites). Run it on single-user
  machines.
- **A shared item proves its sender only to its recipient.** The seal uses a
  key both of them can compute, so the recipient can't prove to anyone else
  who sent it, and someone who steals your sharing key can make items that
  appear to come from anyone, for you.

## Reporting a problem

Please report vulnerabilities privately through GitHub's
**Security → Report a vulnerability** on this repository rather than in a
public issue. Include what you found and how to reproduce it. You should get
a reply within two weeks.
