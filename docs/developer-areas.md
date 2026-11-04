# Developers and their clients

Status: proposed 2026-11-04, waiting for approval. Nothing is built.

yggstore is a co-op of independent developers, each hosting their own
clients. Today one person, the group's admin, does part of every
developer's work by hand. This plan lets a second developer run their
clients without the admin, and without seeing anyone else's.

## The idea: a developer's area is their box

Most of the separation exists already. A box keeps its own people, and only
its owner runs it:

| A developer needs | Today |
| --- | --- |
| Their clients, kept apart from others' | People on a box: each box has its own |
| Limits, suspending, removing | Built: **People on this box** |
| Client sign-in, files and mail in a browser | Built ([web access](web-access.md)) |
| Publishing for a client | Built: their `websites/` folder, or `git push` |
| Nobody else changing their sites | Built: a site changes only from the machine that published it, or an admin |
| Nobody else reading client files | Built: other members hold only encrypted pieces |
| **A client on their own domain** | **By hand, by the admin, and one name per person** |
| **All their clients in one view** | **Partly: People on this box shows people, not site or mail health** |
| **A fair share of space** | **Counted (Give and use), not limited** |
| **Taking a client elsewhere** | **Partly: moving a site is built, moving a person is parked** |

So there's no new kind of account. A developer is a member with a box, and
their clients are the people on it. Four things are left to build.

## 1. A client's own domain

This is the most important step: it's what clients pay developers for, and
it's what the admin does by hand today. It builds on
[more than one website](more-sites.md), which allows a person a domain of
their own.

1. On the box's dashboard, the developer adds `acme.example` to a client.
2. The request goes to the group's admin, as a name does. Approving it
   is one click, and it could become automatic.
3. The admin's dashboard, through Cloudflare's API, adds the zone to the
   group's account, routes the site to the web nodes and the mail to the
   Worker, and sends back the two nameservers.
4. The developer's dashboard shows them, and what to change at the
   registrar, then says when the domain is live.

Mail to any address at the domain comes to that client. Sending from it
needs the domain added to the mail relay and its DNS records, also through
the API.

## 2. A clients page

**People on this box** grows into one view of a developer's clients:
for each one their site and whether it answers, their domain and whether
its DNS is right, mail received lately, and space used. Anything wrong
shows first. The box already knows most of this.

## 3. Fair shares

A co-op needs a rule, or one member's clients fill everyone's disks. Give
and use already counts what each member stores and gives. The rule:
**you may store on others about as much as you give**, with a warning at
80%, and new uploads refused at 120%. Yggs stay as a record, with no money
changing hands.

## 4. Leaving, and a clear promise

Developers will ask what happens to their clients in a bad week.

- **A developer leaves:** they take each client's site, mail and files
  with them. This needs [moving a person](move-person.md), now parked.
- **A box dies:** sites and mail carry on, the box is rebuilt from the
  group, and its people return ([recovery](recovery.md)). This should
  be tried for real and written down.
- **The admin disappears:** today this would stop new names and domains.
  [Admin handover](admin-handover.md) is on hold, and a co-op can't lean on
  one person for long. It should come after the first few developers.

## Build order

1. Domains for a client, by hand but written down
   ([host your first client](host-a-client.md)). The first outside developer
   tries this, with the admin doing step 3 of the domain flow.
2. A client's own domain, from the dashboard.
3. The clients page.
4. Fair shares.
5. Moving a client out, and admin handover.

## Decisions

1. **Who may join?** By invite and the admin's approval, as now, or do
   members vouch for new ones?
2. **Whose Cloudflare account?** One for the group is simplest, but it
   holds every client's zone, and the group's domain reputation is shared.
   Each developer having their own is safer and much more work.
3. **What may a developer's clients do?** Everyone shares withcare.co.za's
   mail reputation, so one bad client affects all. Who answers abuse
   reports?
4. **Must every developer bring a box?** This plan says yes.
