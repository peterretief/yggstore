# Set up your yggstore storage box

*For Windows users.* Turn a second-hand mini PC into your storage box: it keeps your files backed up across the group, and holds a share of everyone else's. You'll need about an hour, once. After that it runs by itself.

| Word | Meaning |
|---|---|
| **Storage box** | The little computer you set up here. The technical word for it is a *node*. |
| **Group** | The people who back each other up. Each of your files is encrypted, cut into 6 pieces and spread over their boxes; any 4 pieces rebuild it. |
| **Invite** | A one-time code from the group's organiser, starting `yggjoin1:`. It works once, for 7 days. |
| **Stub** | The small `.ystub` file left in place of a stored file. It holds the key; without it the file can't be rebuilt. |

> [!NOTE]
> This guide hasn't been followed end to end on real hardware yet. If a step doesn't match what you see, please [open an issue](../../issues).

## What you need

- A second-hand mini PC or thin client, for example an HP t630 (about R1,000 refurbished).
- An external USB drive for the group's storage, ideally an SSD or a 2.5″ hard drive. **It will be erased.**
- A USB stick of 2 GB or more for the installer. **It will be erased.**
- A screen and keyboard for the first half hour only. The t630 has DisplayPort sockets, so you may need a DisplayPort-to-HDMI adapter.
- A network cable from the box to your router.
- Your invite message, and the `yggstore-linux-amd64` program file (from the organiser, or this project's [releases](../../releases)).

## 1. Make the installer stick — *on your Windows PC*

The box will run Debian Linux instead of Windows: it's free, light and runs for years unattended.

1. Download the Debian “netinst” image for **amd64** from [debian.org/distrib/netinst](https://www.debian.org/distrib/netinst).
2. Download [Rufus](https://rufus.ie), plug in the USB stick, pick the Debian file in Rufus and press **Start**. If Rufus asks how to write the image, choose **DD Image mode**.

## 2. Install Debian on the box — *on the box, with screen and keyboard*

> [!WARNING]
> **This erases the box.** Unplug the external storage drive for now, so it can't be erased by mistake.

Connect the screen, keyboard, network cable and the installer stick. Switch on and keep tapping **F9** (on HP boxes) to get the boot menu, then choose the USB stick. In the installer:

- Choose **Install**, then your language, country and keyboard.
- Hostname: something like `storagebox`. Leave the domain empty.
- **Leave the root password empty.** Your own account then gets admin rights through `sudo`, which these steps use.
- Create your user; remember the username and password.
- Partitioning: **Guided – use entire disk**, on the box's own small disk.
- Software selection: **untick** “Debian desktop environment” and “GNOME”; **tick** “SSH server” and “standard system utilities”.

When it finishes, remove the stick and let it restart. Log in, then find the box's network address:

```sh
ip -br address
```

Note the address starting with `192.168.` (or `10.`). Below, replace `192.168.0.50` with it and `you` with your username. Tip: in your router's settings, reserve that address for the box so it never changes.

You can now unplug the screen and keyboard.

## 3. Connect from Windows — *on your Windows PC*

Open **Terminal** (or PowerShell) from the Start menu. Windows 10 and 11 can connect to the box directly:

```powershell
ssh you@192.168.0.50
```

Answer `yes` the first time, then type your password. Everything marked “on the box” from here on is typed in this window.

## 4. Prepare the storage drive — *on the box*

Plug in the external drive, then list the drives:

```sh
lsblk -o NAME,SIZE,MODEL
```

Find your external drive by its size, for example `sdb`. The box's own disk is usually `sda`.

> [!WARNING]
> **Check the name twice.** The next command erases that drive. Replace `sdb` if yours differs.

```sh
sudo mkfs.ext4 -L yggstore /dev/sdb
sudo mkdir -p /srv/yggstore
echo 'LABEL=yggstore /srv/yggstore ext4 defaults,nofail 0 2' | sudo tee -a /etc/fstab
sudo systemctl daemon-reload
sudo mount -a
sudo chown $USER: /srv/yggstore
df -h /srv/yggstore
```

The last line should show the drive's size. It now mounts by itself at every start.

## 5. Install Yggdrasil — *on the box*

Yggdrasil is the encrypted network the group's boxes talk over. These are the [official install steps](https://yggdrasil-network.github.io/installation-linux-deb.html):

```sh
sudo apt-get install -y gnupg
sudo mkdir -p /usr/local/apt-keys
gpg --fetch-keys https://neilalexander.s3.dualstack.eu-west-2.amazonaws.com/deb/key.txt
gpg --export 1C5162E133015D81A811239D1840CDAC6011C5EA | sudo tee /usr/local/apt-keys/yggdrasil-keyring.gpg > /dev/null
echo 'deb [signed-by=/usr/local/apt-keys/yggdrasil-keyring.gpg] http://neilalexander.s3.dualstack.eu-west-2.amazonaws.com/deb/ debian yggdrasil' | sudo tee /etc/apt/sources.list.d/yggdrasil.list
sudo apt-get update
sudo apt-get install -y yggdrasil
sudo systemctl enable --now yggdrasil
```

Now give it the peers listed in your invite message, so it can reach the group. Open the settings file:

```sh
sudo nano /etc/yggdrasil.conf
```

Find the line `Peers: []` and put the peers from your invite between the brackets, in quotes, separated by commas, like this:

```
Peers: ["tls://91.98.161.68:9001?key=0e638944bfd6b277fa5e0dddbeb4444778eea8bece63a9862c661797022a8f05", "tls://5.252.118.13:443"]
```

Save with **Ctrl+O**, Enter, and leave with **Ctrl+X**. Then restart it and check it's connected:

```sh
sudo systemctl restart yggdrasil
sleep 10
sudo yggdrasilctl getPeers
```

At least one line should say `Up`.

## 6. Close the doors — *on the box*

On Yggdrasil your box gets an address anyone on that network can reach. This firewall lets in only two things: the group, on the storage port, and you, from your home network.

```sh
sudo apt-get install -y ufw
sudo ufw default deny incoming
sudo ufw allow from 192.168.0.0/16 to any port 22 proto tcp
sudo ufw allow in on tun0 to any port 7400 proto tcp
sudo ufw enable
```

If your home addresses start with `10.`, use `10.0.0.0/8` instead of `192.168.0.0/16`. Answer `y` when asked.

## 7. Copy the yggstore program to the box — *on your Windows PC*

Open a **second** Terminal window, go to the folder where you saved the program (usually Downloads) and copy it over:

```powershell
cd $HOME\Downloads
scp .\yggstore-linux-amd64 you@192.168.0.50:
```

Then, back in the window connected to the box:

```sh
sudo install -m 755 yggstore-linux-amd64 /usr/local/bin/yggstore
```

Or, instead of copying it over, download the latest release straight onto the box and check it isn't damaged:

```sh
curl -fLO https://github.com/peterretief/yggstore/releases/latest/download/yggstore-linux-amd64
curl -fLO https://github.com/peterretief/yggstore/releases/latest/download/SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS
sudo install -m 755 yggstore-linux-amd64 /usr/local/bin/yggstore
```

The check should say `yggstore-linux-amd64: OK`.

## 8. Join the group — *on the box*

Change the space you give (in GB; stay below your drive's size), your box's name and your name, and paste your invite between the quotes:

```sh
yggstore join -service -dir /srv/yggstore -outfiles /srv/yggstore/outfiles \
  -quota 500 -name anna-box -me "Anna" \
  'yggjoin1:PASTE-YOUR-INVITE-HERE'
```

It should end with *You're in*. Then keep it running after restarts, even when nobody is logged in:

```sh
sudo loginctl enable-linger $USER
systemctl --user status yggstore-node --no-pager
```

The status should say `active (running)`. The organiser is now in your contacts, and you in theirs.

## 9. Open your dashboard — *on your Windows PC*

The dashboard only answers on the box itself, so nobody else on your network can use it. To open it from Windows, connect with this command and leave the window open:

```powershell
ssh -N -L 7480:127.0.0.1:7480 you@192.168.0.50
```

Then open <http://127.0.0.1:7480> in your browser. You'll see the group's boxes, your files, your sharing code and what you give and use.

### Without the tunnel (optional)

Your dashboard can also open from your PC directly, at `http://anna-box.local:7480`, once your PC runs Yggdrasil too and the box knows it's yours. On the box:

```sh
sudo apt-get install -y avahi-daemon
sudo ufw allow from 192.168.0.0/16 to any port 7480 proto tcp
sudo ufw allow from 192.168.0.0/16 to any port 5353 proto udp
sudo ufw allow in on tun0 to any port 7480 proto tcp
```

Install [Yggdrasil for Windows](https://yggdrasil-network.github.io/installation-windows.html) on your PC, then open `http://anna-box.local:7480` (your box's name instead of `anna-box`). The page shows your PC's Yggdrasil address and the command that lets it in, `yggstore devices add ADDRESS "my pc"`; run that on the box and reload. More in [remote-dashboard.md](remote-dashboard.md).

## 10. Store, restore and share files — *on your Windows PC*

Install [WinSCP](https://winscp.net) (free) and connect to `192.168.0.50` with your username and password, protocol SFTP. Open `/srv/yggstore/outfiles`; it works like a drop box:

| To | Do this |
|---|---|
| Store | Drag files or folders in. Each file is replaced by its `.ystub` once it's safely stored and checked. |
| Restore | Press Restore on the dashboard, or move a `.ystub` into `restore`. The file appears in `restored`. |
| Send to someone | Press Share on the dashboard and send them the `.ysend` file. Drop `.ysend` files you receive into `outfiles`. |
| Delete | Press Delete on the dashboard, or move a `.ystub` into `delete`. |

## 11. Keep a copy of your keys — *on your Windows PC*

> [!IMPORTANT]
> **Your stubs exist only on your box.** If its drive dies, the pieces of your files are still safe on the other boxes, but without the stubs nobody can put them back together.

With WinSCP, copy these to your PC (or another drive) now, and copy the stubs again whenever you've stored new files:

- all `.ystub` files in `/srv/yggstore/outfiles` and its folders;
- `/srv/yggstore/sharing.key`, which opens files people send you.

Keep them private: anyone with a stub who can reach the group can read that file.

## Living with it

### Leave it on

The group relies on your box being reachable. A thin client like the t630 uses under 10 W, a few kWh a month.

### Updates

Once a month, on the box:

```sh
sudo apt-get update && sudo apt-get upgrade -y
```

New versions of yggstore come from the organiser or the releases page; copy and install them as in step 7, then run `systemctl --user restart yggstore-node yggstore-dashboard`.

### If something's wrong

| You see | Try |
|---|---|
| *Yggdrasil is not running* when joining | Check step 5: `sudo yggdrasilctl getPeers` must show a peer `Up`. |
| *Could not reach the group* | Same as above, then try again in a minute. The organiser's box may be restarting. |
| *The group did not accept this machine* | The invite was used or has expired. Ask for a new one. |
| The dashboard page doesn't load | The window from step 9 must stay open; run that command again. |
| The box doesn't appear on the group's dashboard | `systemctl --user status yggstore-node` on the box; if it isn't running, `systemctl --user restart yggstore-node`. |

---

*Written for yggstore as it is today. Later versions aim to replace steps 1–8 with a ready-made image and a setup page you open from Windows. A printable version with copy buttons is in [storage-box-guide.html](storage-box-guide.html).*
