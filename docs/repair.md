# Repair

Every chunk of a stored item is split into shards with Reed-Solomon codes:
some data shards and some parity shards, on different machines. Any
data-count of them rebuild the chunk, so the item survives losing as many
machines as it has parity shards. The dashboard shows an item as
**degraded** when some of its shards can't be reached right now but enough
can, and **lost** only when too few can.

Degraded is fine for a while: a node that's switched off or rebooting
comes back. But a node that never comes back leaves its chunks one failure
closer to loss. Repair rebuilds those shards on nodes that are up.

## When it runs

The dashboard runs a repair pass every 30 minutes. A shard is rebuilt when:

- its node has been **down for a day** (`-repair-after 24h` on
  `yggstore dashboard`; `0` turns repair off), or
- its node has **left the peer list**, or
- its node is **up but no longer has it** (a wiped or replaced disk).

Each rebuilt shard goes to a node the chunk doesn't use yet, on a machine
below its share of the chunk, fast nodes before slow ones. The stubs (and
older versions sharing the chunk) are rewritten to point there. The
dashboard's activity list says `repair: rebuilt N shard(s)`.

Repair works on the encrypted shards and needs no keys. A rebuilt shard is
the original byte for byte, so its hash and `verify` challenges still hold.
The old copy is left on the node that went away: if that node comes back,
shares you sent earlier, whose stubs still name it, keep working.

If more than half of the machines don't answer, repair does nothing: it is
then more likely this machine's own network that is down.

It looks after your own items (outfiles, their older versions and your
mail). Items shared with you are the sender's to repair.

## Retiring a node now

To take a node out for good without waiting a day, remove it from the peer
list (the dashboard's admin page, or `peers.json` and a push). The next pass
rebuilds its shards elsewhere. Or, with the node switched off:

```sh
yggstore repair -after 0
```

`-after 0` takes every node that doesn't answer right now as gone, so only
use it when the nodes that are down are meant to be.

## How items are split

New items are split according to how many separate machines are online
(nodes with the same `host` in the peer list are one machine):

| Machines | Layout | Survives losing | Space used |
|---|---|---|---|
| 1-3 | 4+2 | 1 machine | 1.5x |
| 4 | 2+2 | any 2 machines | 2x |
| 5 | 3+2 | any 2 machines | 1.67x |
| 6-8 | 4+2 | any 2 machines | 1.5x |
| 9+ | 6+3 | any 3 machines | 1.5x |

An item keeps its layout: a new version is split like the old one (so
unchanged chunks are reused), and repair rebuilds shards in the item's own
layout. Items stored before this used 4+2 (very old ones 2+1).
