# Open your dashboard and files from your other devices

Open your dashboard and your files from your laptop or phone, at home or
away, with no password: the dashboard knows your devices by their
Yggdrasil address.

Whoever opens the dashboard can restore, share and delete your files and
read your mail, so it answers only on its own machine
(<http://127.0.0.1:7480>) unless it's started with `-remote`
(`yggstore join -service` does this). Then it also answers your own
devices over Yggdrasil:

- A Yggdrasil address can only be used by the device holding its key, so
  it says which device is asking. Only you can add to your list of devices,
  not even the group's admins.
- On the local network, <http://BOX.local:7480> only sends the browser on
  to the box's Yggdrasil address, where the device is checked.
- A device not on the list is shown its address, and can ask to be let in.

## Let a device in

1. Install [Yggdrasil](https://yggdrasil-network.github.io/installation.html)
   on the device. Away from the box's network, give it one of the public
   peers shown under **Your devices**.
2. On the device, open the box's address, such as
   `http://[200:1f12::cfd6]:7480/files`. It says the device isn't let in,
   and offers **Ask to let it in**.
3. Press **Allow** next to it under **Your devices** on the dashboard (or
   on the box's screen: **A**, **1**, its number). Reload the page on the
   device.

Or add it by address: `yggstore devices add 201:5e1:8a3c::7 "anna's laptop"`.
`yggstore devices` lists them; `yggstore devices remove NAME` shuts one out
at once. Remove a device you sell or lose.

## Pages

| Page | Address | What's on it |
| --- | --- | --- |
| Files | `/storage`, `/files` | what's stored, restoring, sharing; your files in the browser |
| Website | `/web` | your name, address and websites |
| Messages | `/messages` | messages between the group's machines |
| Mail | `/mail` | your mail |
| Wallet | `/wallet` | your yggs ([ygg](ygg.md)) |
| Admin | `/admin` | the group, invites, people on the box, devices, usage |

A device you **Allow** gets every page but **Admin**, which changes who
gets in, so it's only given on purpose: tick the pages each device may open
under **Your devices**, or run
`yggstore devices pages "anna's phone" mail,messages`. The device you're
using can't take Admin from itself.

## Your files

`/files` works on your folder directly: browse, download, upload files or
whole folders of any size, make folders, and rename, move or delete with
**⋯**. **Your websites** changes a site's files, published as any change
is. A new upload shows *storing…* for a minute, until it's in the group.

## Names instead of addresses

A link like `http://[201:1193::a74]:7480` breaks in email and chat, so each
machine also gets a name such as `localbox-003.ygg.withcare.co.za`: a DNS
record pointing at its Yggdrasil address. Anyone can look it up, but only
devices on Yggdrasil reach it, and the device list still decides who gets
in. An admin dashboard with a Cloudflare API token adds the records itself;
without one, its **Network** page lists them.

## Setting it up by hand

`yggstore join -service` and `provision-pi.sh` do this. Otherwise:

1. Give the box a name on its network: install `avahi-daemon`.
2. Let the network reach the dashboard and Avahi, and Yggdrasil reach the
   dashboard:

   ```sh
   sudo ufw allow from 192.168.0.0/16 to any port 7480 proto tcp
   sudo ufw allow from 192.168.0.0/16 to any port 5353 proto udp
   sudo ufw allow in on tun0 to any port 7480 proto tcp
   ```

3. Add `-remote` to the dashboard's command and restart it. Its log says
   where it answers.

Pages over Yggdrasil are encrypted end to end, but the browser sees plain
`http://`, so **Copy** buttons may select the text instead of copying it.
