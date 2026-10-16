# Admin handover: localbox-004 becomes the group's admin box

Status: on hold (2026-10-15), at the owner's request. Nothing here is built yet.
Open question from before the hold: the admin box should also be on the
owner's local network, which may change the move and the rehearsal.

Today the desktop is the group's admin. When it is off, nobody can be
invited, the member list can't change, and names can't be given out.
localbox-004 (the HP t630) is always on, so it takes the role over. It also
becomes a storage box with many USB sticks.

## What being admin means today

| What | Where it is now | What happens to it |
| --- | --- | --- |
| `"admin": true` on the list entry | desktop's entry in peers.json | moves to localbox-004's entry |
| `domain` and `ygg_peers` (public peers every node links to) | desktop's entry | moved to localbox-004's entry |
| Open invites | `~/.yggstore/invites.json` (3 open) | used up or cancelled first; each one names the desktop as the node that accepts the join, so they stop working |
| Names given out (`peter.withcare.co.za`, `kath...`) | `~/.yggstore/names.json` | copied to localbox-004 |
| People who joined, as contacts | `~/.yggstore/contacts.json` | copied to localbox-004 |
| Your own names and sharing code | desktop's entry | stay on the desktop's entry; they belong to you, not to the admin |

Not part of the admin role, and staying on the desktop for now: the
group's websites (`-web`, `-web-tunnel`), mail-in and mail-out. They stop
when the desktop is off too. Moving them is a separate later step.

Nodes accept a pushed list only from an admin, and only if the sender stays
an admin in the new list (`peers.Live.Replace`). So the desktop can't hand
over in one push. It takes two: first both are admins, then localbox-004
drops the desktop. Between the two pushes, nobody changes the list.

## Part 1: storage on USB sticks

**One node per stick.** Each stick gets its own ext4 filesystem, label
`yggdata-N`, a mount at `/srv/yggstore/stickN` with `nofail` (so the box
still boots with a stick missing), and its own node on port 7401, 7402 and
so on. `yggstore join` with `-dir` and `-port` already does this, and it
sets `host: localbox-004` on each extra node.

Why not pool the sticks into one big disk: with one node per stick, a dead
stick is one node going down. Repair rebuilds its shards elsewhere and the
other sticks carry on. A pool (LVM, RAID 0) loses everything when any one
stick dies. mergerfs would avoid that, but then the node can't tell which
shards it lost.

**The limit to know before buying sticks.** Every chunk is 6 shards (4
data + 2 parity). No machine gets more than 2 of a chunk's shards, so
losing a whole machine never loses data. All the sticks share
`host: localbox-004`, so together they hold at most a third of whatever
the group stores. Space beyond about half of what all the other machines
offer together will sit empty until the rest of the group grows.

**Hardware.** The t630 has one USB 3 bus with 4 ports and one USB 2 bus.
Use a **powered** USB 3 hub for more than two sticks, because sticks
dropping off for lack of power is what broke pi2. Use real-brand sticks or
USB SSDs. Check new ones with `f3probe`, since fake-capacity sticks are
common. The admin files and the system stay on the internal SSD, never on
a stick.

**To build:** `scripts/add-stick.sh` asks the box which stick to use,
formats it (after showing its size and model and asking first), adds the
fstab line, then joins it as the next node with an invite you make on the
dashboard. This works before or after the handover.

## Part 2: rehearsal at home, with no LAN

Before the move, localbox-004 runs for 48 hours as if it were already
somewhere else:

1. Turn off its LAN links: set `MulticastInterfaces: []` in
   `/etc/yggdrasil/yggdrasil.conf`, and use ufw to block Yggdrasil's port
   from 192.168.0.0/24. It then reaches the group only through the public
   peers in `ygg_peers`.
2. Check from the desktop:
   - every node shows localbox-004 up;
   - its remote dashboard opens over Yggdrasil;
   - a file uploaded from the desktop puts shards on it;
   - a Syncthing user on the hub still syncs.
3. Pull its power and check that it comes back by itself: Yggdrasil, the
   node, the dashboard and the hub (with all sticks mounted, or the missing
   ones shown as down).
4. Set the LAN links back on afterwards, or leave them off if it is moving
   soon.

## Part 3: the handover (one sitting, about 15 minutes)

1. **Freeze.** Use up or cancel the open invites on the desktop's dashboard.
   Don't run joins or name changes until step 6.
2. **Back up** the desktop's peers.json, invites.json, names.json and
   contacts.json to `~/.yggstore/before-handover/`.
3. **Copy** names.json and contacts.json to localbox-004's yggstore home.
4. **Push 1, from the desktop:** localbox-004 gets `admin`, `domain` and
   `ygg_peers`, and the desktop keeps `admin`. Wait until every node's
   dashboard shows that list.
5. **Push 2, from localbox-004:** the desktop loses `admin`, `domain` and
   `ygg_peers`. Wait until every node shows that list.
6. **Check:**
   - a push from the desktop is now refused ("only an admin node may change
     the peer list");
   - make an invite on localbox-004's dashboard and join the `yggjoin` test
     container with it;
   - give the test node a name.
7. The desktop's dashboard drops its admin pages by itself. From now on you
   run the group from localbox-004's dashboard, which opens from your
   devices.

**Undo:**
- Before step 5: the desktop pushes the old list back.
- After step 5: localbox-004 pushes a list with the desktop as admin again,
  and the steps run the other way round.

**To build:** `yggstore admin handover -to localbox-004`, run on the
current admin. It:
- refuses while invites are open;
- makes the backup;
- does both pushes;
- waits after each push until every node reports the new list's hash;
- stops with a clear message if any node doesn't.

Push 2 needs localbox-004 to send it, so the command asks localbox-004's
dashboard to send it, over Yggdrasil. That is a call the desktop's device
is already allowed to make. Steps 1 to 6 by hand also work, so the command
is a convenience, not a must.

## Part 4: the move

Do the handover after the rehearsal, at home, where you can still plug in
a screen. Then move it. While it is on the road, the group keeps storing
and serving files. Only invites and list changes wait until it is back up.

At the new place it needs nothing but power and internet. It finds the
group through the public peers.

## Part 5: standby admins (more than one)

If localbox-004 dies for good, nobody is left who can push a list, and
the group is stuck. So several machines are **standby admins**. The
desktop, the second machine and localbox-003 are the first ones. Put them
in different places where you can, so one power cut or one bad internet
line can't take out the active admin and every standby together.

**How a standby works:**
- It is listed with `admin: true` and a new `standby: true` flag.
- Its dashboard refuses to push anything. The one exception is
  `yggstore admin takeover`, which you run on it by hand.
- The active admin copies invites.json, names.json and contacts.json to
  every standby each night, over Yggdrasil.

**Keeping one live admin with several standbys.** Two standbys could take
over at the same time, for example if you ran takeover on two of them
while the network was split. Pushes don't merge, so that would give two
live admins. A new `term` number in the list prevents it:
- Every list carries a term number.
- A node accepts a push only if its term is higher than the list it
  already has, or if its term is the same and the push comes from that
  list's active admin.
- Takeover raises the term by one, and makes the standby the active
  admin. The old active admin becomes a standby too, so it can come back
  without fighting.
- If two takeovers do race with the same term, nodes keep whichever
  reached them first. Then the standby with the lower name wins: the other
  one sees its takeover refused, and its dashboard says which standby won.
- When a dead admin comes back with an old list, nodes refuse its pushes
  because its term is lower. It picks up the new list like any other
  member.

**Takeover checks first.** Before taking over, it asks the active admin
and the other standbys whether they are reachable. If the active admin
still answers, it refuses unless you add `-force`, because a handover is
the right tool in that case.

The handover in Part 3 changes too:
- push 2 marks the desktop `standby` instead of removing its `admin`;
- both pushes keep the same term, because the active admin sends them.

**To build:**
- the `standby` flag and the `term` number, plus the term rule in
  `peers.Live.Replace`;
- the dashboard refusing to push on a standby;
- `yggstore admin takeover`;
- the nightly copy to every standby.

## Still open

- **The gateway.** Is it the test `gw` container, or a real one for paying
  customers on localbox-004? Leave the test one on the desktop until
  after the move.
- **Websites and mail-in.** Move them to localbox-004 too? That would need
  cloudflared there, which it may need anyway.

## Build order, once approved

1. `scripts/add-stick.sh`, then add the first sticks.
2. Rehearsal (Part 2). There is nothing to build for it.
3. The `standby` flag, the `term` number and refusing pushes on a
   standby. They are needed before the handover, so the desktop can stay
   a standby.
4. `yggstore admin handover`, then the handover itself.
5. The move.
6. `yggstore admin takeover` and the nightly copy to the standbys:
   localbox-003 and the desktop first, then the second machine when it
   arrives.
