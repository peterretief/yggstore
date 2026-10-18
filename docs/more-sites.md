# More than one website for someone on a box

Status: proposed on 2026-10-18, waiting for approval. It changes one thing
decided on 2026-10-15 in [box-people-sites.md](box-people-sites.md): "one
name per person".

## Why

Tim is on localbox-004 with tim.withcare.co.za. He also runs a business
site, messagemate.co.za. A copy of it is on the group as
messagemate-test.withcare.co.za, published from the desktop. He wants to
download its files, edit them and upload them again himself.

Today he can't, for two reasons:
- a person on a box gets one name, and the desktop holds the copy;
- every node's member list check (`peers.validNames`) refuses a second
  name for the same person.

Others will want the same: a club page beside their own, or their own
domain.

## What changes

A person keeps one **address**: the first name they were given. That
name is still where they sign in (`/files`) and where their mail comes
(tim@withcare.co.za). On top of that they can hold any number of
**sites**. A site is only a website: it has no mail and no `/files`.

A site can be either of these:

| Kind | Example | Who vets it |
| --- | --- | --- |
| A name under the group's domain | messagemate-test.withcare.co.za | the group admin approves it, the same way as the address |
| A domain of their own | messagemate.co.za | the group admin approves it **and** routes the domain to the group's tunnel. Whoever controls the domain points its DNS there. |

Each site works the same way as the address's site does today:
- It's a folder in their files, `websites/NAME`, under **Your websites**.
  They download, edit and upload its files there, and the box publishes
  every change.
- The site card on the files page shows each of their sites. Each one
  has its own contact form switch and **Back to the previous version**.
- Contact form messages are sealed for the person's key. They land in the
  same **Messages** list and are forwarded the same way: all of Tim's go
  on to tim@messagemate.co.za.
- Their storage quota counts all of their sites.
- When the box admin removes them, every name goes back to the group
  admin. When the box admin suspends them, every site comes down.

## In the member list

There's still one `people_names` entry per name. A site is marked
`"site": true`:

```json
"people_names": [
  {"name": "tim.withcare.co.za", "person": "Tim", "code": "ys1…"},
  {"name": "messagemate-test.withcare.co.za", "person": "Tim", "code": "ys1…", "site": true}
]
```

The check becomes: each person has exactly one entry without `site`, and
every entry for a person carries the same code. Mail (`peers.MailHolder`), `peers.PersonNameOf` and
the sign-in at `NAME/files` look only at the entries without `site`.

Web nodes take a site from the node holding its name, as they do today.
For a domain of the group's own, nothing new is needed. For a domain of
their own, it's new: today an outside domain belongs to whichever node
published it first. A domain listed in `people_names` instead belongs to
the box that holds it. This is what lets the copy move from the desktop to
Tim's box, and it stops anyone else from taking it over.

**Every node needs the new version first.** An older node's check
refuses a second entry for one person, and it would drop the box's entry
from its member list. The admin's dashboard won't approve a site while any
node runs an older version, and it says which ones.

## Asking for one

On the box's dashboard, under **People on this box**, Tim's row gets
**Add a site**. The box admin types the name and the request goes to the
group admin, the same way the address did. The group admin sees "a site
messagemate-test.withcare.co.za for Tim, on localbox-004 (peter)", with
Approve and Decline.

## Moving a site someone already publishes

For messagemate-test.withcare.co.za, the desktop publishes the copy now.
Once Tim's site is approved:

1. The box takes the files the web nodes are serving now. They become
   Tim's `websites/messagemate-test.withcare.co.za`, so nothing is lost
   and he starts from what's live.
2. The box publishes the site. The web nodes take it from the box because
   the box now holds the name.
3. The desktop stops publishing its copy. Its contact messages now go to
   Tim's Messages instead of Peter's mailbox.

## messagemate.co.za itself

This plan makes it possible but doesn't do it. The live site stays where
it is until Tim's business decides to move it. If it moves:
- its DNS points at the group's tunnel;
- the admin approves messagemate.co.za as Tim's site;
- the box publishes it.

The mail records for messagemate.co.za aren't touched. Tim's mail there
stays where it is.

## Build order, once approved

1. The member list: `site` entries, the new check and tests. Mail and
   sign-in look only at the address.
2. A person's sites run from the box: one watcher for all of them, the
   quota over all of them, and taking down and giving back all of them.
3. Domains of their own: web nodes take a listed outside domain only
   from its box.
4. Asking and approving, with the "every node is up to date" check.
5. The files page and the site card, for several sites.
6. Moving a published site to a person: take the files the web nodes
   serve, then the old publisher stops.
7. Try it with Tim and messagemate-test.withcare.co.za.

## To decide

- Can a person ask for a site themselves on `/files`, or only through the
  box admin? I'd start with the box admin only, as for the address.
- Is there a limit on the number of sites per person? I'd say 5 to begin
  with, which the box admin can raise.
