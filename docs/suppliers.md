# Ordering and invoices between boxes

Status: proposed 2026-10-29. Nothing is built. Comes after
[claim.md](claim.md).

A business with a box can order straight from a supplier that also has
one: a restaurant from its fish supplier, its baker or its wine merchant.
The order goes from box to box over Yggdrasil, with no ordering platform
in between and nothing to pay one.

## What exists already

- **Topics and direct messages** between nodes ([messaging.md](messaging.md)):
  a node that was off gets what it missed, for up to 30 days, and every
  message comes from a known node.
- **Sealing for one node:** a message or a file can be encrypted so that
  only one box can read it, as a `.ysend` is.
- **Storage** for anything bigger than a message, such as photos or a
  price list as a PDF.

Topics can be read by any member, so anything private (an order, a price
agreed with one customer) is sealed for the box it's meant for.

## Orders and invoices are email

Every owner has an email address, so an order or an invoice is an email
from one to the other: a readable message, a PDF, and a small attachment
the receiving box reads (the lines, amounts, dates and order number),
which files it under **Orders** or **Invoices** by itself.

- **Between boxes in the group**, the mail goes straight from one box to
  the other over Yggdrasil, sealed for the receiver (see
  [mail between boxes](#mail-between-boxes)). It never leaves the group.
- **To anyone else**, it's an ordinary email with the PDF. A customer or
  supplier without a box reads it like any other, and can answer it.
- **Ordinary mail works too:** an owner can write to another owner the
  same way, and it takes the same direct path.

So nothing here needs a channel of its own, and the same order or invoice
works with businesses in the group and outside it.

### Mail between boxes

Today mail from one member's address to another's goes out through the
relay to the internet, and comes back in through Cloudflare and a web
node to the other box. Instead, the sending box looks the address up in
the group's list (names under the group's domain, and domains bound to a
box, [claim.md](claim.md)). If a box holds it, the message is sealed for
that box's owner and sent to it as a direct message. If that box is off,
it's delivered when it's back, as messages are.

It's quicker, it isn't limited to 60 an hour, and no relay, Cloudflare or
web node sees it. Mail to addresses outside the group goes out through the
relay as now.

## How it works

1. **A supplier lists themselves**, as a listed service
   ([the building blocks](pubsub-use-cases.md#building-blocks-the-network-needs)).
   On their admin page they turn on **Take orders from the group**, and
   fill in a catalogue: products, units, prices, delivery days, and the
   areas they deliver to. Their box publishes it on the suppliers' topic.
   Turning it off takes them out.
2. **A buyer finds them.** Under **Services** on the buyer's admin page,
   the listed suppliers and their catalogues. Only suppliers who listed
   themselves are shown; nothing else about the group is.
3. **The buyer orders.** They pick quantities and a delivery day, and
   press **Send order**. The order is emailed to the supplier's
   address, straight to their box. If the supplier's box is off, it's
   delivered when the box is back, and the buyer's page shows it as
   waiting.
4. **The supplier answers.** The order shows under **Orders** on their
   admin page and in their mail. They **accept** it (with any
   changes: "no kingklip today, hake instead?") or **decline** it. The
   answer goes back the same way, as a reply.
5. **Delivery and paying.** The supplier marks it delivered, with the
   invoice attached. The buyer pays as they already do, by EFT or on
   account. Both boxes keep the order, the answer and the invoice, stored
   in the group like their other files.

A **standing order** ("every Tuesday and Friday, 10 kg hake") is sent
once and repeats until it's changed.

A supplier can also keep a **price list for one customer**, sealed for
that buyer's box, so other buyers don't see it.

## Invoices between boxes

Any box can send an invoice to any other, with or without an order
before it: a plumber to the restaurant, the restaurant to a client it
catered for, one member to another for work done.

- **Making one:** on the admin page, **Invoices**, **New invoice**: who
  it's to (a listed service, or a box that has dealt with you before),
  the lines, VAT if they're registered, the due date, and how to pay
  (their bank details, or a payment link). The box numbers invoices in
  order and keeps the business's details (name, address, VAT number) so
  each one is a proper tax invoice.
- **Sending it:** emailed to their address, as an order is. In the
  group it goes straight to their box and lands under **Invoices** as
  well as in their mail; if their box is off, it's delivered when it's
  back. A customer outside the group gets an ordinary email with the
  PDF.
- **Answering it:** the receiver marks it **paid** (with the date and
  reference), or **queries** it with a note. The sender sees the answer,
  and can send a reminder once it's overdue.
- **Keeping them:** both boxes keep every invoice, payment note and
  query, stored in the group like their files, so a lost disk loses
  nothing. Each side can download them for the books: a list for a
  month or a year, as a spreadsheet, and the PDFs.
- **An invoice can't be changed once sent.** A mistake is fixed with a
  credit note that points to it, as accounting rules expect.

Invoices from an order (step 5 above) are made the same way, with the
order's lines already filled in.

## Customers who aren't in the group

The restaurant's own customers order or book through its website, as
any site can do today with a form ([sites.md](sites.md#a-contact-form)).
Their messages are sealed for the box and arrive with its mail. This
plan is only for businesses that both have a box.

## Paying

Orders are paid in rands, outside the group, as between any business and
its supplier. Yggs aren't used for this: they count what members give and
use of each other's machines, and aren't money
([ygg.md](ygg.md)). Paying through the group could come later, once
yggs are settled with money and the rules allow it.

## Still to decide

- **What's in a catalogue:** a fixed form (product, unit, price, stock),
  or a page the supplier writes, or both.
- **Who may list:** any member, or only with the group's admin's
  agreement, so the list stays trustworthy.
- **Areas:** whether buyers see only suppliers who deliver to them.
- **Invoices:** whether a box can also take in an invoice made elsewhere
  (a PDF from the supplier's own accounting program) and keep it with the
  rest; and whether to export to accounting programs such as Sage or
  Xero, or only a spreadsheet.
- **Who can be invoiced:** any box that has dealt with you before, or
  only listed services and boxes you've added, so nobody gets invoices
  from strangers in the group.
- **Many buyers:** messaging sends from the publisher to every follower
  itself, which suits tens of nodes. A supplier with hundreds of buyers
  needs the catalogue passed on by other nodes (see messaging.md).

## Build order

1. Mail between boxes: delivered box to box when the address is in the
   group, through the relay otherwise.
2. The suppliers' topic and the catalogue, published from the
   supplier's admin page.
3. **Services** on the buyer's page, and an order sent as an email with
   the attachment the supplier's box reads.
4. Accept, change or decline, as a reply the buyer's box reads.
5. Invoices between any two boxes: make, send, mark paid or query,
   reminders, credit notes, and the download for the books.
6. Delivered, with the invoice made from the order.
7. Standing orders, and price lists for one customer.
8. Try it with one real supplier and one real buyer before anyone else.
