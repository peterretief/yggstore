# Open your dashboard and files from your other devices

The dashboard has no password: whoever can open it can restore, share and
delete your files and read your mail. So by default it only answers on the
machine it runs on, at <http://127.0.0.1:7480>.

It also serves [your files](#your-files) at `/files`, to browse and change
in the browser, and both open on your own laptop and phone over Yggdrasil.

Started with `-remote` (`yggstore join -service` does this), it also opens
to **your own devices, over Yggdrasil**:

- A Yggdrasil address can only be used by the device holding its private
  key, so the address says which device is asking. You list your devices'
  addresses once, on the dashboard's machine; nobody else can add to that
  list, not even the group's admins.
- On the LAN the box has a name, such as `anna-box.local`. Opening
  <http://anna-box.local:7480> sends the browser on to the box's Yggdrasil
  address, where the device is checked. Nothing is served over the LAN
  itself.
- A device that isn't on the list is shown its own address and the command
  that would add it.

```mermaid
flowchart LR
  phone["your laptop<br/>(Yggdrasil)"] -- "anna-box.local:7480" --> lan["box, LAN<br/>sends you on"]
  lan -. "[200:…]:7480" .-> ygg["box, Yggdrasil<br/>is this device on the list?"]
  phone -- "[200:…]:7480" --> ygg
```

Each device needs Yggdrasil installed and running. On the same LAN it finds
the box by itself; elsewhere it needs a public peer (see
[the mesh](mesh.md#3-give-every-node-public-peers)).

## 1. Give the box a name on the LAN

Install Avahi, which answers for `HOSTNAME.local`:

```sh
sudo apt-get install -y avahi-daemon          # Debian, Ubuntu
apk add avahi && rc-update add avahi-daemon && rc-service avahi-daemon start   # Alpine
```

Windows 10 and later, macOS, iOS and Android find `.local` names by
themselves. Check from another machine with `ping anna-box.local`.

## 2. Open the doors

Let the LAN reach the dashboard (it only sends people on) and Avahi, and
Yggdrasil reach the dashboard (it checks the device):

```sh
sudo ufw allow from 192.168.0.0/16 to any port 7480 proto tcp
sudo ufw allow from 192.168.0.0/16 to any port 5353 proto udp
sudo ufw allow in on tun0 to any port 7480 proto tcp
```

With ip6tables instead of ufw, add `-A YGG-IN -p tcp --dport 7480 -j ACCEPT`
before the `DROP` and save.

## 3. Start the dashboard with -remote

`yggstore join -service` already does. Otherwise add `-remote` to the
dashboard's command (in `~/.config/systemd/user/yggstore-dashboard.service`)
and restart it:

```sh
systemctl --user daemon-reload && systemctl --user restart yggstore-dashboard
```

Its log says where it answers:

```
dashboard also on http://[200:1f12::cfd6]:7480 for the devices in /home/anna/.yggstore/devices.json, and http://anna-box.local:7480 on the LAN
```

## 4. Add your devices

The dashboard's **Your devices** section lists them, and says how to add
one:

1. Install Yggdrasil on the device
   ([Windows, Mac, Linux, Android and iPhone](https://yggdrasil-network.github.io/installation.html)).
   It makes the device its own key, which never leaves it.
2. Give it one of the public peers the section shows (in its settings,
   under Peers), so it finds your computer from anywhere.
3. On the device, open the address the section shows, such as
   `http://[200:1f12::cfd6]:7480/files`. The page says the device isn't on
   the list, shows its address, and has **Ask to let it in**.
4. Press **Allow** next to it under **Your devices**, on your computer or
   on a device already let in. Reload the page on the device.

**Remove** shuts a device out at once (the device you're using can't remove
itself). Requests are kept for a day, ten at most, so strangers on
Yggdrasil can't fill the list.

Or add a device by its address, on the dashboard or the command line:

```sh
yggstore devices add 201:5e1:8a3c::7 "anna's laptop"
```

```sh
yggstore devices                      # who may open it
yggstore devices remove "anna's laptop"
```

Remove a device when you sell or lose it. Its Yggdrasil keys are in its
`yggdrasil.conf`; anyone who copies that file can use its address.

> [!NOTE]
> The page goes over Yggdrasil, which encrypts it end to end, but the
> browser sees plain `http://`. So "copy" buttons may not reach the
> clipboard; they select the text instead, for Ctrl+C.

## Your files

`/files` on the dashboard (`http://127.0.0.1:7480/files` on its own
machine, or its Yggdrasil address on your devices) is the same page as
your [work from anywhere](recovery.md#your-files-in-the-browser) link, but
served by your computer and working on your folder directly:

- browse, and click a file to download it;
- upload files, or drag them onto the page, of any size;
- new folders, and **⋯** to rename, move or delete.

Changes are made at once, with no waiting on the web nodes and no 100 MB
limit. A file just uploaded shows *storing…* until it's stored in the
group, a minute or so, and is then a stub like any other. While your files
are being worked on at another place, the page only shows them.
