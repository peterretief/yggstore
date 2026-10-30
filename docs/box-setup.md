# Setting up on the box itself

Status: proposed 2026-10-29. The status screen with Restart and Shut down
is built (`yggstore screen`, 2026-10-30); the rest isn't. Goes with
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

**Set up this box**, only while it's unclaimed:

- their name, and the email address the order was placed with;
- their domain: one they have, a new one, or for now a name under the
  group's;
- then the box sends a link to the order's email address. Opening it on
  a phone finishes the claim and makes the passkey (a passkey can't be
  made on a text screen). The QR code for the phone is on the screen too.

This is the claim from [claim.md](claim.md), steps 5 and 6, with the
typing done on the box instead of the phone.

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

**Unlocking Settings** is the owner's:

- **With their phone:** the screen shows a QR code; they scan it and
  approve with their passkey. This is the usual way.
- **With a PIN** they chose when claiming the box, for when the
  internet is down and the phone can't reach it. Stored only as a hash,
  five wrong tries locks it for an hour, and changing it needs the phone.

Settings locks again after five minutes without a key pressed, and when
the screen or keyboard is unplugged. Changes that can't be undone
(removing a person, moving a domain) ask again, as on the admin page.

Also:

- **The claim needs the order's email** (the link sent to it), so a box
  plugged in by someone else on the way can't be claimed.
- **Files and mail aren't read on the box's screen:** they're for the
  owner's own devices. Settings change how they're kept, not what's in
  them.
- **No login prompt or shell** on the box's screens. Root has no
  password; the group's admin gets in over SSH as now.
- **Nothing secret is shown:** no keys, no codes once the box is claimed.

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
3. **Set up this box**, once unclaimed boxes and the claim page exist
   ([claim.md](claim.md#build-order) steps 1 to 3).
4. **Unlocking and Settings**, starting with Wi-Fi, people and devices,
   then the rest; each one uses the same code as the admin page.
5. **The thin clients**, with a keyboard plugged in: the same program,
   added to the x86 setup.
6. Try it with someone who hasn't seen it, on a box on another network.
