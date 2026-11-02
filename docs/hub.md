# A box that devices sync with (hub)

A box running `yggstore hub` holds a folder for each person who uses it,
and stores everything put there in the group. People reach their folder as
a network drive, in a browser, or synced to their laptops and phones with
[Syncthing](https://syncthing.net); none of these needs yggstore on their
device.

The box's page, at `http://NAME.local:7490` on its local network, shows
people how to connect. It holds nothing private, so anyone on the network
can open it and nobody outside it can.

## Open your files

- **As a network drive**, on the box's network: `\\BOX\NAME` in File
  Explorer, or `smb://BOX.local/NAME` on a Mac or Linux, with your name and
  password ([network drives](box-setup.md#network-drives)).
- **In a browser**, from anywhere with Yggdrasil: the address under **Your
  files** on the box's page, such as `http://[200:1f12::cfd6]:7490/files`.
  Browse, upload, download, rename, move and delete. Your device's
  Yggdrasil address says who you are, so you only ever see your own folder
  ([your files](remote-dashboard.md#your-files)).
- **Synced to a device**, with Syncthing: see
  [sync a device](#sync-a-device).

The folder works the same in each:

```
files/       your files: each is stored in the group and becomes a stub
restore/     copy a stub here to get its file back
restored/    where it comes back, in the same folders as in files/
delete/      put a stub here to delete its file from the group
whole/       a folder put here is stored as one item
received/    files someone sent you
```

Putting a changed file back keeps the older version too
([history](history.md)). A stub deleted by mistake is kept on the box in
`.stversions`.

## Sync a device

On a computer, the box's page (under **Advanced**) gives one command:

```sh
irm http://pi.local:7490/connect.ps1 | iex          # Windows, in PowerShell
curl -fsSL http://pi.local:7490/connect.sh | sh     # Mac
wget -qO- http://pi.local:7490/connect.sh | sh      # Linux
```

It installs Syncthing if needed (checked against the release's
checksums), starts it at login, pairs with the box, and adds your folder in
`~/yggstore`, the websites in `~/yggstore-websites` and teams in
`~/yggstore-teams/NAME`. Running it again skips what's done.

On a phone (Syncthing-Fork on Android, Möbius Sync on iPhone), follow
**Connecting by hand** on the page: add the box's ID and address, and
accept the folder it shares.

New devices pair only from the box's local network, at most 5 a day. Once
paired, a device keeps syncing from anywhere, through Syncthing's discovery
and relays; what it syncs is encrypted end to end. The group's admin gets a
message on their dashboard when a device pairs or is turned away.

### People with several devices

Each person has one folder, synced by all their devices. A new device is a
new person, named after it, until you link it:

```sh
yggstore hub people                       # who uses the box, and their devices
yggstore hub link anna-phone anna-laptop  # the phone now syncs anna-laptop's folder
```

### Work from anywhere

On the box's page (**Advanced**, step 4), a person pastes their device's
Syncthing ID and an email address. The box keeps recovery copies of their
folder and emails them a link that opens their files in any browser, and
can pair another device from wherever it is. Only one device has the folder
at a time; **Move them here** takes it ([recovery](recovery.md)).

## A website and email address for someone on the box

Anyone on the box can have a name in the group, such as `anna.example.org`:
a website, a contact form and the address `anna@example.org`, with no node
of their own.

1. Ask for it: the box's admin, **People**, the person, **W** (or
   **Give them a website and email address** on the box's dashboard).
2. The group's admin gives it if it's free, and gets a report.
3. The box starts the site with a first page in the person's
   `websites/anna.example.org` folder. Changes there are published by
   themselves ([websites](sites.md)).

On their files page, under **Your website and messages**, the person turns
the contact form on, reads and answers messages, and can have them
forwarded to an address they already read (a code is sent there first,
then the box's admin presses **Allow**).

Their mail and messages are sealed for their own key. The key is kept on
the box, so the box's owner could read them, and their page says so.
**Suspend** takes the site down until they're let back in; **Remove** gives
the name back.

## Look after the people on the box

On the box's screen (**A**, then **2**), on the box's dashboard (**People
on this box**, opened only on the owner's devices), or on its command line:

```sh
yggstore hub person add anna              # prints her invite link
yggstore hub person invite anna           # a new one
yggstore hub person limit anna 20 3       # 20 GB, 3 devices (0: no limit)
yggstore hub person suspend anna          # or resume, or remove
yggstore hub people                       # what each stores, and their limits
yggstore hub devices                      # whose devices open which files
yggstore hub devices remove "anna's phone"
```

- **Add** makes an empty folder and an invite link, valid for a week on a
  device with Yggdrasil. The person names the device and lets their others
  in themselves.
- **Limit** caps their storage and devices; an upload that doesn't fit is
  refused, and they're told why.
- **Suspend** shuts them out and stops syncing; **Remove** does it for
  good. Either way their files stay, and adding them again brings them
  back.

## Team folders

A team folder is shared by several people and stays on the box, so the
others carry on when one is away. Each member's devices sync it as
**team NAME**.

```sh
yggstore hub team add Projects anna bob
yggstore hub team join Projects carol
yggstore hub team leave Projects bob
yggstore hub teams
yggstore hub team remove Projects          # stops syncing; its files stay
```

When two people change a file at once, Syncthing keeps both
(`plan.sync-conflict-…`); `yggstore hub conflicts` lists them, and the box's
page says how many wait. A recovery copy of the team folders' stubs is kept
in the group.

## Websites on the box

The box keeps the sites it publishes in `~/yggstore-hub/websites/`, a
folder per domain, offered to every paired device as **websites (NAME)**.
A change there is published as described in
[folders that publish themselves](sites.md#folders-that-publish-themselves).
`yggstore hub sites` shows each site and why one isn't published;
`yggstore hub site publish DOMAIN` publishes a held-back change anyway.

Every paired device can edit every site in this folder, so keep it to people
you'd let change them.

## Setting up a box in the factory

The group's admin sets each Raspberry Pi up from their own machine, with
the Pi on the same network; nothing is typed on the Pi. Its settings are in
`factory/pi.conf`, or a `factory/NAME.local` kept out of git:

| Setting | What it is |
| --- | --- |
| `NAME` | the box's name on the network and in the group |
| `OWNER` | whose space the box counts towards |
| `PI_SSH` | how to reach it on the bench (`root@NAME.local`) |
| `WIFI_SSID`, `WIFI_PASSWORD`, `WIFI_COUNTRY` | Wi-Fi, for a Pi without a cable |
| `STORAGE` | `sd` or `usb`: where the shards go |
| `QUOTA_GB`, `CUSTOMERS` | what it gives the group, and whether it holds customers' files |
| `PAIR_LIMIT`, `SITE_MAX_MB` | devices paired a day; the largest site |
| `TIMEZONE`, `KEYBOARD` | default `Africa/Johannesburg` and `us` |

1. **Write its card or drive:** `scripts/flash-pi.sh` lists them;
   `scripts/flash-pi.sh /dev/sdX` writes Raspberry Pi OS Lite, with SSH for
   this machine's key and the Pi's name.
2. **Boot it** with a network cable (or within reach of its Wi-Fi). It
   answers at `NAME.local` after a minute or two. With `STORAGE=usb`, plug
   in the stick.
3. **Set it up:** `scripts/provision-pi.sh` (or `-n` to see the steps). It
   installs yggstore, Syncthing and Yggdrasil, formats the stick after you
   type `erase`, joins the group with an invite from the admin dashboard,
   starts everything at boot, and checks it works. It ends with the box's
   page address. Run it again after fixing whatever stopped it.

With the shards on a USB drive, the node stops if the drive drops off,
rather than fill the card, and the drive is mounted and the node started
again when it's back. For a drive set up by hand, run
`scripts/drive-follows.sh MOUNTPOINT LABEL SERVICE` on the box as root.

A Pi Zero 2 W (Wi-Fi only, shards on a USB stick through an OTG adapter)
uses `factory/zero.conf` and `scripts/setup-zero.sh /dev/sdX`, which writes
the card, waits for the Zero and sets it up.

Over Yggdrasil the box lets in only its node (7400), dashboard (7480), hub
(7490), website (8480) and Syncthing (22000), and SSH only from the admin's
machine (`/etc/yggstore/yggdrasil-in.nft`).

### Start it by hand

On Raspberry Pi OS, Debian or Ubuntu, as root:

```sh
apt-get install -y syncthing
systemctl enable --now syncthing@root
yggstore hub -service
```

To run both as a user instead, use `systemctl --user enable --now syncthing`,
`sudo loginctl enable-linger $USER` and `yggstore hub -service`. A firewall
must let in the page (7490), Syncthing (22000) and discovery (21027/udp)
from every private range, so the box works on whatever network it ends up
on. The hub uses the group of the node on the same machine.

## Commands

```sh
yggstore hub off          # stop taking new devices; paired ones keep syncing
yggstore hub on
yggstore hub pause        # stop syncing for everyone
yggstore hub resume
yggstore hub users        # who is paired, and from where
yggstore hub remove NAME  # unpair a device; its folder stays
```

## Putting a full node on another computer

1. An admin makes an invite (**Invite someone** on their dashboard).
2. Paste it on the box's page, which gives a command:

   ```sh
   curl -fsSL 'http://pi.local:7490/install.sh' | sh -s -- 'yggjoin1:…'
   ```

3. Run it on the new computer (Linux or macOS). It installs yggstore, joins
   the group and starts the node. On Windows, follow the
   [storage box guide](storage-box-guide.md).

The box hands out its own build, and for other systems the latest release,
checked against its checksums (`-bin DIR` for builds of your own).

## What to know

- Everything on the box is stored as the box's owner, with its key: whoever
  runs the box can read what arrives before it's stored.
- The page and installer come over plain HTTP on the local network, so use
  the hub on a network you trust. The invite never goes to the hub; the page
  builds the command in your browser.
