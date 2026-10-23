# Moving a person to another box

Status: **parked** on 2026-10-19, not to be built. With one owner per box
(see [box-people-sites.md](box-people-sites.md)), people stay on their
owner's box, and moving one is rare. Tim, the case it was written for, was
removed from localbox-004 and added to localmail afresh instead, as he had
no real data yet. Websites still move on their own ([move-site.md](move-site.md)).
Kept in case a real need comes up. The rest of this page is the proposal
as written. It builds on
[move-site.md](move-site.md): the same steps, for everything a person has
on a box rather than one website.

## Why

Tim uses the group through a box. His account is on localbox-004, at
Peter's place: his address tim@group.example and its mail, his own site
tim.withcare.co.za, the test site messagemate-test, his passkey and his
files. But the box at his office is localmail, and that's where his laptop
will sync.

People change offices, a box breaks or is retired, and a household moves
house. Each time, the person should carry on as they were, on another box,
without starting again.

## What moves, and what doesn't

Everything that is the person's moves together:

| What | Where it is on the old box | On the new box |
|---|---|---|
| Their record: name, limits, email for Work from anywhere | the hub's state | the same, under the name the new box gives them |
| Their sharing key | `keys/FOLDER.key` | the same key: their mail and contact forms stay sealed for it |
| Their address and sites, in the member list | the old box's `people_names` | the new box's `people_names`, in one push |
| Their mail | `people/FOLDER/mail` (stubs, folders, flags) | the same: each message is already stored in the group |
| Their sites, with every version | `people/FOLDER/sites` | the same, announced from the new box ([move-site.md](move-site.md)) |
| Their forwarding | the hub's state | the same: confirmed addresses stay confirmed |
| Their files | their folder's stubs and history (`users/NAME`) | a folder with the **same Syncthing folder ID**, filled with the same stubs |
| Their passkeys | the hub's state (public halves) | the same keys, so they sign in as before |
| Their devices: Syncthing IDs and the addresses that open `/files` | the hub's state | the same, already let in |

What stays:

- **Team folders** belong to their box. The person leaves the old box's
  teams; the move lists them first, so the admin can add the person to
  teams on the new box.
- **Invites** not yet used are cancelled. The new box makes new ones if
  needed.
- **Web sessions** end: they sign in again once, with the same passkey.

## What the admin sees

The admin's dashboard gets a **People** page, beside **Websites**: each
box and the people on it, with their address, sites, devices and how much
they store. Each person has **Move**.

**Move** asks for the box, and the name they'll have there (the same by
default). Then it shows what will happen:

> **Tim** moves from **localbox-004** to **localmail**, as **Tim**. With
> him go his address tim@group.example and its messages, his sites
> tim.withcare.co.za and messagemate-test.withcare.co.za with their
> versions, his stored files, and his passkey. He's in no teams.
> Nothing he uses goes offline. His devices connect to localmail from its
> network once (the box's one-line command), and pick up the same folder.
> He signs in on the web again once. Move him?

The row shows **Moving…**, then the new box.

Only the group's admin moves people: it changes two boxes.

## How it moves

The same pattern as moving a site, with the person's whole bundle:

1. **The old box holds still.** The admin sends `person-move` to both
   boxes. The old box stops changing the person's files: new files in their
   folder wait, like a place that doesn't hold the working copy
   ([recovery](recovery.md)). Their sites, mail and sign-in carry on.
2. **The new box asks.** It answers with `person-want`, carrying its own
   sharing code: the bundle includes the person's key, so it is sealed for
   the box, not for the person.
3. **The old box hands over.** It gathers the bundle: the record, the key,
   the mail's stubs, each site's versions (as in a site move), the folder's
   stubs and history, the passkeys and the devices. It stores it in the
   group as one item, and sends its stub sealed for the new box
   (`person-handover`).
4. **The new box takes it.** It files everything under the person, gives
   them a folder with the same Syncthing folder ID, and renews the leases
   of everything it now holds. It tells the admin (`person-ready`).
5. **The names change hands.** The admin moves the person's address and
   sites from the old box's `people_names` to the new box's, with the same
   sharing code, and pushes the list. From then on the web nodes deliver
   their mail and contact forms to the new box.
6. **The new box takes over.** Once the list gives it the names, it
   announces the sites, tells the web nodes about the person (below), and
   claims their working copy (`holding`, on their workcopy topic). It sends
   `person-moved` to the old box and the admin.
7. **The old box lets go.** It unshares the person's folder and unpairs
   their devices from its Syncthing, takes them off its state, and keeps
   everything in `people/.moved/NAME-DATE/` for 30 days. Its page shows
   "Tim moved to localmail on 19 October".

Nothing goes offline in between: the sites are served throughout (as in a
site move), and mail waits safely wherever it arrives.

### Mail that arrives during the move

Until step 5, mail for the person still goes to the old box. Anything that
arrives after the bundle was made goes on to the new box as a small
`person-late` handover (the messages' stubs, sealed for the new box). The
old box keeps the person's key for the 30 days, so it can open what still
arrives and send it on.

### Signing in

A passkey names the box and the person it was made for (its user handle,
`localbox-004` and `Tim`), and web nodes send a sign-in to that box. So the
new box tells the web nodes, with the person, what they were before:

```json
{"name": "Tim", "keys": […], "was": [{"box": "localbox-004", "person": "Tim"}]}
```

A web node sends a sign-in for a box and person that some box lists under
`was` to that box instead. Nothing changes for the person: same passkey,
same addresses (`my.withcare.co.za`, `tim.withcare.co.za`). Their old
sessions end, and they sign in again once.

### Their devices

- **Syncthing:** the new box's folder for them has the same folder ID as
  before, and already lists their devices. Each device needs the new box
  added once, by running the new box's connect command on its network.
  The command sees the folder it already has, shares it with the new box
  instead of the old, and nothing is copied twice.
- **`/files` over Yggdrasil:** the devices' addresses move with the
  person. The new box lets them through its firewall, as for any device
  added there.

## Build order

1. **The person's bundle**: export and import of the record, key, mail,
   sites, folder and devices, with tests that a moved person's mail opens,
   their sites roll back and their stubs restore.
2. **The move**: `person-move` to `person-moved` and `person-late`, on
   the sitemove agent, with the old box holding still and letting go.
3. **The names in one push**: a person's address and sites, from box to
   box.
4. **Signing in**: `was` in the box's people, and web nodes routing by it.
5. **Devices**: the connect command re-pointing a moved folder; the
   firewall letting moved devices in.
6. **The admin's People page** and **Move**.

**First use:** Tim from localbox-004 to localmail. localmail has an empty
person called Tim, added on 2026-10-19 for the site move: a move takes the
place of a person with that name who has nothing yet (no files, devices,
names or keys), and refuses otherwise.

## Open questions

1. **Devices away from the new box.** As designed, a device connects to
   the new box from the new box's network once, like any device. The new
   box already knows the moved devices, so it could accept them from
   anywhere, through Syncthing's relays. Should it?
2. **Telling the person.** Should the move email them (the address they
   gave for Work from anywhere, if any) and say so on their files page?
3. **Boxes with different owners.** Here both boxes are Peter's. When
   they belong to different people, the stored files still count as the
   old box owner's uploads (see [ygg](ygg.md)). Should the new box store
   them again under its owner, or should the count just move?
