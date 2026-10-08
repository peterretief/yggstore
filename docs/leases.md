# Leases

A stub holds the key: without one, a stored item's shards can't be
decrypted. If every copy of a stub is lost, its shards take up space on the
group's nodes for nothing. Leases let a node see that, without being able to
tell who stored what or reading anything.

Leases only **report**. Nothing is deleted because a lease wasn't renewed.

## How it works

- Each stub's key gives a **token**, and the token a **lease ID** (its
  SHA-256). Every version of an item shares a key, so one lease covers its
  whole history.
- Uploading a shard tells the node its lease ID. The node keeps it beside the
  shard (`HASH.leases`), with a record of when it was last renewed
  (`leases/ID`). A shard that more than one item uses carries each one's lease.
- **Renewing** shows the node the token. The node can check it against the
  lease ID but can't make it up from the ID, so a renewal proves someone
  holds the stub: the person who stored it, someone they shared it with,
  or someone who restored the stub from a backup onto a new machine.
- The dashboard renews **once a day**, on each node that is up, for every
  stub it holds: your items, their older versions, your mail and shares
  you've received. This happens as part of the repair pass, and also when
  repair is off (`-repair-after 0`).
- A renewal also tells the node this machine is about. Shards stored
  before leases existed have no lease, so they count as wanted while the
  machine that stored them keeps renewing.

Nodes ignore renewals for leases they don't hold, and keep no record of
them. Each day a node prunes the records of leases whose shards have all
been deleted.

## Checking a node

On the node's machine:

```sh
yggstore leases            # -days 90 by default; -data if not ~/.yggstore/shards
```

```
WRITER   SHARDS  SIZE       UNRENEWED 90d+  LONGEST UNRENEWED  BEFORE LEASES
laptop   412     1.2 GiB    37 (96.0 MiB)   since 2026-04-02   0
desktop  860     844.9 MiB  -               since 2026-10-06   860
```

Per machine that stored shards here: how many shards and how much space,
how much nobody has renewed in `-days`, the longest any shard has gone
unrenewed, and how many shards predate leases. A node counts nothing as
unrenewed from before it started keeping track.

Unrenewed shards usually mean one of these:

- **The machine has been off.** A laptop away for a month, say. Its
  shards are renewed when it's back.
- **Copies repair left behind.** Repair leaves the old copy on a node
  that was down (shares sent earlier may still name it). Once the node is
  back, nobody renews that copy.
- **Deletes that missed a node.** If a node was down when an item was
  deleted, its shards stayed there.
- **A lost stub.** The item can't be read any more.

Before reclaiming anything, check with the others: the person may have
the stub on a machine that is off.
