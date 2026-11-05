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

So there's no new kind of account. A developer is a member, usually with a
box, and their clients are the people on it. Four things are left to build.

## What's decided

- **Anyone may join,** and the admin approves each one for now (the invite,
  or the application form).
- **Each developer moves to their own Cloudflare account.** They can start
  on the group's, and move when they're ready.
- **Giving storage is optional, but space isn't free.** A developer who
  gives none pays the developers who do, and yggs keep track.
- **Every developer has a machine of their own.** Without a box, that's a
  small server running [a box in Docker](docker-box.md), which gives little
  storage. Their clients live there, so no other member could read their
  mail.

## 1. A client's own domain

This is the most important step: it's what clients pay developers for, and
it's what the admin does by hand today. It builds on
[more than one website](more-sites.md), which allows a person a domain of
their own. There are two stages.

**Starting out, on the group's Cloudflare account:**

1. On the box's dashboard, the developer adds `acme.example` to a client.
2. The admin's dashboard, through Cloudflare's API, adds the zone, routes
   the site to the web nodes and the mail to the Worker, and sends back
   the two nameservers.
3. The developer's dashboard shows them, and what to change at the
   registrar, then says when the domain is live.

**Moving to their own account:** a Cloudflare tunnel belongs to one account,
so a developer's domains can't point at the group's. They need their own:

1. a tunnel, on a web node of their own (`-web`, `-web-tunnel`), which can
   be their box;
2. the mail Worker in their account, with a token for their web node;
3. sending set up for their domains (`-mail-out`).

A guided step on the dashboard takes their Cloudflare API token and does
this, then moves each zone across, with the records it had. Their clients'
sites stay stored in the group as before.

A site then reaches the internet only through that developer's tunnel, so
one web node is a single point of failure. The tunnel should run on a
second machine too, with the developer's own second box or another member
they trust holding its token ([always on](web-access.md#always-on)).

## 2. A clients page

**People on this box** grows into one view of a developer's clients:
for each one their site and whether it answers, their domain and whether
its DNS is right, mail received lately, and space used. Anything wrong
shows first. The box already knows most of this.

## 3. Paying for space

Give and use already counts what each member stores on others and holds
for them, and the ledger already charges it at midnight
([yggs](ygg.md#step-2-storage-in-the-ledger)). So a member who gives no box
simply runs up a balance below 0, and the members holding their files earn
it. What's left to build:

- **Real settling.** Yggs are only a record today. A member past the limit
  below 0 (50,000 yggs, $50) has to pay, in a way still to be chosen
  ([settling](ygg.md#settling)).
- **What happens past the limit.** It only warns now. It should warn, then
  refuse new uploads, and never take a client's site down without notice.
- **The rate.** 10 yggs per GB-month is 1 US cent, far too little to matter
  to anyone. It needs setting.
- **Other work than storage.** A web node, a tunnel for others and sending
  mail cost something too, and aren't counted.

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
   tries this, with the admin doing the Cloudflare part.
2. A client's own domain from the dashboard, on the group's account.
3. The clients page.
4. Moving to their own Cloudflare account.
5. Settling, and what happens past the limit.
6. Moving a client out, and admin handover.

## Still to decide

1. **Who answers abuse?** Once on their own account, a developer answers
   for their domains. While on the group's, and for names under its domain,
   the admin does, and one bad client hurts everyone's mail.
2. **The rate and how to settle,** as above.
