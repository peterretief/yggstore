# A box for a new owner, claimed with a code

Status: proposed 2026-10-29. Nothing is built.

A box is sold to someone the group doesn't know yet, and sent to their
home or office. They plug it in and claim it with a code from their order.
From then on it's their box: their domain, files, mail and websites are
bound to it, and they run it from their own admin page. They don't install
anything to get there, and they don't see the other boxes in the group.

For example, the owner of a restaurant, Harbour Kitchen, buys a box. Its
website is `harbourkitchen.example`, and mail to
`bookings@harbourkitchen.example` and from the site's booking form comes
to the box. The domain is theirs, registered in their name; the group
only serves it. The box can also order from the restaurant's suppliers
in the group ([suppliers.md](suppliers.md)).

## What exists already

- **Setting a box up before it ships** ([hub.md](hub.md#setting-up-a-box-in-the-factory)):
  it joins the group, and at its destination it needs only power and a
  network cable.
- **Names under the group's domain** ([mail.md](mail.md#addresses-for-members)):
  `anna@withcare.co.za` and `anna.withcare.co.za`, approved by the group's
  admin.
- **Sites on a domain of one's own** ([more-sites.md](more-sites.md), proposed;
  [sites.md](sites.md#2-publish-a-site)): the domain on the group's
  Cloudflare account, published from a box.
- **Pages through the web** ([web-access.md](web-access.md), [login.md](login.md)):
  people on a box use their files and mail at an `https://` address,
  signed in at `my.withcare.co.za`, without Yggdrasil.
- **Registering domains** in the old LocalBox app (domains.co.za's API,
  then the zone on Cloudflare). It can be carried over.

What's missing is the handover: a box that is nobody's yet, a code that
makes it someone's, and an admin page they can reach without Yggdrasil.

## From order to first sign-in

1. **Before it ships**, the group's admin sets the box up as today, but
   **unclaimed**: it's in the group and tested, and its owner is the
   admin until it's claimed. Setting it up makes a claim code, which goes
   to the shop with the box's name.
2. **The order email** has a link with the code, such as
   `https://claim.withcare.co.za/` and the code. The code works once and
   for 30 days. A new one can be made if it's lost.
3. **They plug the box in.** It reaches the group over Yggdrasil from any
   network. Nothing changes on their router.
4. **They open the link** on a phone or laptop, anywhere. The web nodes
   find the box by the code and pass the page through to it, as they do
   for `/files` today. If the box isn't on yet, the page says so and
   waits.
5. **On that page they:**
   - give their name;
   - choose their domain: one they have, a new one (bought with the box
     or later), or for now a name under the group's, such as
     `harbourkitchen.withcare.co.za`;
   - make a passkey (or a password) for signing in;
   - press **This is my box**.
6. **The box becomes theirs.** They're its owner and a member of the
   group, with a ygg balance. Their folder is made, the device they used is
   signed in, and the code stops working.
7. **Their address starts working.** A name under the group's domain
   works once the group's admin approves it. Their own domain works once
   its nameservers point to the group (below); the page shows each step
   as it's done. The box starts the site with a first page.

Files work from step 6. Mail and the website start at step 7.

## Their domain comes first

The domain belongs to the owner, above everything else:

- **It's registered in their name**, as the registrant, with their own
  login at the registrar if they want one. A new domain bought through the
  shop is registered for them, not for LocalBox or the group, and they
  get its transfer code whenever they ask.
- **It's their main address:** their website, their mail
  (`bookings@`, `info@`, a name for each person on the box), and where they
  sign in. A name under the group's domain is only there for people who
  don't have a domain yet, and can stay as a second address.
- **The group only serves it.** Its DNS is on the group's Cloudflare
  account so the group's web nodes can serve the site and take its mail,
  but the group never owns it and never keeps it from them.
- **If they leave, or are removed by a vote**, the domain goes with them:
  the page gives them the transfer code and tells them where to point it.
  Their files, mail and site can be taken out first
  ([ygg.md](ygg.md#a-member-who-wont-settle)).

| | What they do | What happens |
| --- | --- | --- |
| **A domain they already have** | Type it in. The page shows two nameservers to set at their registrar. | The domain is added to the group's Cloudflare account. Once the nameservers point there, the box publishes the site, and mail to the domain comes to the box. |
| **A new domain** | Search for one, with the box or later, and pay for it in the shop. | It's registered in their name with the group's nameservers already set, and bound to the box the same way. |
| **No domain yet** | Choose a name under the group's domain. | `harbourkitchen.withcare.co.za` and its mail, after the group's admin approves the name. A domain can be added later and made the main address. |

Moving mail for a domain they already use is their choice. The page
warns them first: once the nameservers change, mail for that domain
comes to the box, not to wherever it went before. Before the switch it
lists the domain's current records (mail, and anything else such as a
booking system's), and copies them across, so nothing stops working.

A domain bound to a box can only be published from that box, and its
mail goes only to that box. One box can hold several domains.

## The owner's admin page

The page every owner needs, at their address (`anna.withcare.co.za/admin`)
through the web, and at the box over Yggdrasil for those who have it.

- **Mail:** read, write, search, delete; forwarding to another address.
- **Websites:** their sites, upload or edit, back to an earlier version,
  the contact form and its messages.
- **Domains:** their address under the group's domain, and the domains
  they bind (above).
- **Files:** what's stored, restoring, sharing.
- **People:** others they let use the box, such as family or staff, each
  with a name, limits, suspend and remove (as under People on this box
  today).
- **Devices:** the browsers and computers signed in, and the synced-folder
  installer for a new one.
- **The box:** whether it's online, its disk, what it gives the group and
  uses, and updates.
- **Yggs:** their balance, and what they owe and are owed.

- **Services:** boxes in the group that offer something and chose to be
  listed: suppliers to order from ([suppliers.md](suppliers.md)), and
  later a security firm or carers to send alerts to
  ([pubsub-use-cases.md](pubsub-use-cases.md#building-blocks-the-network-needs)).
  An owner can list their own box here too.
- **Invoices:** send invoices to other boxes and to customers, and keep
  the ones that come in ([suppliers.md](suppliers.md#invoices-between-boxes)).

**What they don't see:** the other boxes, the other members, and anyone
else's names, sites or mail. The admin page shows the group only as
totals ("your files are stored across 12 machines"). The only others
they see are listed services. The group's own admin pages stay on the
admin's machines.

The group's admin can still reach every box, to keep it up to date. The
claim page says so, and so do the shop's terms.

## Safety

- **The code** is long and random, works once, expires, and is tried at
  most a few times an hour per box. Only the box and the shop's order
  know it.
- **Not printed on the box** by default: anyone who handled it on the
  way could claim it first.
- **The admin page through the web** is the part that needs the most
  care. Today a member's dashboard only opens over Yggdrasil, from their
  own devices, because it can do everything to their files. Through the
  web it's signed in with a passkey at `my.withcare.co.za`, and the
  actions that can't be undone (deleting mail or files, removing a person,
  moving a domain) ask for the passkey again.
- **What the web nodes see:** the page passes through Cloudflare and a web
  node, as `/files` does today, so they see it while it's shown. Files
  and mail are still stored sealed for the owner.

## Still to decide

- **Approving names:** whether a name under the group's domain, chosen
  when claiming, is approved automatically (it's checked against names
  already taken) or waits for the group's admin, as every name does today.
  Their own domain needs no approval: owning it is enough, once its
  nameservers point to the group.
- **Passkey or password** first: passkeys haven't been tried with a real
  device yet ([login.md](login.md)).
- **Registering new domains:** which registrar (domains.co.za, as before?),
  and how the yearly renewal is paid: in the shop, or by the owner at the
  registrar.

## Later: boxes sent straight from the supplier

With ITAD sending boxes straight to buyers, nobody sets each box up
first. Every box gets the same image; at first start it makes its own
keys, and the claim code (from the order) is what lets it join the group.
That's a bigger change and comes after the steps below.

## Build order

1. **Unclaimed boxes and claim codes:** `provision-pi.sh` marks the box
   unclaimed and makes its code; `yggstore claim code` makes a new one.
2. **The claim page** through the web nodes, and the handover: owner,
   member, first person, first device, the name request.
3. **The owner's admin page through the web**, starting with mail,
   websites, files and devices; the rest after.
4. **Hiding the group** from owners' pages: only their own box, as
   totals.
5. **The shop:** the code in the order email, and a new code on request.
6. **Domains they already have:** add the zone, copy the current records,
   check the nameservers, publish the site, route the mail. This has to
   work before the first box is sold, since most owners will want their
   own domain.
7. **New domains** through the shop, registered in the owner's name.
8. Test the whole thing on Windows, a Mac and a phone, with a box on
   another network, before anyone buys one.
9. Ordering from suppliers ([suppliers.md](suppliers.md)).
