# Apps that work by mail: addresses and steps

Status: proposed and built 2026-11-19 (not yet deployed): mail-in (part 1),
steps, tell and remind (part 2), the help desk (part 5, as
`app/testdata/helpdesk.sql`). Not built: replies landing on their row,
signed approvals (part 4), the leave example in the tutorial.

How to turn it on for an app:

1. `yggstore app address NAME LABEL` on the app's machine gives the app
   LABEL@ the group's domain (at once on the admin's machine; otherwise the
   admin approves it under Names), and prints its folder.
2. Start the app with `-mail-in` that folder, and `-mail NAME` for the
   emails steps send.
3. Write `mail-in:`, `step:`, `tell:` and `remind:` in `design.sql`.

An app's emails (its `tell:` notes, reminders and acknowledgements) come
from its own `mail-in:` address, so answers come back to it. Its internal
twin (`help@withcare.internal`, see [mail.md](mail.md)) takes mail only from
inside the group: a `mail-in:` at the internal address is for members by
its nature, and its emails can't reach anyone outside.

Lotus Notes ran whole companies on two things: databases that copied
themselves between servers, and mail that carried documents from one person
to the next. A leave form, a purchase order, a sign-off: each was a
document that arrived in someone's inbox, waited for them, and moved on.

yggstore now has both halves:

| Notes | yggstore |
| --- | --- |
| A database, copied between servers | An app ([app-patterns.md](app-patterns.md)): `design.sql`, its log copied between the app's machines |
| Forms and views | A form for each table, a `CREATE VIEW` for each report |
| Who may read and edit what | `allow:` rules and the admin, staff and client roles |
| Mail between people in the company | Group mail; between members it stays in the group (since 2026-11-19) |
| A doclink in a message | A signed link to a row, like the invite links |
| A mail-in database | **Not yet**: part 1 below |
| A document moving from person to person | **Not yet**: part 2 below |

This plan adds the two missing parts, and signing with the person's own
key, all written in `design.sql` like everything else an app does, so a
developer writes no code for them. A help desk (part 5) puts them together.

## 1. Mail-in: an app has an address

Mail to an app's address becomes a row in one of its tables.

```sql
CREATE TABLE requests (
  -- mail-in: leave@withcare.co.za
  -- allow: staff read, staff write
  id TEXT PRIMARY KEY,
  sender TEXT NOT NULL,          -- from: address
  sender_name TEXT,              -- from: name
  subject TEXT,                  -- mail: subject
  body LONGTEXT,                 -- mail: text
  received TEXT,                 -- mail: received
  member INTEGER                 -- mail: in the group
);
```

- **The address** is a name the admin gives, like a person's, but to the
  app: the app has a sharing key of its own, and mail to its address is
  sealed for that key. The app's machine collects it, as a node collects
  its owner's mail, but into the app instead of a mailbox. Only the app
  can read it; the machine's owner's own mail stays separate.
- **Which machine collects it:** the one the name points to. The row then
  reaches the app's other machines through the log, like any change.
- **Columns say what goes where:** `from: address`, `from: name`,
  `mail: subject`, `mail: text`, `mail: received`. Attachments become the
  row's documents, as if uploaded on the form. Columns without a `mail:` or
  `from:` comment start empty, for staff to fill in.
- **`mail: in the group`** is 1 when the message came from a member through
  the group's own mail, and so certainly from the address it says. Mail
  from outside can claim any From; an app that acts on who sent something
  (a leave request, an approval) should accept only members' mail, with
  `-- mail-in: leave@withcare.co.za, members only`. Outside mail to it is
  then refused and the sender told why.
- **Replies:** the app can answer the sender from its own address, once,
  to say it has the request (optional, `-- mail-in: ... , acknowledge`).

What this needs underneath: the web node marks mail it delivers in the
group (a header inside the sealed message), and strips that header from
mail arriving from outside, so it can't be faked.

## 2. Steps: a row moves from person to person

A table with steps has a `status` column, and says who acts at each step and
who is told.

```sql
CREATE TABLE leave (
  -- allow: staff read own, staff write own
  -- step: draft -> owner submits -> submitted
  -- step: submitted -> role:admin approves -> approved
  -- step: submitted -> role:admin declines -> declined
  -- step: approved -> owner cancels -> cancelled
  -- tell: submitted to role:admin
  -- tell: approved, declined to owner
  id TEXT PRIMARY KEY,
  login TEXT NOT NULL,           -- the person asking (owner)
  first_day TEXT NOT NULL,
  last_day TEXT NOT NULL,
  reason TEXT,
  status TEXT NOT NULL DEFAULT 'draft'
);
```

- **`step: FROM -> WHO ACTION -> TO`**: in status FROM, WHO may press ACTION,
  which moves the row to TO. WHO is `owner` (the row's `login`), a role
  (`role:admin`, `role:staff`), or a column holding an address
  (`column:manager`), so a request can go to a particular person.
- **The form shows the buttons** the person may press, and nothing else:
  the status can't be typed in, and a step not listed can't happen. The
  same check as every other permission decides it.
- **`tell:`** emails people when a row reaches a status: a short note
  ("Anna asks for leave, 2 to 6 December") and a link to the row. They sign
  in as usual and press Approve or Decline there.
- **Each step is kept:** who pressed what, when, with an optional comment.
  The log already holds this; the row's page shows it as its history, and
  a report can list it.
- **Follow-ups:** `-- follow-ups: owner, through:owner, role:staff` puts
  "Add a follow-up" on each row: anyone who may read the row writes on it,
  the thread is kept in its history, and the people named are emailed, not
  the writer. Staff hear who wrote; the customer's side hears what was
  written, not which of the staff wrote it. `through:COLUMN` is the people
  of the record COLUMN points to, such as a sign owner's people.
- **A customer's colleagues:** `client read own through owner` (and `client
  write own through owner`) gives a client every row whose `owner` points
  to a record they belong to (a sign owner they're a person of), not only
  rows they made: so a colleague picks a ticket up.
- **Links that fill in:** a list opened with `?fill.COLUMN=value` carries
  those into its Add, so another page (the signs page's Support button)
  opens a person's tickets with a new one ready for that sign. Signed out,
  the link goes to sign in and then back to it.
- **Reminders:** a row that waits more than the days set
  (`-- remind: submitted after 3 days`) reminds the same people once, not
  every day.

Following the one-email-at-a-time rule: a row sends one note per status it
reaches, never a second, different one, and a reminder repeats the same
link.

## 3. The two together

A request can start either way, on the app's form or by mail:

1. Anna emails `leave@withcare.co.za`, or fills in the form.
2. A row appears in `submitted`; the admins get a note.
3. An admin opens the link, reads it, presses **Approve**.
4. Anna gets "Your leave, 2 to 6 December, is approved", with the link.

Between members, none of these messages leave the group's machines.

## 4. Signed approvals

Notes could sign a document, or a single field, with the person's own key,
so years later anyone could check who approved which version, without
trusting the server or the admin. For contracts, policies, drawings and
anything audited, that's the part that matters.

Members already carry a key that signs: their **passkey** (the fingerprint
or face on their phone or laptop, used for the group's sign-in at
my.withcare.co.za). A passkey signs whatever challenge it's given, so:

```sql
CREATE TABLE contracts (
  -- step: draft -> role:staff sends -> for signing
  -- step: for signing -> column:signer signs -> signed
  -- signed: signs
  ...
);
```

- **`signed: ACTION`** makes that step need the person's passkey. Pressing
  **Sign** asks for the fingerprint, and the passkey signs a fingerprint of
  the row as it stands (every column and its documents' contents), with
  the app, the step and the time.
- **What's kept:** the signature and the passkey's public key, in the log
  with the step. The row's page shows "Signed by Anna, 3 December, on her
  phone", with a tick while the row is unchanged since.
- **Changing a signed row** is allowed only where the steps allow it, and
  the signature then shows as "signed an earlier version", with that
  version kept and viewable. Nothing can be changed under a signature
  quietly.
- **Checking it later:** `yggstore app verify ROW` (or the row's page)
  checks each signature against the public keys the group's sign-in holds.
  A signature can be checked by anyone with the row and the key, on any
  machine; it doesn't rely on the app, the server or the admin being
  honest.
- **Read and accepted:** the same, without a step. A policy's page asks
  each person to sign that they've read this version; a report lists who
  has and who hasn't.

What this needs underneath: apps using the group's sign-in (step 3 of
[login.md](login.md), not built), or the app keeping its own passkeys
for its people. The first is better: one passkey per person, for every app.

## 5. A help desk: the example that uses it all

A help desk is what Notes was used for most, it's useful to every client,
and it exercises every part of this plan.

```sql
CREATE TABLE tickets (
  -- mail-in: support@client.co.za, acknowledge
  -- allow: staff read, staff write
  -- allow: client read own
  -- step: new -> role:staff takes -> open
  -- step: open -> role:staff asks -> waiting
  -- step: waiting -> owner answers -> open
  -- step: open -> role:staff solves -> solved
  -- step: solved -> owner reopens -> open
  -- step: solved -> role:staff closes -> closed
  -- tell: new to role:staff
  -- tell: waiting, solved to owner
  -- remind: new after 1 day
  -- remind: open after 3 days
  id TEXT PRIMARY KEY,
  login TEXT,                    -- from: address (owner)
  subject TEXT NOT NULL,         -- mail: subject
  body LONGTEXT,                 -- mail: text
  received TEXT,                 -- mail: received
  assigned TEXT,                 -- who has it
  priority TEXT,                 -- choices: low, normal, urgent
  status TEXT NOT NULL DEFAULT 'new'
);

CREATE VIEW open_tickets AS     -- allow: staff read
  SELECT assigned, priority, count(*) AS tickets
  FROM tickets WHERE status IN ('new', 'open', 'waiting')
  GROUP BY assigned, priority;
```

1. A customer emails `support@client.co.za`. A ticket appears as **new**,
   the customer gets "We have your message", and staff get a note.
2. Someone presses **Take**; it's theirs, **open**.
3. They need more from the customer: **Ask** sends the question; the ticket
   waits. The customer's reply (or their answer on the site, signed in)
   puts it back to **open**.
4. **Solve** tells the customer; they can **Reopen** for a while, then
   staff **Close** it.
5. A ticket nobody takes for a day, or left open for three, reminds once.
6. **open_tickets** shows who has how many, by priority, with totals and
   a download.

For this, mail-in needs one more thing than part 1: a reply to a ticket's
email must land on that ticket, not make a new one. The app's messages
carry the ticket in their Message-ID, and a reply's In-Reply-To finds it;
the reply becomes a comment on the ticket, and outside senders are matched
to the ticket's own address only.

## An example to build first

Leave requests in the learning app (`learning.withcare.co.za`), with a
section in `TUTORIAL.md`, so the next developer copies a working one. Then
the help desk (part 5), as a starter any client can be given, and a
purchase request that needs two approvals (staff, then admin) to check the
steps are general.

## Build order

1. The web node marks in-group mail, and strips the mark from outside mail.
2. App addresses: the admin gives an app a name; its machine collects the
   mail for the app's key.
3. `mail-in:` into a table, with attachments as documents and
   `members only`.
4. `step:`, the buttons, and the history.
5. `tell:` and `remind:`, through the app's `-mail`.
6. The leave example and the tutorial section.
7. Replies landing on their row, and the help desk starter.
8. Signed approvals, once apps use the group's sign-in (login.md step 3).

Parts 1 to 3 are useful on their own (a contact address whose messages
become rows for staff), and come before steps.

## To decide

- **Signing with a passkey only, or a password too?** A signature is only
  worth what the key is: a passkey stays on the person's device. A password
  "signature" would be just a step with a name on it. Proposed: passkey only
  for `signed:` steps.

- **Approve from the email, without signing in?** Notes needed you signed
  in, and so does this plan: the link opens the row, and the buttons are
  there. A one-press link in the email is quicker, but anyone the mail is
  forwarded to could press it.
- **Reply by mail to act:** answering a note with "approved" could do the
  step. It's what people try first, but a reply is easy to misread. Left
  out of the first version.
- **Who gives an app its address:** the group's admin, as for people's
  names (proposed), or the developer for any address at their own domain.
