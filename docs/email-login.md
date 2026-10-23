# Signing in by email

Status: proposed 2026-10-23, and parked: people sign in with a password
the group's admin sets first (see [web-access.md](web-access.md#passwords)).
Email may come back as the way to reset a forgotten password.

A second way to sign in to the [group login](login.md), next to a passkey.
It works for everyday sign-in, for someone whose device can't make a
passkey (no screen lock), and for someone who has lost their devices. It
takes the place of the browser key and PIN, which was removed on
2026-10-23 (see [web-access.md](web-access.md#without-a-screen-lock)). Each person has a main email address and a backup
one. If both are lost, the box's owner, or the group's admin, rescues them.

## What the person sees

1. At `my.withcare.co.za` they choose **Sign in with email** and type their
   email address.
2. The screen shows a number, e.g. **47**, and waits.
3. An email arrives: "Signing in? Tap the number on your screen", with three
   buttons, e.g. **12 · 47 · 83**.
4. They tap 47. The waiting screen signs in. The device they tapped on shows
   "Done, go back to your screen". It might be their phone, while the screen
   is a laptop.
5. **No email?** The waiting screen has **Send to my backup address
   instead**.
6. **Neither address works?** They ask the box's owner, who gives them a
   rescue link (see [Rescue](#rescue)).

A wrong button cancels that sign-in. The email also says: "Didn't try to
sign in? Ignore this; nobody gets in without the number on your screen."

## Why the screen shows the number

The email's buttons are secret links, so nobody can guess one. Picking the
number on your own screen means you approve **your** sign-in. Someone who
starts a sign-in with your address can't get in unless you tap the number
on their screen, which you never see.

The other way round (the number in the email, the choices on the screen)
was considered and turned down. There, the only secret is which of a few
buttons to press, so anyone who knows your address could guess.

## How it works

- **The addresses live on the box** with the person: `Email` (it's already
  there, for the recovery link) and a new `Backup`. The box sends both to
  the web nodes along with the person's keys, as it does now. Signing in by
  email then works while the box is off.
- **An attempt** is kept on the web node and shared with the other web nodes,
  like the login's codes. It holds:
  - the person
  - the number shown
  - a hash of each button's link
  - a hash of the waiting browser's secret, kept in a cookie
  - when it ends, 10 minutes after it starts

  Each button works once. Once any button is tapped, the others stop
  working.
- **The waiting screen** asks every few seconds. Once the right button has
  been tapped, the web node starts a session for that browser only, and
  only if the browser has the attempt's cookie.
- **Who gets an email:** the address must be someone's main or backup
  address, and they must be allowed on the web: not suspended, not removed,
  and web access not turned off. Whatever the outcome, the page always says
  "If that address is in the group, we've sent an email", so it doesn't tell
  anyone who is in the group.
- **The session** is an ordinary group session, so the person's devices list
  shows it as "Email". Signing out works as for any other session. It lasts
  7 days unused, less than a passkey's 30, because email is easier to break
  into than a device.
- **Sending** uses the web nodes' existing mail relay (`workhere.Email`). A
  web node without the relay asks one that has it.

## Limits

- At most 5 emails an hour for any one person, and a limit for each
  address the requests come from, as for PINs.
- 3 wrong buttons in a day turn email sign-in off for that person. Both
  addresses get an email saying so. Signing in with a passkey turns it back
  on, and so does a rescue.
- **One email at a time.** Asking again while a sign-in is waiting sends
  the same email again (same number, same buttons), at most every 10
  minutes, so two emails never disagree. The waiting screen says when it
  went, that email can take a few minutes, and to look in the spam or
  junk folder; **Send it again** waits until the 10 minutes are up.

## Changing the addresses

Signed in, the person can change either address on their devices page. A
new address is used only after they tap the right number in an email sent
to it. Both old addresses get an email saying it changed. The box's owner
can also set them in People on the box.

## Rescue

The last resort, when both addresses are lost or taken over.

- **The box's owner** chooses **Rescue** next to the person in People on the
  box. They get a one-time link to give the person directly, in person or
  by phone, never by email. Email might be what was lost.
- **The group's admin** can do the same on any web node, with
  `yggstore login rescue BOX/PERSON`. It works while the box is off.
- **The rescue link** works once, within 24 hours. It signs the person in,
  signs out every other session they have, turns email sign-in back on, and
  asks them to make a passkey and check both addresses.
- Each rescue is logged on the box and on the web node.

## Trade-offs

- **Anyone who can read the person's email can sign in as them.** That is
  why email sessions are shorter, and why changing an address emails the
  old ones.
- **Email can be slow or end up in spam.** The backup address and rescue
  cover that.
- **The web nodes keep everyone's email addresses.** The addresses are
  already on the boxes, and the web nodes already vouch for everyone (see
  [login](login.md)).

## Build order

1. **Signing in by email** on the web nodes: attempts, the email, the
   waiting screen. Only the main address for now.
2. **The backup address**: on the box, in what boxes send the web nodes, and
   "Send to my backup address instead".
3. **Changing addresses** on the devices page, with the emails to the old
   addresses.
4. **Rescue**: from People on the box, and `yggstore login rescue` on a web
   node.
