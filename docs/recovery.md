# Work from anywhere

Your files are stored across the group, but the stubs that open them are
on your computer, with your sharing key and your mail. To work on them
somewhere else (at a client's, on a laptop), or if you can't get to your
computer at all, those need to be somewhere you can reach. A link in your
email opens them on any device: in the browser, or synced to it.

So the dashboard keeps a **recovery copy** in the group, stored like any
other item: encrypted, in pieces across the nodes. It holds:

- your outbox's stubs (`files/`, `whole/`, `received/`) and their history;
- from `~/.yggstore`: your sharing key, contacts, websites, names, invites
  and devices;
- your mail as its stubs, folders and flags: each message is already
  stored in the group, and a recovered mailbox fetches the messages back
  from there;
- the group's list of nodes (`peers.json`).

It's kept small, at a few hundred KB for most people. Restored files,
files not stored yet, the stubs' challenges (which only spot-check the
nodes) and the node's own shards, tokens and passwords stay out. A new copy is made a couple of minutes after something
changes, the last 5 are kept, and repair looks after them like your
files. The dashboard's **Work from anywhere** section shows when
the last one was saved.

It is **on by default**. Anyone who would rather not have a copy can turn
it off there.

## Turning it on

On the dashboard, under **Work from anywhere**, give an email address that
isn't on this computer (Gmail, say) and choose the **box** you work through
when away. The dashboard keeps a record on the group's web nodes and emails
you a link, once: keep that email. **Send it again** sends a new one.

People who use a box have no dashboard of their own: they turn it on from
the box's page instead (see [the hub](hub.md#work-from-anywhere)).

The link opens a page on any of the group's websites
(`https://example.org/_yggstore/work#…`). It does two things.

## Your files in the browser

The page shows your files as they were in your latest recovery copy, with
nothing to install:

- **Browse** your folders, and **click a file to download it**. A folder
  you kept whole comes as a `.tar`. Files are fetched from the group as you
  download them, at any size.
- **Upload files here** (or drag them onto the page) puts files in the
  folder you're looking at, up to 100 MB each. Uploading over a file with
  the same name asks first; the one there is kept as an older version.
- **New folder**, and **⋯** on any file or folder to **rename** it,
  **move** it to another folder or **delete** it. Deleting asks first: it
  removes the file and its older versions from the group for good.

The page shows each change at once, marked *arriving…* or *moving…* until
it's made. The change itself is made by the place holding your files, your
computer or your box: every 20 seconds it collects what waits for it on the
web nodes, in the order you made it, and makes it in your folder. An
uploaded file becomes a stub there like any other. So changes are made
while that place is on; until then they wait, and the page says so.

What waits is sealed with your link's key, like your copy. The web node
stores an uploaded file in the group straight away, and deletes its own
copy once your computer has fetched it. Cloudflare can send each of the
page's requests to a different web node, so each one also asks the others
what waits, and the page looks the same whichever answers.

On your own laptop or phone you can do the same with no size limit and no
waiting, over Yggdrasil: see [your files on your devices](remote-dashboard.md#your-files).

## Working on another device

To work on your files as on your computer (open them in your programs,
change many at once), move them to the device. Under **Work on this device:
your folder through Syncthing**, the page asks for the device's Syncthing
ID, and then:

1. The box asks the place holding your files (your computer, say) to hand
   them over. It saves a final copy and stops changing them, so nothing
   you did there is lost. If it doesn't answer within a minute and a half,
   the box takes over from the latest copy.
2. The box unpacks the copy into a folder of yours and pairs your device
   with it, wherever the device is.
3. The page shows the box's ID to add in Syncthing. Accept the folder it
   offers, and you work as on a box: stubs in `files/`, copy one into
   `restore/` to get the file back.

Only one place changes your files at a time. Your computer shows **Your
files are being worked on at BOX/you** and stores, restores and deletes
nothing until they move back; its node carries on for the group as usual.
The box keeps making recovery copies while it has them, and collects what
you do on the link page, so you can move on again. If the folder is open
on another device, the page says so, and **Move them here** takes it from
that device first.

## How the link is kept safe

The key to the copy is in two halves. One is in the link, after the `#`,
which a browser never sends to a website; the group's web nodes hold the
other. Either half alone opens nothing.

Anyone who can read the email can open your files, as with the "forgot
password" emails other services send. If that worries you, turn **Work
from anywhere** off. More is planned: an optional code from an authenticator app, and an
admin who knows you resetting it if you lose your phone.

## Not done yet

- Moving your files **back** to your computer from a box: for now the
  computer takes them back only when the box hands them over.
- Your mail keeps arriving at your computer's node while you work
  elsewhere.
- Uploads through the link page are limited to 100 MB a file (Cloudflare's
  limit); sending them in pieces would lift it.
- The page's files are decrypted on the web node, not in your browser.
