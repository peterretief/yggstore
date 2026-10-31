# Work from anywhere

Open your files from any device if you're away from your computer, or if
it's lost: a link in your email opens them in a browser, or moves them to
another device.

Your files are in the group, but the stubs that open them, your sharing key
and your mail are on your computer. So your dashboard keeps a **recovery
copy** of them in the group, encrypted like any file:

- your outbox's stubs (`files/`, `whole/`, `received/`) and their history;
- your sharing key, contacts, websites, names, invites and devices;
- your mail's stubs, folders and flags;
- the group's list of nodes.

It's a few hundred KB, made a couple of minutes after something changes;
the last 5 are kept. It's **on by default**; turn it off under **Work from
anywhere** on the dashboard.

## Turn it on

On the dashboard, under **Work from anywhere**, give an email address that
isn't on this computer and choose the box you'll work through. You get a
link by email, once: keep it. **Send it again** sends a new one.

People on a box turn it on from the box's page
([the hub](hub.md#work-from-anywhere)).

## Your files in the browser

The link opens a page on any of the group's websites, with nothing to
install:

- **Browse**, and click a file to download it, at any size.
- **Upload** files or folders, or drag them onto the page. Uploading over a
  file asks first, and keeps the old one as an older version.
- **New folder**, and **⋯** to rename, move or delete. Deleting removes the
  file and its older versions for good.

The page shows each change at once (*arriving…*). Your computer or box makes
it in your folder within 20 seconds while it's on; until then the change
waits, sealed, and the page says so.

On your own laptop or phone, [your files over Yggdrasil](remote-dashboard.md#your-files)
do the same with no waiting.

## Work on another device

To work on the files in your own programs, move them to the device. Under
**Work on this device**, give the device's Syncthing ID:

1. The box asks your computer to hand the files over. It saves a final copy
   and stops changing them. If it doesn't answer within a minute and a half,
   the box takes over from the latest copy.
2. The box unpacks the copy into a folder of yours and pairs your device
   with it, wherever the device is.
3. Add the box's ID in Syncthing and accept the folder.

Only one place changes your files at a time: your computer shows **Your
files are being worked on at BOX/you** until they come back. **Move them
here** on the page takes them from whichever device has them.

## How the link is kept safe

The key to your copy is in two halves: one in the link, after the `#`,
which a browser never sends to a website, and one on the group's web nodes.
Either alone opens nothing. Anyone who can read the email can open your
files, as with any "forgot password" email; if that worries you, turn it
off.

## Not done yet

- Moving your files back to your computer from a box, unless the box hands
  them over.
- Mail keeps arriving at your computer while you work elsewhere.
- An interrupted large upload starts again.
- The page's files are decrypted on the web node, not in your browser.
