# Email on the group

Get email at your own domain without a mailbox provider. Each message is
encrypted for you as it arrives and stored across the group, so a lost disk
loses no mail.

```mermaid
flowchart LR
  S["sender's mail server"] --> ER["Cloudflare<br/>Email Routing"]
  ER --> WK["mail Worker<br/>seals it for you"]
  WK -- "tunnel" --> W1["web node"] & W2["web node"]
  W1 -- "stored in the group,<br/>notice to your node" --> Y["your node"]
  Y --> D["your dashboard"]
```

Incoming mail passes through Cloudflare Email Routing, which hands it to a
small Worker that seals it for your sharing code; from then on only you can
read it. Outgoing mail goes through an SMTP relay. Neither keeps it. Email
itself isn't private: Cloudflare sees each message before it's sealed, and
senders keep their copies. Treat it as ordinary email, stored well.

## Read and write mail

- **On your dashboard:** **Mail** lists your messages; open one to read
  it, download attachments or the whole message (`.eml`), **Reply**, or
  **Write** a new one.
- **In a terminal:** `yggstore mail list`, `yggstore mail read ID`, and

  ```sh
  echo "Hello" | yggstore mail send -to someone@example.net -subject "Hi"
  ```

Messages are plain text, up to 50 recipients, and 60 an hour. A copy of
each goes to **Sent**. A node that was off collects its mail when it's back.

Mail between members stays in the group. A message to an address the
group gives (a member's name, or someone's own domain on a box) is sealed
for the recipient by the web node and stored, as incoming mail is: it
never goes out to the internet and back, Cloudflare never sees it, and it
arrives when the internet is down as long as the machines reach each other.
Everyone else's copy goes out through the relay. If the recipient's node
can't be told just now, their copy goes out too, and comes back in through
Cloudflare as before.

### Internal addresses: mail that never leaves the group

Every name the group gives has an internal twin at the group's `.internal`
domain: `anna@example.org` is also `anna@example.internal`, the same
mailbox. The `.internal` domain is set aside for private networks, so
nothing on the internet can send to it, and the web nodes never send it
out:

- Mail to an internal address is delivered only in the group. If the
  recipient's node can't be told just now, the sender gets an error and
  tries again later; it doesn't go out through Cloudflare instead.
- Mail from an internal address may go only to the group's own addresses,
  ordinary or internal; anyone else is refused before anything is sent.
- On the dashboard, **Write** has a **From** choice between your address
  and your internal one, and a reply to a message that came to your
  internal address starts from it. In a terminal: `yggstore mail send
  -from anna@example.internal ...`.

### Local addresses: everyone on a box

Every person on a box has a local address from the moment they're on it,
name or no name: their name there, at the box, under the group's internal
domain, such as `kath@localbox-004.withcare.internal` ("Kath Smith" is
`kath-smith@...`). Like the internal addresses, it works only inside the
group: anyone in the group can write to it, and they can write to anyone in
the group, but nothing comes in from outside or goes out.

- The box tells the web nodes each person's sharing code with the rest of
  what it says about its people; mail to a local address is sealed for it
  and goes to the box, which files it in their mailbox as usual.
- Only the box sends as its own people's local addresses.
- Someone without a name in the group sees their local address on their
  mail page on the box, and writes from it; forwarding to an address
  outside isn't offered for it.
- Apps that sign people in with the group know such a person by it.

### New mail notices

On their mail page on the box, anyone on a box can press **Tell me about
new mail on this device**: the browser there (a phone, or a computer) then
gets its own notice, "New mail, from Kath", whenever a message reaches
their mailbox, Notes' emails included. The box sends it through the
browser's push service (Google's, Mozilla's or Apple's), encrypted for that
browser (RFC 8291), with its own key; the notice says who the mail is from,
never what it says. **Stop** on the same page, or a device the push service
says is gone, ends it. On an iPhone it works once the page is on the Home
Screen (Share, Add to Home Screen), which the page says.

An app's address has a twin too (`help@example.internal`), and mail to it
is from members by its nature.

## Use a mail program

The dashboard serves your mailbox to mail programs on the same machine:
IMAP on `127.0.0.1:2143` and SMTP on `127.0.0.1:2587`.

1. Run `yggstore mail bridge`. It prints the settings and the password.
2. In Thunderbird, **Add Mail Account** › **Configure manually**, and enter
   them. Connection security is **None**: Thunderbird warns, but the
   connection never leaves your machine.

You get Inbox, Sent, Drafts, Trash, Archives, Junk and your own folders,
with read and flagged marks. Emptying Trash deletes a message from the
group for good. Use `-imap` and `-smtp` on `yggstore dashboard` to change
the ports (`""` turns one off).

## Addresses for members

Every name the group gives is an address and a website: `anna@example.org`
and `anna.example.org`.

1. The member asks for a name on their dashboard, under **Your name** (or
   the box's admin, **W**, for someone on a box).
2. The admin's dashboard gives it at once if it's free, and puts a report
   in the admin's mailbox ("New website and email: ..."). A taken name is
   declined. Names kept for the group (`admin`, `www`, `postmaster` and
   the like) wait under **Requests** for the admin.
3. Mail to the address arrives in the member's mailbox, and they can send
   as it.

**Take back** on the admin's dashboard ends a name. To decide on every name
yourself, start the admin's dashboard with `-approve-names=false`.

## Mail at a client's own domain

Someone on a box with a [domain of their own](host-a-client.md) has one
mailbox for it: every address at the domain comes to them, and they can send
from any address at it. **Take its mail too** under **Names** on the admin's
dashboard sets it up, and changes nothing if the domain's mail already goes
somewhere else. It needs the relay's token to have Email Sending: Edit. The
domain's DMARC reports come to the admin's address.

## Set up mail for a domain

Once, for the group's admin. You need [web nodes](sites.md#setting-up-web-nodes)
and a domain on Cloudflare whose mail isn't used for anything else.

### Receiving

1. **Make a token for the Worker,** on one web node:
   `yggstore mail token ~/.yggstore/mail-in.token`. Copy the file to the
   other web nodes (same path, private), add
   `-mail-in ~/.yggstore/mail-in.token` to each one's `serve` command, and
   restart them.
2. **Give the Worker a way in:** in Cloudflare, add a published
   application route on the web nodes' tunnel, such as
   `mail-in.example.org` → `http://localhost:8480`.
3. **Give the group the domain:** add `"domain": "example.org"` to the
   admin's entry in `peers.json`.
4. **Deploy the Worker:**

   ```sh
   cd deploy/mail-worker
   cp wrangler.example.jsonc wrangler.jsonc   # set INGEST_URL to https://mail-in.example.org/_yggstore/mail
   yggstore mail token ~/.yggstore/mail-in.token | npx wrangler secret put INGEST_TOKEN
   npx wrangler deploy
   ```

   Addresses not in the file's `MAILBOXES` are looked up from the group's
   names. `wrangler.jsonc` holds real addresses, so it stays out of git.
5. **Send the domain's mail to it:** in Cloudflare, the domain's **Email** ›
   **Email Routing**, turn it on, and route the catch-all to the Worker
   `yggstore-mail`.

Send yourself a message: it shows on your dashboard within a minute.

### Sending

These steps use SMTP2GO; any relay with an SMTP login on port 587 or 465
works.

1. **Verify the domain at the relay:** **Sending** › **Verified Senders**,
   add the domain, and add the DNS records it lists in Cloudflare (DNS only).
   If the domain has no DMARC record, add one:
   `_dmarc TXT "v=DMARC1; p=none; rua=mailto:you@example.org"`.
2. **Give the web nodes a login:** add an SMTP user at the relay, then on one
   web node run `yggstore mail relay ~/.yggstore/mail-out.json` and fill in
   the file:

   ```json
   {
     "smtp": "mail.smtp2go.com:587",
     "user": "…",
     "password": "…",
     "senders": {"you@example.org": "200:…"}
   }
   ```

   `senders` gives addresses outside the group's names to a node
   (`yggstore mail address` prints a node's line). Copy the file to the
   other web nodes (mode 600), add `-mail-out ~/.yggstore/mail-out.json` to
   each one's `serve` command, and restart them.

## When something goes wrong

Every web node on the tunnel needs `-mail-in`, since the tunnel can bring
mail to any of them; one without it warns when it starts, and the admin's
dashboard shows **takes no mail** under **Incoming mail**. Refused addresses
are logged on the web node (`mail: refused`) and counted there.

| You see | Check |
| --- | --- |
| nothing arrives | `npx wrangler tail`: 403 means the tokens differ; 503 means the web node couldn't store it or reach your node (the sender retries) |
| "no web node that sends mail is online" | the web nodes' logs: none has a working `-mail-out` file |
| "this node may not send as …" | the address isn't one of your names, and no `senders` entry gives it to you; entries must match on every web node |
| "the mail relay: …" | the relay's answer, such as a wrong login or an unverified domain |
| "Can't open this message" | it was sealed for another sharing code: `MAILBOXES` must have the code `yggstore mail address` prints here |

A web node logs `mail: took ID for NODE` for each message, and your node
`mail: collected ID`.
