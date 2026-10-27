# Members of a site: registration and course material

Status: proposed 2026-10-23. Nothing is built.

Kath wants people to register on her site and, once she has let them in,
download her course material. That is the first real use; it's built so any
site in the group can switch it on.

Decided on 2026-10-23: free for now; the site's owner approves
each person; members sign in with their email address and a password.

## What people see

**A visitor:**

1. On `kath.withcare.co.za/register/` they fill in their name, email address
   and a line about themselves, and send it. The page says Kath will look at
   it and they'll get an email.
2. Once Kath lets them in, an email comes: "You can now download the course
   material: choose your password". The link works for a week.
3. They choose a password (at least 10 characters) and are signed in to
   `kath.withcare.co.za/members/`, where the material is.
4. Later they sign in at `/members/` with their email address and password.
   **Forgotten your password?** sends a link to choose a new one.

**Kath:**

1. A registration arrives in her mail, like a contact-form message, and is
   listed under **Members** on her website page, with **Let in** and **Turn
   down**.
2. **Let in** sends the visitor their email. **Turn down** sends nothing.
3. She puts the course material in her site's `members` folder on her files
   page (**Your websites → kath.withcare.co.za → members**), as she adds any
   file now. It's only served to members who are signed in.
4. **Members** lists who has access and when they last signed in. **Remove**
   ends their access at once.

## How it works

- **Switched on per site,** by the group's admin, as the contact form is.
  The site's `site.json` says `"members": true`.
- **Registering** posts to `/_yggstore/register` on the web nodes. It's the
  contact form's path: sealed for the owner's sharing code and delivered to
  their box, with the same limits per visitor and per site. Nothing about
  the visitor stays on the web node.
- **The box keeps the members:** name, email, when they were let in, and
  their password's hash (`internal/password`, as for people on a box). It
  sends the site's members to the web nodes the way it sends its people, so
  signing in and downloading work while the box is off.
- **Choosing a password** is a link with a one-time code, emailed from the
  group's address for the site. The box makes the code and keeps its hash;
  the web node that gets the new password hands it to the box, so the box
  must be on for that step, as with people's passwords.
- **One email at a time.** Asking again for the link sends the same one, at
  most every 10 minutes; the page says when it went and to look in spam.
- **The `members/` folder** is served only with a member's session for that
  site. Everything else on the site stays public. Sessions last 30 days
  unused and are shared between the web nodes; removing a member ends theirs.
- **Wrong passwords:** 5 in a row make that member wait 15 minutes, and one
  address may try 20 sign-ins in 10 minutes, as for people's passwords.

## Personal information

Kath collects names and email addresses, so South Africa's POPIA applies to
her. The registration page says what the details are for (access to her
course material, and her replying), and that she can be asked to delete
them; **Remove** deletes a member's record from her box, and the web nodes
forget them when the box next tells them.

## What it isn't, yet

- **No payments.** A paid subscription later needs a payment provider, and
  a member record that runs out.
- **No per-course access.** Every member sees all of `members/`. Groups of
  members (one per course) can come later.
- **No WebAssembly.** Extra questions on the form, or anything particular to
  her site, come with the handlers ([site-code.md](site-code.md), stage 1).

## Build order

1. **Publishing that can't break a site** ([site-code.md](site-code.md),
   stage 0): her `register/` and `members/` pages go up as new folders
   beside her page, with a preview first and one step back.
2. **Registering and letting in:** `/_yggstore/register`, the registration
   in her mail and under **Members**, **Let in** / **Turn down**.
3. **Choosing a password and signing in:** the emailed link, the sign-in
   page, sessions, the guarded `members/` folder.
4. **Forgotten password, Remove, last signed in.**
5. **Her pages:** `register/` and `members/` from the starter, in her site's
   style, added beside her page.

Each step is tried on a test site, then with Kath on her laptop.
