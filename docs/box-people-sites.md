# Websites and contact forms for people on a box

Status: approved on 2026-10-15 (one name per person, and a removed
person's name goes back to the group's admin). Stages 1 to 6 are built;
stage 7, keys held in the browser, is not. How to use it is in
[hub.md](hub.md#a-website-and-email-address-for-someone-on-the-box).

> **Decided on 2026-10-19: one owner per box.** Each box has one owner
> (`owner` on its entry in the member list, set by the group's admin),
> and its storage counts towards them. The owner is responsible for
> everything on their box: the people they let use it, and those people's
> addresses, email and websites, such as a business's clients. Anyone who
> wants a box of their own gets one; otherwise they share the owner's. The
> owner's box keeps working for its people when it is off (sign-in and
> files through the web nodes, see [web-access.md](web-access.md)). The
> group's admin keeps access to every box.

A box admin can add people to a box, and they use their folder through
`/files` in a browser (see [hub.md](hub.md)). This plan lets one of them
also have a website under the group's domain, such as
`anna.withcare.co.za`, with a contact form whose messages only they can
read. Both admins vet them on the way.

## Who vets what

| Step | Who | What they check |
| --- | --- | --- |
| 1. Add the person to the box | box admin | that they know the person; this already exists (`yggstore hub person add`, or "People on this box" on the box's dashboard) |
| 2. Ask the group for a name for them | box admin | the name suits the person |
| 3. Approve the name | group admin | the same Approve / Decline as members' names today; nothing is approved automatically |
| 4. Turn the contact form on | the person, on `/files` | only on their own site |
| 5. Forward the form to an ordinary email address (optional) | the person asks; the address confirms; the box admin allows | that the address is really theirs and isn't someone else's |

The group admin can take a name back at any time, and web nodes stop
serving that site at once, as they do for members today. The box admin
can suspend or remove the person, which takes their site and form down
with them.

## What gets built

### 1. A sharing key for each person

Today everyone on a box shares the box's sharing key. Each person gets
their own key, made when they are added. Contact messages, and later
"Sent to you", are sealed for that key's code.

**Where the key lives: two stages**

- **Stage 1: on the box,** in the person's entry in hub.json and readable
  only by root. It's simple, and forwarding (part 5) can work. The box's
  owner could read the messages, so the person is told so when they turn
  the form on.
- **Stage 2: only in the person's browser,** wrapped with the key their
  recovery link already carries (the part after `#`). The box keeps
  sealed messages it can't open. Forwarding then can't work, because the
  box can't read what it would forward. So stage 2 is for people who want
  privacy from the box owner more than forwarding.

I'd build stage 1 first.

### 2. Names for people, not only for nodes

A name today belongs to a node's entry in the member list (`names`), with
that node's `code`. People on a box get names too, listed on the box's
entry with each one's own code:

```json
{"name": "localbox-004", "addr": "...", "owner": "peter",
 "people_names": [{"name": "anna.withcare.co.za", "person": "Anna", "code": "ys1..."}]}
```

(Built as `people_names`, see `peers.PersonName`.)

- The box's node publishes the person's site, because web nodes take a name
  only from the node holding it, and that is still the box.
- Contact forms on that site are sealed for the person's code, not the
  box's.
- Mail to `anna@withcare.co.za` is sealed for her code in the same way.
- The name request, approval and decline messages gain a `person` field.
  The group admin's dashboard shows "anna.withcare.co.za for Anna, on
  localbox-004 (peter)", so they know whose box is vouching for her.

### 3. The person's website on `/files`

The `/files` page gets a **Your website** card for a person with an
approved name. It has:
- Publish a folder;
- Edit;
- Back to the previous version;
- **Contact form** on/off;
- Take it down.

These are the same actions the dashboard has, but each one works only on
that person's own name. The box's node does the publishing for them.

### 4. Reading the messages on `/files`

A **Messages** card lists what the contact form brought in. Each message
is opened with the person's key, and there's a reply button that opens
their own mail app with Reply-To filled in.

### 5. Forwarding to an ordinary email address (optional)

1. The person types an address on `/files`.
2. The box sends a 6-digit code to that address, through the group's mail
   relay, from the person's own name.
3. Once they type the code in, the box admin sees "Anna wants her contact
   form forwarded to anna@example.com" and presses Allow.
4. After that, each contact message goes to that address as well as into
   Messages.

This uses the group's mail relay (a web node with `-mail-out`, the desktop
for now), which already lets a node send as the names it holds. It also
needs stage 1 keys, as explained above.

## Limits

- The spam limits stay as they are: 5 messages per visitor every 10
  minutes, 100 per site a day, and the hidden robot field.
- The box admin's storage quota for the person counts their website too.
- Web nodes serve the site, and those are the desktop for now. So the form
  works while a web node is up. That's no change from today.

## Build order, once approved

1. Each person's own key (stage 1), with tests.
2. Names for people: the list field, request and approval with `person`,
   and the admin's view of it.
3. Contact forms and mail sealed for the person's code.
4. The **Your website** card on `/files`, then the **Messages** card.
5. Forwarding, with the confirmation link and the box admin's Allow.
6. Try it end to end on localbox-004 with a test person.
7. Later: stage 2 keys, held in the browser.

## Decided

- One name per person.
- When the box admin removes a person, their name goes back to the group
  admin.
- Forwarding confirms the address with a 6-digit code sent to it. A link
  would only open on a device with Yggdrasil.
