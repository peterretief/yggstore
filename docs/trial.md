# Try it: accounts anyone can make

Status: approved and built 2026-11-01; not yet deployed.

Anyone can make an account at `https://my.DOMAIN/signup` and try the
group: a small website at `NAME.DOMAIN` and a few files they can share
by link. Nothing is installed; it all works in the browser. Yggdrasil and
the group's nodes run behind it, as for everyone else.

## What the person sees

1. **Sign up** at `my.DOMAIN/signup`: a name (it becomes their address),
   their email address and a password of at least 10 characters.
2. **Confirm:** an email comes with a link. Opening it signs them in.
3. **Their page** is the usual files page: upload, download and delete
   files, **Share** one by link, and their website, which starts with a
   first page at `https://NAME.DOMAIN`.
4. **Sign in again** at `my.DOMAIN` with the email address they signed
   up with, or `NAME@DOMAIN`, and their password.
5. **Mail** to `NAME@DOMAIN` comes to their mail page, where they can
   **Write** and **Reply** from that address.
6. **Leave** at the bottom of the page deletes their files, their website
   and their account.

They get 500 MB. An account nobody signs in to for 60 days is removed, with
an email a week before.

## How it works

- **One box takes the accounts:** the one marked `"trial": true` in
  `peers.json` (localbox-004 for withcare.co.za). Each account is a
  person on that box, like someone the box's owner adds, with the same
  files page and website. Their files are stored in the group as usual.
- **Signing up** goes to a web node, which passes it to the box over
  Yggdrasil. The box keeps it, waiting, and the web node emails the link
  from `yggstore@DOMAIN`. The link opens on any web node: the box makes the
  person and the web node signs them in.
- **Their name** is asked of the group's admin, as for anyone on a box,
  and given automatically if it's free.
- **A share link** (`my.DOMAIN/s/BOX/…`) downloads one file, without
  signing in, for 30 days or until they stop sharing it. It always
  downloads; it never opens as a page.
- **Leaving** puts every stored file in the box's `delete/`, so it is
  removed from every node, then removes the person, gives their name back
  and deletes their folder. After 7 days it finishes anyway, and the
  nodes drop any pieces left when their leases run out.

## Limits

- At most 50 trial accounts on the box, and 20 new ones a day.
- 3 sign-ups a day from one internet address.
- Mail they send goes to at most 5 people a message and 20 people a day
  (others on a box: 20 and 200). It is always from their own address,
  and the box logs each one, so the admin can see who sent what.
- One email per address at a time: asking again sends the same link, at
  most every 10 minutes. A link works for 2 days.
- Trial sites are ordinary group sites under the domain. The admin can
  suspend or remove any person from the box's admin, as now.

## Settings

| Where | What |
| --- | --- |
| `peers.json` | `"trial": true` on the box that takes the accounts |
| web nodes | `-mail-out`, to send the emails |

## Not in the trial

Syncthing, devices, teams, the dashboard and a node of their own. Joining
the group properly is still by invite.

## For developers

- `internal/hub/trial.go`: sign-ups, confirming, leaving and closing
  unused accounts, on the box; `share.go`: share links.
- `internal/light/trial.go`: the sign-up page, passing sign-ups and links
  to the box, the emails, and share links, on the web nodes.
- The box's dashboard asks for each new account's name
  (`askTrialNames`).
