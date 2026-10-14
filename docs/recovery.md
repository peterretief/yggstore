# If you lose your computer

Your files are stored across the group, but the stubs that open them are
on your computer, with your sharing key and your mail. Lose the computer
and, without a copy of those, the files can't be opened.

So the dashboard keeps a **recovery copy** in the group, stored like any
other item: encrypted, in pieces across the nodes. It holds:

- your outbox's stubs (`files/`, `whole/`, `received/`) and their history;
- from `~/.yggstore`: your sharing key, contacts, mail, websites, names,
  invites and devices;
- the group's list of nodes (`peers.json`).

It's kept small, at a few hundred KB for most people. Restored files,
files not stored yet, the stubs' challenges (which only spot-check the
nodes) and the node's own shards, tokens and passwords stay out. A new copy is made a couple of minutes after something
changes, the last 5 are kept, and repair looks after them like your
files. The dashboard's **If you lose this computer** section shows when
the last one was saved.

Recovery is **on by default**. Anyone who would rather not have a copy can
turn it off there.

## The recovery link (coming next)

You give the dashboard an email address that isn't on this computer, such
as Gmail. It sends you one email with a link: keep it. If the computer is
lost, click the link and your files open in a browser, where you can carry
on working or set up a new machine.

The key to the copy is in two halves. One is in the link, after the `#`,
which a browser never sends to a website; the group's web nodes hold the
other. The page needs both, so a web node can't open anyone's copy, and
the files are decrypted in your browser.

Anyone who can read that mailbox can open your files, as with the "forgot
password" emails other services send. If that worries you, turn recovery
off.
