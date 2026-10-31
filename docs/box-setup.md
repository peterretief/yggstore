# The box's own screen

Plug a box into a screen and keyboard to see that it works, set it up for
its owner, and run it, with no other computer.

## The status screen

The box shows this instead of a login prompt:

```
  LocalBox  localbox-005    peter's box

  Network    Wi-Fi "Kitchen"  192.168.0.15            ok
  Internet   yes                                      ok
  Group      reached 4 of 4 other machines            ok
  Storage    57.8 MiB used of 40.0 GiB                ok
  Services   all 6 running                            ok

  Website    https://peterretief.withcare.co.za
  Email      peterretief@withcare.co.za
  Sign in    https://my.withcare.co.za  as peterretief@withcare.co.za
  Drive      \\localbox-005\peter

  R Restart    S Shut down    A Admin
```

- A line that isn't ok says what to do, such as "plug in the network
  cable, or join Wi-Fi". Read it out to whoever helps you on the phone.
- **R** and **S** ask before they restart or shut down. Shut a Pi down this
  way before unplugging it: pulling the plug while it writes can spoil its
  SD card.
- The screen checks every 15 seconds. Nothing secret is shown, and there is
  no shell.

## Set up a new box

A box with nobody on it shows **Press Enter to set up this box**.

1. Press **Enter**.
2. Type your first name.
3. Type a password twice: at least 10 characters, a few words together is
   easiest. It's your password for the web, the box's admin and your
   network drive.
4. Type a name for your website and email, such as `harbourkitchen`. The
   screen shows what you'll get and refuses names that are taken.
5. Press **Y**.

The box ticks each line as it's done: you as its owner, your password and
login, your folder, your network drive, and your website and email. The
status screen then shows them. On a member's box the name waits for the
group's admin; on the admin's own box it's given at once.

## The admin

Press **A** on the status screen and type the owner's name and password.
**Esc** goes back; so do five minutes without a key. Five wrong passwords
close it for an hour.

| Key | What it does |
| --- | --- |
| **1** Devices | the devices that may open the dashboard, and the ones refused; let one in by its number, or **N** by address |
| **2** People | **A** adds someone; choose a person by number for the keys below |
| **3** Websites | the contact form and ygg button of the box's own sites |
| **4** Services | what's running; restart one by its number |
| **5** Wi-Fi | join a network (it sets the country first) |

For a person:

| Key | What it does |
| --- | --- |
| **W** | ask for a name for their website and email |
| **N** | notices on their website: add one, or take one off |
| **G** / **D** | their site's preview goes live, or is dropped |
| **C** | type their password now, while they're at the box |
| **P** | a new temporary password, to give them |
| **L** | their storage limit |
| **S** | shut them out for now, or let them back |
| **R** | remove them; their files stay |

Changes that can't be undone ask first. Over SSH, the same admin is
`sudo yggstore admin`.

## Network drives

Each person who chose their password at the box has their folder as a
network drive on the local network: `\\BOX\NAME` in File Explorer, or
`smb://BOX.local/NAME` on a Mac or Linux. Sign in with their name and
password. Each person sees only their own folder.

- The drive answers only on private addresses, never on Yggdrasil or the
  internet.
- Shutting someone out turns their drive off; removing them takes it away.
- A password changed later on the web doesn't reach the drive. Choose it
  again at the box (**C**).

To set a box up for drives, install `samba` and `wsdd2`, write an
`smb.conf` that binds to 127.0.0.1 and the private ranges only
(`interfaces`, `hosts allow`, `bind interfaces only`), with
`map to guest = never`, `access based share enum = yes` and
`include = /etc/samba/yggstore-shares.conf`, and make
`/etc/samba/yggstore.d`. `provision-pi.sh` doesn't do this yet.

## Put the screen on a box

```sh
sudo install -m 644 deploy/screen/yggstore-screen.service /etc/systemd/system/
sudo systemctl mask getty@tty1.service
sudo systemctl enable --now yggstore-screen.service
```

The other consoles (**Ctrl+Alt+F2** and up) keep their login prompts. To
undo it, disable `yggstore-screen.service` and unmask `getty@tty1.service`.
`yggstore screen -once` prints the status in any terminal, such as over
SSH.
