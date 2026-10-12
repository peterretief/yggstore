# A box that devices sync with (hub)

`yggstore hub` turns a box such as a Raspberry Pi into a place the
computers and phones on your LAN sync their files with. Each device gets its
own folder on the box, through [Syncthing](https://syncthing.net). Whatever
it puts there is stored in the group: encrypted, split up and spread over the
members' machines. The device doesn't need yggstore, only Syncthing.

The hub pairs devices by itself. Any device on the local network that adds
the box is accepted, and you can turn that off at any time.

The admin sets each box up before it goes out ([below](#setting-up-a-box-in-the-factory)):
its name, its group and its websites are on it already. Where it ends up,
it needs only power and a network cable, and users need only its page.

The hub serves that page on the LAN, at `http://NAME.local:7490`. It is
written for the people who use the box, not for developers:

1. **Install Syncthing**, with steps for Windows, Mac, Linux, Android
   (Syncthing-Fork) and iPhone (Möbius Sync). The visitor's own system is
   opened.
2. **Connect to this box**: the box's Syncthing ID and its address
   (`tcp://IP:22000`), each with a Copy button, and where to paste them in
   Syncthing. Then accept the folder the box offers.
3. **Use the folder**: how to store, restore and delete files.

Below that, **This box** shows whether its node is up, how many of the
group's machines answer, and whether it is taking new devices. A folded
section at the bottom lets members with a computer of their own install a
full node, with an invite.

Unlike the [dashboard](remote-dashboard.md), the page holds nothing private,
so it needs no list of devices: anyone on the LAN can open it, and nobody
from outside the LAN can.

## Setting up a box in the factory

The admin sets up a Raspberry Pi on their own machine, the one whose
dashboard is the group's admin, with the Pi on the same network. Nothing is
typed on the Pi. Its settings are in one file, `factory/pi.conf`:

| Setting | What it is |
|---|---|
| `NAME` | The box's one name: on the network (`NAME.local`), as a node in the group, and as the hub users see. Set it for each Pi. |
| `OWNER` | Whose space and uploads the box counts towards in the group. |
| `PI_SSH` | How to reach the Pi on the bench (`root@NAME.local`). |
| `WIFI_SSID`, `WIFI_PASSWORD`, `WIFI_COUNTRY` | The Wi-Fi network it joins, for a Pi without a cable, such as a Pi Zero 2 W. Empty for a cable. |
| `STORAGE` | Where the node keeps its shards: `sd`, on the drive it boots from, or `usb`, on a USB stick plugged into the Pi. |
| `GROUP`, `ADMIN_DASHBOARD` | The group, and the admin dashboard that makes the box's invite. There is one group. |
| `QUOTA_GB`, `CUSTOMERS` | What the node gives the group, and whether it holds customers' files. |
| `PAIR_LIMIT`, `SITES`, `SITE_MAX_MB` | The hub's settings: devices paired a day, the websites it keeps, their largest size. |

Then, for each Pi:

1. **Write its drive.** Plug a USB drive or SD card into the admin's
   machine:

   ```sh
   scripts/flash-pi.sh              # lists the USB drives and SD cards
   scripts/flash-pi.sh /dev/sdX     # writes one (asks for sudo, and to type its name)
   ```

   It downloads Raspberry Pi OS Lite (64-bit), checks it and writes it.
   Then it sets up the first boot, through cloud-init on the boot
   partition. The Pi takes `NAME`, turns SSH on and lets this machine's key
   (`~/.ssh/id_ed25519.pub`, or `SSH_KEY`) in as root, with no passwords.
   A Pi 3 B+ or newer boots from USB as it comes; a Pi Zero 2 W boots
   from an SD card. With `WIFI_SSID` set, it also joins that Wi-Fi network.
   The password goes on the drive as the WPA key worked out from it, not as
   typed.

2. **Boot it** with a network cable that reaches the internet, or in
   reach of its Wi-Fi network. After a minute or two it answers at
   `NAME.local`. With `STORAGE=usb`, plug in the stick for the shards; on a
   Pi Zero it goes in the USB port, not PWR, through an OTG adapter.

3. **Set it up:**

   ```sh
   scripts/provision-pi.sh -n       # optional: show the steps, change nothing
   scripts/provision-pi.sh
   ```

   It names the Pi and installs this machine's yggstore build and
   Syncthing. With `STORAGE=usb`, it formats the stick (after you type
   `erase`; a stick it formatted before is kept as it is) and mounts it at
   every boot. If the stick is missing, the node waits for it rather than
   filling the SD card. It installs the Yggdrasil daemon (Debian's
   package), which links to the group's peers from the invite and to
   machines on the local network. The daemon reaches every member over TCP,
   where the node's built-in Yggdrasil talks UDP, which members' firewalls
   often drop. A box set up earlier with the built-in one is moved onto the
   daemon with the same key, so it keeps its address. It joins the Pi to the group, with an invite from the admin
   dashboard, and starts its node, hub and Syncthing at every boot. Last,
   it checks that the node is up, the group answers and Syncthing is ready.
   It ends with the address of the box's page.

Each step is skipped if it is done already, so after fixing whatever
stopped it, run it again. Another config file can be given:
`scripts/provision-pi.sh factory/other.conf`.

A box set up with Raspberry Pi Imager instead works too: set `PI_SSH` to
the user and name chosen there (the script uses `sudo` when that user isn't
root).

### A Pi Zero 2 W

A Pi Zero 2 W makes a small hub on Wi-Fi. It has no network socket, starts
from a microSD card, and keeps the shards on a USB stick in its USB port
(the middle one, not PWR), through a micro-USB OTG adapter. Its settings
are in `factory/zero.conf`: set `NAME`, the Wi-Fi where it will live
(`WIFI_SSID`, `WIFI_PASSWORD`, `WIFI_COUNTRY`; 2.4 GHz only) and, if that
isn't this network, the factory's (`BENCH_WIFI_SSID`, `BENCH_WIFI_PASSWORD`).
Then, with the card in a reader on the admin's machine:

```sh
scripts/setup-zero.sh              # lists the cards it could write
scripts/setup-zero.sh /dev/sdX     # writes it, waits for the Zero, sets it up
```

It writes the card as `flash-pi.sh` does, then asks you to put the card and
stick in the Zero and power it. It waits for the Zero to come up on Wi-Fi
(the first start takes a few minutes) and checks it is a Zero 2 W: the
first Pi Zero W is 32-bit and too slow. Then it runs `provision-pi.sh`,
which formats the stick after you type `erase`. If something stopped it
after the card was written, `scripts/setup-zero.sh -s` carries on from the
running Zero.

### The hub's group

The hub uses the group of the node running on the same machine. It finds
that node (`yggstore serve`) and reads its member list, preferring the one
the admin last pushed, so the hub needs no settings of its own. Without a
node it uses `~/.yggstore/peers.json`, or `-peers`. A box that isn't in a
group yet says so on its page, and files wait on it until it joins one.

## Start it by hand

This is what `scripts/provision-pi.sh` does. On Raspberry Pi OS (or Debian,
Ubuntu), as root, which suits a box that does only this:

```sh
apt-get install -y syncthing
systemctl enable --now syncthing@root   # Syncthing for root, at every boot
yggstore hub -service                   # the hub, at every boot
```

Run as root, `-service` sets up services in `/etc/systemd/system`, started
at boot with nobody logged in (`yggstore join -service` does the same). The
hub's service starts Syncthing's first. Check them with
`systemctl status yggstore-hub` and `journalctl -u yggstore-hub -f`.

The hub and Syncthing then run as root, and they take devices from the LAN.
If you'd rather limit what a fault in either could do, run both as a user of
their own instead:

```sh
sudo apt-get install -y syncthing
systemctl --user enable --now syncthing
sudo loginctl enable-linger $USER     # keep both running when you log out
yggstore hub -service                 # runs it now and at every start
```

Nothing here depends on which network the box is plugged into: the hub
works out its local network as it goes, so it can move to another LAN.

If you turn on a firewall (Raspberry Pi OS has none by default), let in
every private range, so it keeps working on whatever LAN the box ends up on:

```sh
for net in 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 fd00::/8 fe80::/10; do
  sudo ufw allow from $net to any port 7490 proto tcp    # the page
  sudo ufw allow from $net to any port 22000             # Syncthing
  sudo ufw allow from $net to any port 21027 proto udp   # finding it on the LAN
done
```

The hub finds Syncthing's address and API key in its `config.xml` (under
`~/.local/state/syncthing/`, which for root is in `/root`). The devices'
folders go in `~/yggstore-hub/users/NAME` and websites in
`~/yggstore-hub/sites/DOMAIN` (`-dir` to change it).

## Syncing a device

The box's page walks users through this; in short:

1. Install Syncthing on the device: on Windows, macOS, Linux, Android
   (Syncthing-Fork) or iPhone (Möbius Sync).
2. In Syncthing, choose **Add Remote Device** and paste the box's ID from
   its page. Under **Advanced**, paste the box's address too
   (`tcp://IP:22000, dynamic`): the box may show up among nearby devices by
   itself, but not on every network.
3. Within a few seconds the hub accepts it and shares a folder called
   **yggstore (NAME)**. Syncthing asks whether to add it: accept, and
   choose where to keep it on the device. (Tick **Auto Accept** when adding
   the box, and Syncthing doesn't ask.)

Then, in that folder:

- **Put a file or folder in** and it is stored in the group. The file is
  replaced by a small stub (`photo.jpg.ystub`) on the box and on the device,
  which is what you keep to get it back.
- **Put a stub into `restore/`** and the file comes back in `restored/`.
- **Put a stub into `delete/`** and the file is deleted from the group.
- Put a file in again after changing it, and the older version is kept too
  (see [history](history.md)).

A stub deleted on the device is kept on the box, in the folder's
`.stversions`, so deleting one by mistake doesn't lose the file.

## Websites

A hub can also keep websites. Each one is a folder of its own, separate
from the files: Syncthing offers it to every paired device next to the
**yggstore** folder, and when someone changes it, the hub publishes it to
the group (see [websites](sites.md)) about 15 seconds after the last change.

Set a site up on the box before devices pair. Publish it once from the box,
which makes the box the machine allowed to change it, then hand it to the
hub:

```sh
yggstore site publish ./shop -name shop.example.org    # once, from the box
yggstore hub site add shop.example.org
```

The hub fills the site's folder (`~/yggstore-hub/sites/shop.example.org`)
with the version being served, so everyone starts from the live site.
A hub can keep several sites; each is published separately.

An edit that looks like a mistake is held back, and the live site stays as
it was:

- the folder has no `index.html`
- it shrank to less than half of the version being served
- it is bigger than `-site-max` (500 MB)

`yggstore hub sites` shows why, and `yggstore hub site publish DOMAIN`
publishes it anyway. Every version is kept, so `yggstore site rollback
DOMAIN` undoes an edit. A site is published at most once a minute.
Syncthing's own files (`.stfolder`, `.stversions`, `.stignore`) are never
published.

```sh
yggstore hub sites                       # the sites, when each was last published
yggstore hub site remove shop.example.org   # stop syncing and publishing it (it stays up)
```

For now every paired device can edit every site on the hub. Keep the hub's
LAN to people you'd let change them.

## Turning it off

```sh
yggstore hub off          # stop taking new devices; paired ones keep syncing
yggstore hub on           # take new devices again
yggstore hub pause        # stop syncing for every device
yggstore hub resume
yggstore hub users        # who is paired, and from where
yggstore hub remove NAME  # unpair a device; its folder stays on the box
```

The hub picks a change up within a few seconds, and it survives restarts.
At most 5 devices pair in a day (`-pair-limit`); others wait until the next
day, or until you raise it.

## What the hub changes in Syncthing

It keeps Syncthing to the LAN: global discovery, relays and NAT traversal
are turned off, and only devices on the local network are paired. That is a
private address (such as `192.168.…`, `10.…` or `fd…`), or one on the same
network as the box (as with most home IPv6). Networks that only live on
the box, such as Docker's, VPNs (Tailscale, WireGuard) and Yggdrasil, don't
count as local. Others are turned away and logged.
Each device's folder is shared with that device only.

Everything on the box is stored as the box's owner, with its sharing key.
That suits one household or people who trust whoever runs the box: they can
read what arrives before it is stored.

## Putting a full node on another computer

1. An admin makes an invite on their dashboard (**Invite someone**).
2. On the new computer, open the box's page and paste the invite. The page
   gives you a command:

   ```sh
   curl -fsSL 'http://pi.local:7490/install.sh' | sh -s -- 'yggjoin1:…'
   ```

3. Run the command in a terminal on that computer. It downloads yggstore for
   that system into `~/.local/bin`, joins the group with the invite and, on
   Linux, starts the node and dashboard as services (`yggstore join -service`).

A web page can't install anything on the computer that opens it by itself,
so you have to run the command. It stops without changing anything if the
computer already has a node (`~/.yggstore/peers.json`).

The installer is for Linux (x86-64, 64-bit ARM, 32-bit ARMv7) and macOS. On
Windows, follow the [storage box guide](storage-box-guide.md).

### Where the build comes from

The hub hands out its own build, so a Pi gives other Pis yggstore without
going to the internet. For other systems, put release builds in a folder
under their release names, and pass it with `-bin`:

```sh
yggstore hub -bin ~/yggstore-builds      # holds yggstore-linux-amd64, yggstore-macos-apple-silicon, ...
```

For a system the hub has no build for, the installer downloads the latest
[release](https://github.com/peterretief/yggstore/releases) and checks it
against the release's checksums.

## What the page lets people do

- The invite is the only secret, and it never goes to the hub: the page
  builds the command in your browser. Without an invite, someone on the LAN
  can see the box's status and download yggstore, and that's all.
- The page and installer come over plain HTTP on the LAN. Anyone who can
  tamper with your LAN traffic could change the installer, so use the hub
  on a network you trust.
- It answers only the local network, in the same sense, in case its port
  is opened to the internet by mistake.
