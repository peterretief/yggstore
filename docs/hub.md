# A box that devices sync with (hub)

`yggstore hub` turns a box such as a Raspberry Pi into a place the
computers and phones on your LAN sync their files with. Each device gets its
own folder on the box, through [Syncthing](https://syncthing.net). Whatever
it puts there is stored in the group: encrypted, split up and spread over the
members' machines. The device doesn't need yggstore, only Syncthing.

The hub pairs devices by itself. Any device on the local network that adds
the box is accepted, and you can turn that off at any time.

The hub also serves a page on the LAN (port 7490). It shows:

- **This box's node**: whether it's up, how much it holds of what it gives
  the group, and how long it has been running.
- **Syncing**: the box's Syncthing ID, whether it is taking new devices,
  and how many are paired.
- **Put a node on this computer**: members with their own box can install
  a full node from here, with an invite.

Unlike the [dashboard](remote-dashboard.md), the page holds nothing private,
so it needs no list of devices: anyone on the LAN can open it, and nobody
from outside the LAN can.

## Start it

On the box, run Syncthing as the same user as yggstore, then the hub:

```sh
sudo apt-get install -y syncthing
systemctl --user enable --now syncthing
sudo loginctl enable-linger $USER     # keep both running when you log out
yggstore hub -service                 # runs it now and at every start
sudo ufw allow from 192.168.0.0/16 to any port 7490 proto tcp     # the page
sudo ufw allow from 192.168.0.0/16 to any port 22000              # Syncthing
sudo ufw allow from 192.168.0.0/16 to any port 21027 proto udp    # finding it on the LAN
```

The hub finds Syncthing's address and API key in its `config.xml`. The
devices' folders go in `~/yggstore-hub/users/NAME` and websites in
`~/yggstore-hub/sites/DOMAIN` (`-dir` to change it).

## Syncing a device

1. Install Syncthing on the device: on Windows, macOS, Linux or Android.
2. In Syncthing, choose **Add Remote Device**. The box shows up in the list
   of nearby devices; or enter the ID shown on the box's page.
3. Within a few seconds the hub accepts it and shares a folder called
   **yggstore**. Syncthing asks whether to add it: accept, and choose where
   to keep it on the device. (Tick **Auto Accept** when adding the box, and
   Syncthing doesn't ask.)

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
are turned off, and only devices that connect from a local address (such as
`192.168.…`, `10.…` or `fd…`) are paired. Others are turned away and logged.
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
- It answers only loopback, private (`192.168.…`, `10.…`, `fd…`) and
  link-local addresses, in case its port is opened to the internet by
  mistake.
