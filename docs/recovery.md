# If you lose your computer

Your files are stored across the group, but the stubs that open them are
on your computer, with your sharing key and your mail. Lose the computer
and, without a copy of those, the files can't be opened.

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
files. The dashboard's **If you lose this computer** section shows when
the last one was saved.

Recovery is **on by default**. Anyone who would rather not have a copy can
turn it off there.

## Working from somewhere else

On the dashboard, under **If you lose this computer**, give an email
address that isn't on this computer (Gmail, say) and choose the **box** you
work through when away. The dashboard keeps a record on the group's web
nodes and emails you a link, once: keep that email.

Open the link on any device, at a client's office or after losing your
computer. The page asks for the device's Syncthing ID, and then:

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
The box keeps making recovery copies while it has them, so you can move
on again. If the folder is open on another device, the page says so, and
**Move them here** takes it from that device first.

## How the link is kept safe

The key to the copy is in two halves. One is in the link, after the `#`,
which a browser never sends to a website; the group's web nodes hold the
other. Either half alone opens nothing.

Anyone who can read the email can open your files, as with the "forgot
password" emails other services send. If that worries you, turn recovery
off. More is planned: an optional code from an authenticator app, and an
admin who knows you resetting it if you lose your phone.
