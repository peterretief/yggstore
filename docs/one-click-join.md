# Joining with one click

Status: proposed 2026-10-25. Step 1 (the Windows installer) built the same day, for computers on the box's network; not yet tried on a real Windows PC.

The aim: a person clicks one link, and their computer has a folder kept in
step with the group, with their files, their websites and their mail in it.
Nothing to type, nothing to set up in Syncthing.

Decided on 2026-10-25: **the synced folder is the way people use the
group.** The browser page stays for phones and for a quick look.

## What a person ends up with

```
yggstore\
  files\      their files: stored in the group, encrypted (as today)
  websites\   one folder per site: save a page, it's published (as today)
  mail\       their mail, one file per message (new)
```

Their computer needs only Syncthing, which the installer sets up. It doesn't
need Yggdrasil, a member's node or admin rights: the box does the rest.

## What exists already

- **The box's page** (`NAME.local:7490`) and the person's own page
  (`name.withcare.co.za/files`).
- **connect.sh / connect.ps1**: they install Syncthing, start it at sign-in,
  add the box and accept its folders into `~/yggstore`. They work on Linux
  and Mac, and with Syncthing 1.29 and 2.x. The PowerShell one failed for
  Tim (2026-10-20): a typed command isn't a way in for these users.
- **Pairing from anywhere**: the person's own page can pair a device that
  isn't on the box's network.

## What gets built

### 1. A Windows installer

`Join withcare.exe`, downloaded from the box's page or the person's own
page. Double-click it, and it:

1. puts Syncthing in the person's own programs folder and starts it at
   sign-in (no admin rights, no UAC prompt);
2. pairs it with the box and accepts the person's folders into
   `C:\Users\NAME\yggstore`;
3. opens that folder in Explorer and says it's done.

It does what connect.ps1 does, written in Go so it's one file with no
PowerShell. The box serves one copy for everyone and adds its own details
(address, name, Syncthing ID) to the end of the file as it sends it, so
there's nothing to type. Downloaded from a signed-in person's page, it also
carries a one-time pairing code, so it works away from the box's network.

**What the person sees:** Windows says "Windows protected your PC" the first
time, because the file isn't signed: **More info → Run anyway**. A
code-signing certificate removes that warning (about $200–400 a year). I'd
start without one and decide after the test on Kath's laptop.

Mac gets the same as a small app later (with one right-click → Open the
first time, unless it's notarised: Apple's $99 a year). Linux keeps the
one-line script.

### 2. Mail in the folder

The box already opens each person's mail (with their key, kept on the box)
to forward it. It also writes each new message to their `mail\` folder:

- `mail\2026-10-25 Asking to join - Contact form.eml`, which opens in Mail,
  Outlook or Thunderbird with a double-click, attachments included.
- Read-only: deleting one in the folder doesn't delete it from the group.
  The box puts it back the next time. Mail is deleted from the browser page.
- Sending stays on the browser page for now. Sending by dropping a file in
  an `outbox` folder can come later if people want it.

Mail is then on the person's computer unencrypted, like any mail program
keeps it. The page that turns this on says so.

### 3. Test on Windows before anyone gets it

On Kath's laptop, in as few steps as possible. There's no
Windows machine here, only Wine, so the installer is checked under Wine
first (it catches most mistakes, not all). Then it's one double-click on
her laptop while I watch the box's side.

## Still to decide

- **Who hands out boxes.** Either one Pi stays at home as a setup station
  people come to, or every box is flashed from a master image and goes to
  the person. Neither changes steps 1–3. A master image is a separate piece
  of work: no copy of localbox-002, because a copy would carry 002's keys
  and address. Each Pi has to make its own on first boot.
- **A code-signing certificate**, after the Windows test.

## Build order

1. The Windows installer, checked under Wine here. **Built:** `cmd/yggstore-join` and
   `internal/join`; the hub hands it out at `/join.exe` (the button on its
   page under Windows) when `yggstore-join.exe` sits beside its binary, which
   `deploy.sh` now does. `scripts/hub-join.sh` runs it end to end on Linux
   against a hub with real Syncthing. Under Wine it installed Syncthing 2.1.7
   (checksum checked), set it to start at sign-in and added the box. Not
   built yet: the one-time pairing code for computers away from the box's
   network.
2. Mail in the folder, with tests.
3. One try on Kath's laptop.
4. The master Pi image, once the first question above is answered.
