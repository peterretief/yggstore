# Light clients: people on a box, through the web

Status: approved 2026-10-16. Steps 1 to 4 are built, and always on (6); writing mail (5) and the rest are not.

Most people on a box are **light clients**: they want files, a website and
email, and nothing to install. Everything they do goes through their box,
and they use it in a browser, at an ordinary `https://` address, with a
passkey instead of a password. They don't install Yggdrasil or Syncthing,
and they have no node, dashboard or wallet; the box's owner looks after
those.

Today someone on a box must install Yggdrasil on every device to open their
files. That becomes the advanced, direct route, for people who want it.

## What the person sees

1. **The box's owner adds them** under People on this box, as now. The
   invite link is an ordinary web address:
   `https://my.withcare.co.za/join?t=…`. It works once, for a week.
2. **They open it in any browser,** on their laptop or phone. The page
   asks them to name the device and **make a passkey**: the device's
   fingerprint, face or screen-lock PIN. There's no password to choose.
3. **Their files open,** on the same page box people have today: browse,
   upload, download, rename, delete, and their website and messages if
   they have a name.
4. **Next time** they go to `https://my.withcare.co.za` and press **Sign
   in**. The browser offers their passkey.
5. **Another device:** passkeys made on a phone or laptop often sync to
   the person's other devices (Apple, Google and Microsoft accounts do
   this). If not, they press **Add a device** on a device already signed
   in, which shows a one-time link to open on the new one, valid for 15
   minutes.
6. **Lost every device:** the box's owner sends a new invite. Later,
   perhaps a code by email to an address they already read.

## How it works

```mermaid
flowchart LR
  B["browser<br/>my.withcare.co.za"] -- https --> CF["Cloudflare"] -- tunnel --> W["web node<br/>(relay)"] -- "Yggdrasil" --> H["the person's box<br/>(hub, port 7490)"]
```

- **One address for the group:** `my.` and the group's domain, served by
  the web nodes through the same Cloudflare tunnel as the group's sites.
  It's its own address, not the person's website, so nothing on a website
  can reach a signed-in session.
- **The web node only relays.** It passes each request on to the person's
  box over Yggdrasil, and the answer back. It keeps nothing.
- **The box does the signing in.** It keeps each person's passkeys (only
  the public half, in its state file), checks each sign-in, and gives the
  browser a session cookie (`Secure`, `HttpOnly`, `SameSite=Lax`, so the
  link in an email still opens signed in; every action also needs a
  header other sites can't send), good for 30 days of use. The person sees their signed-in browsers under **Your
  devices**, beside their Yggdrasil devices, and can sign any of them out.
- **Which box:** the invite link names the box. The passkey carries the box
  and the person in its user handle, which the browser sends back when
  signing in, so the web node knows where to relay. After that a cookie
  says which box.
- **The box only takes relayed requests from the group's web nodes,**
  known by their Yggdrasil addresses, as it already does for the recovery
  link. Everything else still needs a device let in over Yggdrasil.
- **It's the same page** as over Yggdrasil: one set of code, so files,
  website, messages and (later) writing mail all work both ways.

## At their own address

Someone on a box who has a name in the group (see
[hub.md](hub.md#a-website-and-email-address-for-someone-on-the-box)) signs
in at their own address instead: `https://kath.withcare.co.za/files`.
Their invite link uses it too. Decided 2026-10-16.

- The web nodes send `/files` on that address to the person's box; the
  rest of the address is still their website.
- A passkey made there works only there, and only that person can make
  one or sign in there: someone else's invite or passkey is refused at it.
  A session started there counts only there.
- People without a name keep using `my.` and the domain.
- **The trade-off:** their website and their files share one address, so a
  page they put on their own site could act on their files while they're
  signed in. That's fine for pages they wrote; it's a reason to be careful
  with scripts copied from elsewhere.

## Without a screen lock: a key kept by the browser

A phone makes a passkey only if it has a screen lock, and even a password
manager may not get round that. For someone who doesn't want one, the
invite page has **No screen lock? Keep the key in this browser instead**
(added 2026-10-16):

- The page makes an ECDSA P-256 key with WebCrypto that can't be copied
  out of the browser, keeps it in the site's own storage (IndexedDB), and
  signs the box's challenge with it, together with the address
  (`passkey.BrowserMessage`). The box keeps the public half, marked as a
  browser key.
- **A PIN as well** (added the same day, after the first person to use it
  said it didn't feel safe): the person chooses a PIN of at least 6 digits
  or letters when making the key, and the sign-in page asks for it. The
  box checks it, not the browser, and keeps only a salted PBKDF2-SHA256
  hash, so taking the device apart doesn't let anyone try PINs unseen.
  The browser's signature is checked first, so only the device can try a
  PIN at all. 5 wrong PINs in a row lock the key (a new invite gives a new
  one). A key made before PINs is asked to choose one at its next sign-in.
- Sessions started with a browser key last an hour unused, not 30 days,
  so the PIN is asked again after time away.
- **What it costs:** it lives in that one browser, so clearing the
  browser's data, forgetting the PIN or changing device means a new invite
  from the box's owner.

## What the box's owner sees

On each person's row under People on this box:
- the invite now gives the web link, with the Yggdrasil one under
  "Advanced";
- their passkeys and signed-in browsers, and when each was last used;
- **Web access** on or off for that person (on by default);
- Suspend and Remove sign every session out at once.

## Privacy and safety

- **Passkeys can't be phished or guessed.** A passkey only works on
  `my.withcare.co.za`, and nothing secret is typed, so a fake site gets
  nothing.
- **What passes through:** over the web, the connection is encrypted from
  the browser to Cloudflare, from Cloudflare to the web node, and from the
  web node to the box, but Cloudflare and the web node see what passes
  between them: the files being opened or uploaded, and messages. Files are
  still stored encrypted in the group either way. The page says this, and
  anyone who wants the direct route can use Yggdrasil instead.
- **A web node can't sign in as someone:** it can't make a passkey's
  signature. But it carries session cookies, so the web nodes must be
  machines the group trusts, as they already are for the group's sites and
  mail.
- **Limits:** sign-in attempts and invite opens are rate-limited per
  address and per person; sessions end after 30 days unused, or at once
  when signed out, suspended or removed.

## Limits

- **Uploads go in pieces** of up to 32 MB, as the recovery link's page
  already does, because Cloudflare refuses a single request over 100 MB.
  Any size still works.
- **With the box off,** signing in works and files open from the latest
  recovery copy, with changes waiting for the box (see
  [Always on](#always-on)).
- **It's for people on a box.** Members with their own node keep their
  dashboard, over Yggdrasil.
- **At home too,** a light client uses `my.withcare.co.za`: the request
  goes out to Cloudflare and back to the box. Browsers only allow passkeys
  on `https://` pages, so the box's plain `http://` page on the LAN can't
  sign them in. Later the box could get a certificate of its own for a
  name like `localbox-003.withcare.co.za` pointing at its LAN address
  (Cloudflare's DNS makes that possible), so traffic at home stays at
  home.

## How it's built

- `internal/passkey`: checking passkeys with the standard library only.
  The browser gives the public key in the usual form
  (`getPublicKey()`), so no CBOR is needed; attestation isn't checked,
  since only an invite can make a passkey. Challenges carry their time, so
  a web node can make one a box will take, each good once for 5 minutes.
- `internal/light`: the web node's relay (`Relay`, serving `my.` and the
  domain for `site.Web`), and the sign-in and invite pages. A passkey's
  user handle is the box's name and the person's, which is how a web node
  routes a sign-in from a browser it hasn't seen.
- `internal/hub/light.go`: on the box, requests with `X-Yggstore-Relay`
  from one of the group's web nodes (by Yggdrasil address) are light
  clients': `/files/join` makes a passkey with an invite, `/files/light/…`
  signs in and out, and a session opens the same files page. Passkeys and
  sessions are kept in the hub's state file.
- Big uploads go in 32 MB parts (`X-Part`) when the page is on `https`.
- For withcare.co.za, the domain's wildcard record already sends
  `my.withcare.co.za` through the tunnel to the web nodes. A group without
  one needs a record and tunnel route for `my.` added, as for a site.

## Always on

The point of the group is that it's always there: no one machine being
off should stop anyone signing in or reaching their files. Today a light
client depends on their box for both. The files themselves already survive
a machine being off (they're in pieces across the group); what only the
box has is the sign-in details and the stubs that open the files. So:

1. **Sign-in moves to the web nodes** (built 2026-10-17). Every web node keeps each light
   client's passkeys, browser keys and PIN hashes (public halves and slow
   hashes only, so copying them is safe), checks sign-ins itself and keeps
   the sessions. Boxes send their people's records to every web node when
   they change, and the web nodes pass them among themselves, so a sign-in
   works while any one web node is up. Wrong PINs are counted across the
   web nodes. The web node then names the person to the box
   (`X-Yggstore-Person`), and the box takes that only over Yggdrasil from
   the group's web nodes, whose addresses can't be forged there.
   - **How:** the box's hub sends `PUT /v1/light/people` to every web
     node's node API when its state changes (and every 10 minutes); a web
     node takes a box's people only from that box's address
     (`light.Store`). Web nodes share sessions (`/v1/light/session`), asking
     the others for one they don't know, and wrong PINs
     (`/v1/light/fails`); they tell the box of each key's use, wrong PINs
     and a PIN chosen for a key made before PINs (`/files/light/seen`).
     Using an invite still goes to the box, which holds the invites; the
     web node that passed it on signs the person in with the new key.
   - **Limits:** a passkey's signature counter isn't checked, since the
     web nodes don't share one; a used challenge is remembered by the web
     node that took it, so within its 5 minutes it could be replayed once at
     the other (it's sent over https, so only someone who had broken that
     could). Choosing a PIN for a key made before PINs needs the box on.
   - **Trade-off:** today the box checks every passkey itself, so a web
     node that was broken into couldn't open anyone's files. After this it
     could pass as anyone. The web nodes already see all web traffic, so
     the group trusts them already, but it is a step.
2. **Every light client gets a recovery copy** (built 2026-10-17), on by
   default, with no Syncthing ID or email needed: once someone with web
   access has a key, the box makes them a sharing key, saves a first copy
   and keeps their record on the web nodes, as for "work from anywhere"
   (see [recovery](recovery.md)). It tells the web nodes their recovery ID
   with their keys. If it can't start (the group's messaging or nodes
   unreachable), it tries again every 10 minutes. No link is emailed: the
   key half for the browser comes in step 3.
3. **A box that's off is served from the recovery copy** (built
   2026-10-17). When the box doesn't answer, the web node sends the
   browser to the recovery link's page (`/_yggstore/work`, on `my.` or the
   person's own address) with the key half it keeps, and the page says
   "Your box is off right now… changes you make here are kept, and reach
   your box when it's back". It's the same page as the recovery link:
   browse and download from the group, upload, new folder, rename, move,
   delete. Changes wait on the web nodes and the box makes them in the
   person's folder within 20 seconds of being back.
   - **The copy's key** is in two halves, as for the recovery link: the web
     nodes hold one, and the person's browser keeps the other. While the box
     is on, opening `/files` gives the browser its half (`X-Yggstore-Copy`
     from the box, which the web node turns into the `ygg_copy` cookie:
     `Secure`, `HttpOnly`, so no page script can read it, even on the
     person's own website). Signing out removes it. So the web nodes alone
     still can't open the copy at rest, though they see the half as it
     passes, as they see the recovery link's.
   - **Finding the box off quickly:** a web node checks a box it hasn't
     heard from for a minute (4 seconds at most) before passing a request
     on, and once a box hasn't answered it doesn't wait for it again, but
     asks it every 10 seconds until it's back. While it's off, any page a
     browser opens under `/files` (a link on the page included) goes to
     the copy, and the files page reloads itself when the box stops
     answering, so it goes there too. The box gives the half on any page
     or listing, and starts a new key's copies at once.
   - **Limits:** a browser that has never opened its files while the box
     was on has no half, and gets "Your box isn't answering" (there's no
     recovery email for light clients yet). The copy is as the box last
     saved it, a couple of minutes after a change. Moving the folder to
     another device through Syncthing is hidden on that page, since it
     needs the box. The page doesn't ask for a sign-in itself: like the
     recovery link, holding the half is enough, but only a signed-in
     browser is sent there.

After these, losing any one machine (a box, a web node, a storage node)
stops nobody. Mail (step 5 of the build order) should follow the same rule.

## Build order

1. **The relay:** web nodes serve `my.` the group's domain and pass
   requests to a box over Yggdrasil. The box accepts relayed requests from
   web nodes only. The DNS record and tunnel route are added the same way
   as for sites.
2. **Passkeys and sessions on the box:** making a passkey from the invite,
   signing in, sessions, signing out, and listing them under Your devices.
3. **The web invite:** People on this box gives the web link, and the
   person's row shows their passkeys and sessions, with Web access on/off.
4. **Uploads in pieces** on the files page.
5. **Writing mail:** write and reply from their own address, on their
   page, through the group's mail relay. This works over Yggdrasil too.
6. **Always on** (above): sign-in on the web nodes, a recovery copy for
   every light client, and the web nodes serving it while the box is off.
7. **Later:** signing back in with a code by email.

## Decided (2026-10-16)

- **The address** is `my.` and the group's domain: `my.withcare.co.za`.
- **Web access is on by default** for new people; the box's owner can turn
  it off for someone.
- **Sessions** last 30 days of use.
- **Lost every device:** for now, a new invite from the box's owner. A code
  by email may come later, if people want it, knowing it makes their email
  account the key to their files.
