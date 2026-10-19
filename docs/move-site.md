# Moving a website to another machine

Status: proposed on 2026-10-19, with its open questions decided the same
day ([below](#decided-2026-10-19)), waiting for approval to build. It builds on
[more-sites.md](more-sites.md) (sites that are only websites, with no
mail) and doesn't change how sites are published or served.

## Why

Tim's laptop is about to connect to localmail, the box at his office. He
wants to edit messagemate-test.withcare.co.za, the copy of his business
site, from that laptop. Today the copy is published from the desktop, so
it isn't in anything localmail syncs, and there's no way to hand it over:

- A name under the group's domain is served only from the machine the
  member list gives it to (`peers.SiteKeeper`), or from an admin.
  messagemate-test isn't given to anyone: it works only because the
  desktop is the admin.
- The machine that publishes a site keeps every version's stub
  (`~/.yggstore/sites/NAME/versions/`). Nobody else has them, so nobody
  else can roll the site back or keep its leases renewed.
- The publishing machine's websites folder puts a site's folder back if
  it's deleted, so it would carry on publishing anything edited there.
- Contact form messages go to the publishing machine's mailbox.

The same move comes up again for Tim's own tim.withcare.co.za (from
localbox-004 to localmail), and for messagemate.co.za itself if the
business moves its live site onto the group.

## What the admin sees

On the admin dashboard, a **Websites** page lists every site the group's
web nodes serve: its name, where it's published from, when it last
changed and whether its contact form is on. Each row has **Move**.

**Move** asks where to:

| Destination | Who can edit it there |
| --- | --- |
| **A box, for everyone on it** | every device paired with the box, in its `websites` folder (`~/yggstore-websites/NAME` on their computers) |
| **A person on a box** | only that person, in their own folder (`~/yggstore/websites/NAME`) and under **Your websites** on their files page |
| **A member's machine** | its owner, in their outbox's `websites/` folder or with `yggstore site publish` |

Then it shows what will happen, for example:

> messagemate-test.withcare.co.za moves from **desktop** to **Tim, on
> the box localmail**. Its 1 version moves with it. The site stays online
> throughout, and so does its contact form: new messages go to Tim's
> Messages on localmail once the move is done. Move it?

and **Move it** starts it. The row then shows **Moving…** and, once the
new machine serves it, **localmail** as its publisher. Nobody else has to
do anything.

## How it moves

The site stays online the whole time: the web nodes go on serving the
version they have until the new machine announces the same version as its
own.

1. **The name changes hands in the member list.** The admin's dashboard
   gives the name to the destination and pushes the list, as it does when
   it approves a name. A site-only name goes in a new `sites` list on a
   node's entry, or in `people_names` with `"site": true` (from
   [more-sites.md](more-sites.md)):

   ```json
   {"name": "localmail", "sites": ["messagemate-test.withcare.co.za"], "code": "ys1…"}
   ```

   A site name has no mailbox: messagemate-test@withcare.co.za doesn't
   become an address. The check `peers.validNames` refuses a name held
   twice, so it's taken off the old holder in the same push.

2. **The old machine hands over its versions.** The admin sends it a
   `site-move` message (site, destination node and, for a person, their
   name on the box). The old machine sends each version's stub to the
   destination as a `site-handover` message, sealed for the destination's
   sharing code: the person's own code for a person on a box, the node
   owner's otherwise. One version per message: a stub is about 9 KB and a
   message holds 64 KB. The last message carries the site's settings
   (contact form on or off, and which version is being served).

3. **The new machine takes it on.** It files the stubs under its own
   `sites/NAME/versions/` and renews their leases straight away. It fills
   the site's folder (the box's `websites/NAME`, the person's
   `websites/NAME`, or the outbox's) with the version being served. Then
   it announces that version. Because the member list now gives it the
   name, the web nodes take the announcement from it and record it as the
   site's owner (`Web.handle` does this already for names under the
   group's domain). The new machine answers `site-moved` to the old one
   and to the admin.

4. **The old machine lets go.** On `site-moved`, or once the member list
   no longer gives it the name, the old machine:
   - stops publishing the site and stops renewing its leases;
   - moves its `sites/NAME/` to `sites/.moved/NAME-DATE/` and deletes it
     after 30 days, in case the move has to be undone;
   - leaves its websites folder for the site in place, with a note beside
     it, `NAME - MOVED.txt`, that says where it went and when. It no
     longer refills the folder, and nothing edited there is published.

Every version is kept, and `yggstore site rollback` works on the new
machine as it did on the old one.

### The contact form

A site's contact form is how customers send new orders, so a move never
turns it off and never loses a message. The form follows the site:

- **To a person on a box:** their messages are sealed for the person's own
  code, and land in their **Messages**, which they can forward on (to
  tim@messagemate.co.za, for example).
- **To a box for everyone, or a member's machine:** the messages go to that
  node's mailbox.

**Move** checks first that the destination collects mail. If it doesn't
(localmail's node doesn't today), it won't start, and says what to set up
on the destination; until then the form keeps working where it is.

During the move, messages go on to the old holder until the new one has
announced the site: the web nodes switch the form's delivery at the same
moment as the site's owner. A message the web node can't deliver is kept
and tried again, as today. Both machines keep collecting mail until the
move is done, so nothing falls between them.

After a move, the admin's Websites page sends a test message through the
form and shows whether it arrived at the new holder. Until it has, the row
shows **contact form not yet checked**.

### Domains outside the group's

For a site on a domain of its own, such as messagemate.co.za, the web
nodes don't look at the member list: the site belongs to whoever
published it first (`Web.mayChange`). For these, the admin's dashboard
also sends the web nodes a signed `site-owner` message (site, new owner).
Web nodes already take changes to any site from an admin, so this is a
small addition: they record the new owner and take the next announcement
from it. Routing and DNS don't change: the domain points at the group's
tunnel either way.

### If the old machine is off

The admin can't wait for a machine that's gone. After an hour, the
**Moving…** row offers **Move without it**. The new machine then starts
from the version being served: the web nodes have that version's stub,
from its announcement, and send it on. Earlier versions are lost, and the
admin is told so before choosing it. When the old machine comes back, the
member list tells it the name has gone, and it lets go as in step 4.

## Moving back, and mistakes

A move is undone by moving the site back: it's the same steps the other
way. Within 30 days the old machine still has its versions in
`sites/.moved/`, so they aren't lost even after **Move without it**.

The move refuses to start:
- while the site is moving already;
- to a destination that isn't up;
- to a destination that can't take the site's contact form messages;
- to a person who has been suspended or removed from their box.

## Who may move a site

- **The group admin** (Peter) may move any site, to and from anywhere.
  This is the "super access" for when something needs fixing.
- **A box's admin** may move a site only between the people on their own
  box, and between those people and the box itself: the people who
  connect to that box locally. They can't move a site off their box, or
  onto it from elsewhere; that goes through the group admin.

## What isn't changing

- How a site is stored, published, checked and rolled back.
- The website folders, and how edits in them are published.
- Routing, DNS and the Cloudflare tunnels.
- The live messagemate.co.za, which is served by Apache on localmail and
  isn't a group site at all. Moving it onto the group is a separate
  decision for the business.

## Build order

1. **Site names in the member list**: `sites` on a node's entry, shared
   with more-sites' `"site": true` for people, with the checks in
   `peers.validNames` and `SiteKeeper`.
2. **Handover**: `site-move`, `site-handover` and `site-moved`, with
   `Publisher` export and import, lease renewal on arrival, and the
   `.moved` copy.
3. **Folders let go**: the `NAME - MOVED.txt` note, for the outbox's,
   box's and person's website folders.
4. **The admin's Websites page** and **Move**, including **Move without
   it**.
5. **Domains outside the group's**: the `site-owner` message on web nodes.

Step 5 can wait until a site on its own domain needs to move.

**First real use:** move messagemate-test.withcare.co.za from the desktop
to Tim, as a person on localmail. Before it:

1. Tim's laptop connects to localmail (the one-line command), and the box
   links the laptop to a person called Tim.
2. localmail's node collects mail (a mailbox for the box, which needs a
   short restart of its yggstore node; the mail server isn't touched).

Then Tim edits the site from his laptop in
`~/yggstore/websites/messagemate-test.withcare.co.za`, the site changes
within a minute, and a test order sent through its contact form reaches
his Messages and is forwarded to tim@messagemate.co.za.

## Decided (2026-10-19)

1. **messagemate-test goes to Tim as a person on localmail**, not to the
   box for everyone. Tim is the only one connected to localmail today, so
   either would do, but a person's site can't clash with someone else on
   the box editing it later.
2. **The contact form must keep working**: customers use it for new
   orders. A move never turns it off (see [the contact form](#the-contact-form)).
3. **Box admins move sites only among the people connected to their own
   box. The group admin can move any site**, to put things right.
