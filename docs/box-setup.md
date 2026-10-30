# Setting up on the box itself

Status: proposed 2026-10-29. The status screen with Restart and Shut down
(`yggstore screen`) and a first admin (`yggstore admin`) are built
(2026-10-30); the rest isn't. Goes with
[claim.md](claim.md#the-boxs-screen), which has the box show a page on a
screen; with a keyboard as well, the box can be set up there. The first
box to try it on is a Pi 400, whose keyboard is built in.

## What it is

`yggstore setup`, a program on the box's own screen (the first console,
in place of the login prompt). Plain text and numbered choices, so it
works with any screen and keyboard and needs no browser or desktop on the
box. The same program runs on the Pi and on the thin clients.

The Pi 400's text is made larger at first start, so it can be read from
across a desk.

## The screens

**Status**, shown all the time unless another screen is open:

```
  LocalBox  harbourkitchen         Harbour Kitchen's box

  Network     Wi-Fi "Kitchen"        ok
  Internet                           ok
  Group       reached, 15 machines   ok
  Storage     57 GB stick, 12% used  ok
  Services    all running            ok

  Admin page: harbourkitchen.co.za/admin

  1 Wi-Fi    2 Set up this box    3 Settings    4 Restart    5 Shut down
```

Each line that fails says what to do in a few words: "no cable and no
Wi-Fi: press 1", "the stick isn't plugged in". The owner can read it out
on the phone.

**Wi-Fi:** the networks it can see, pick one, type its password. Useful
where the cable is a problem, or there's no free port on the router. The
cable is used whenever it's plugged in.

**Set up this box**, the first time it's started. Plugging it in should
be all a new owner has to do before this, and this should be the only
thing they're asked, all on one screen (on the box, or on their phone
through the claim link, [claim.md](claim.md)):

1. **Their first name.** They become the box's owner and its first
   person.
2. **A password**, typed twice. It signs them in everywhere: on the web,
   at the box (its admin), and on their network drive.
3. **A name for their website and email**, such as `harbourkitchen`,
   checked as free while they type. It gives
   `harbourkitchen.withcare.co.za` and `harbourkitchen@withcare.co.za`.
   Their own domain comes later, from the admin.

The box then does the rest by itself, ticking each line on the screen as
it's done:

```
  Setting up Harbour Kitchen's box

  [x] You're on the box, as its owner
  [x] Your folder, stored in the group as you add files
  [x] Your network drive: \\harbourkitchen\anna
  [x] Your name asked for: harbourkitchen
  [ ] Your website: harbourkitchen.withcare.co.za, with a first page
  [ ] Your email: harbourkitchen@withcare.co.za
```

- **The folder** is the person's folder on the box, as for anyone on
  it: what they put in is kept in the group, so a broken box loses
  nothing (the outbox, [README](../README.md#outbox-folder)).
- **The name** is asked of the group as a name for someone on a box is
  today ([hub.md](hub.md)), and the website and email start when it's
  given.
- **When it's done**, the status screen keeps showing what they have:
  their website, email, network drive and the admin page, so it's
  always visible, not only on the day.

Everything else is for later, once this works, and differs from owner
to owner: their phone and PCs (devices), other people on the box, their
own domain, a contact form, forwarding, and whatever systems they run.
All of it is in Settings.

**Settings**, for the owner, once they've unlocked it (below). The same
settings as on the admin page, so either place can be used, and a change
in one shows in the other:

- the box's name, their domain, and the names under it;
- people on the box: add, change their limit, suspend, remove;
- devices: list, remove, and add one by showing a QR code to scan;
- mail: addresses, forwarding;
- websites: which are on, the contact form and the ygg button;
- storage: the limit given to the group, and a new stick or disk;
- the network: fixed addresses, and forgetting saved Wi-Fi (joining one
  needs no unlocking, so a box that lost its network can be brought back).

**Restart** and **Shut down** ask once more. Shutting down properly
matters on a Pi: pulling the plug while it's writing can spoil the SD
card.

## Who can do what at the box

Anyone near the box can use its keyboard, so without unlocking it only
does what's safe for anyone in the room: Status, Wi-Fi, Restart and Shut
down.

**Settings is the owner's, and logging in on the box is the key.** The
status screen says how: Ctrl+Alt+F2, log in with the box's user and its
password, then `sudo yggstore admin` (sudo asks for the password again).
It leaves after five minutes without a key pressed, back to the login
shell. Changes that can't be undone ask first, as on the admin page.

Later, for owners who never use a console, approving with their phone's
passkey (a QR code on the screen) could open it too.

Also:

- **The claim needs the order's email** (the link sent to it), so a box
  plugged in by someone else on the way can't be claimed.
- **Files and mail aren't read on the box's screen:** they're for the
  owner's own devices. Settings change how they're kept, not what's in
  them.
- **No login prompt or shell** on the box's screens. Root has no
  password; the group's admin gets in over SSH as now.
- **Nothing secret is shown:** no keys, no codes once the box is claimed.

## Network drives

On a box set up with Samba, each person who chooses their password on
the box (`yggstore admin`, People, C) gets their own folder as a network
drive: `\\BOX\NAME` in File Explorer, or `smb://BOX/NAME` on a Mac or
Linux, signed in with their name and that password. It's on the local
network only: Samba listens on private addresses, never on Yggdrasil or
a public address. Each person sees only their own folder. Shutting
someone out turns their drive off; removing them takes it away, and
their files stay.

Samba keeps its own copy of the password, so a password changed later
on the web doesn't reach the drive; it's chosen on the box again.

Setting a box up for it, install `samba` and
`wsdd2` (so Windows finds the box under Network), and an `smb.conf`
with `interfaces` and `hosts allow` set to 127.0.0.1 and the private
ranges, `bind interfaces only`, `map to guest = never`, `access based
share enum = yes`, and `include = /etc/samba/yggstore-shares.conf`;
then make `/etc/samba/yggstore.d`. `provision-pi.sh` doesn't do this
yet.

## Putting the screen on a box

`yggstore screen` runs on the first console from
`deploy/screen/yggstore-screen.service`, in place of the login prompt:

```
sudo install -m 644 deploy/screen/yggstore-screen.service /etc/systemd/system/
sudo systemctl mask getty@tty1.service
sudo systemctl enable --now yggstore-screen.service
```

The other consoles (Ctrl-Alt-F2 and on) still have login prompts. To
go back: `sudo systemctl disable --now yggstore-screen.service` and
`sudo systemctl unmask getty@tty1.service`, then start `getty@tty1`.

`yggstore screen -once` prints the same status in any terminal, for
checking a box over SSH.

## Build order

1. **Status, Restart and Shut down**: the console program, started
   instead of the login prompt, with larger text. Works on every box
   today, claimed or not. Built; tried first on a thin client with HDMI.
2. **Wi-Fi**, through NetworkManager (already on Raspberry Pi OS).
3. **Set up this box** on a box with no people yet: the three questions,
   then the person, password, network drive and name request in one go,
   and the status screen showing the website, email and drive. The same
   on the phone once the claim page exists
   ([claim.md](claim.md#build-order)).
4. **Settings** (`yggstore admin`, built 2026-10-30 for devices, with
   the ones that were refused; people on the box; websites; services),
   then Wi-Fi and the rest; each one uses the same code as the admin
   page.
5. **The thin clients**, with a keyboard plugged in: the same program,
   added to the x86 setup.
6. Try it with someone who hasn't seen it, on a box on another network.
