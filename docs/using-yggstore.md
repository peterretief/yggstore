# Using yggstore

## Trust

A group can run on trust, or on what the software enforces either way:
nodes hold only encrypted pieces they can't read, only the node that stored
a piece can delete it, each node sets how much space it lends, storage
challenges check that pieces are still held, members join by invite, and
each node answers only the group.

## How it works

1. `put` splits a file into 4 MiB chunks, encrypts each with AES-256-GCM
   (a random key per file) and erasure-codes it into data and parity
   shards. The split depends on how many separate machines are online:
   **6+4** from 5 machines (any 6 of the 10 shards rebuild a chunk), and
   **2+2** with 2, **4+2** with 3, **2+2** with 4. With 4 or more, any two
   machines can fail; from 10, no machine holds two shards of a chunk and
   any four can fail. With one machine online, an upload is refused.
2. Shards are named by their SHA-256 and spread over the online nodes.
3. A stub, `FILE.ystub`, holds the manifest **and the key**: anyone with it
   and access to the nodes can read the file.
4. `get` fetches shards in parallel, rejects any whose hash is wrong, and
   rebuilds each chunk from enough of them.
5. A node down for a day, or gone from the group, has its shards rebuilt
   elsewhere; one the admin marks as leaving has them moved off first
   ([repair](repair.md)).
6. The uploader keeps 20 single-use challenges per shard
   (`FILE.ystub.challenges.json`, private); `verify` uses one to check a
   node still holds its shard.
7. Each upload leases its shards, renewed daily by whoever holds the stub,
   so a node can see what nobody has wanted for months
   ([leases](leases.md)).

A node's Yggdrasil address comes from its public key, so it identifies the
caller. It is the node ID. Nodes answer only addresses in `peers.json`,
listen only on Yggdrasil (with the [built-in Yggdrasil](builtin.md), not on
the machine's network at all), and let only a shard's writer delete it.

## Running it

Download yggstore from the
[releases](https://github.com/peterretief/yggstore/releases) (Linux on PCs
and Raspberry Pis, Windows, macOS) and check it against `SHA256SUMS`, or
build it with Go 1.24 or later:

```sh
go build -o bin/yggstore ./cmd/yggstore

bin/yggstore id                      # this node's ID and peers.json entry
bin/yggstore serve -peers peers.json # on every machine (port 7400)
bin/yggstore status -peers peers.json
bin/yggstore put -peers peers.json photo.jpg   # -> photo.jpg.ystub
bin/yggstore verify photo.jpg.ystub
bin/yggstore get -o copy.jpg photo.jpg.ystub
bin/yggstore rm photo.jpg.ystub
```

```json
[
  {"name": "desktop", "addr": "[200:1234:5678::1]:7400", "admin": true},
  {"name": "pi",      "addr": "[201:abcd::1]:7400", "slow": true},
  {"name": "t1",      "addr": "[202:ef::1]:7400", "host": "desktop"}
]
```

`"slow"` gets one shard per chunk while others have room. Nodes on one
machine share a `"host"`, so the per-machine limits apply to the machine.

### Joining a group

On an admin dashboard, **Invite someone** makes a one-time invite
(`yggjoin1:…`, valid 7 days) and the message to send, with **Copy** and
**Copy for WhatsApp**. On Linux or macOS, the newcomer pastes its one line
into a terminal:

```sh
curl -fsSL https://get.withcare.co.za/install.sh | sh -s -- 'yggjoin1:…'
```

It downloads yggstore into `~/.local/bin` and checks it, joins (with
yggstore's own Yggdrasil if the machine has none), starts the node and
dashboard at every login (systemd or launchd), and opens the dashboard.
No admin password is needed. With yggstore already installed, the same is
`yggstore join -service yggjoin1:…` (`-name NODE -me NAME -quota GB` to
change the defaults). On Windows, people use a box's network drive and the
web instead; on a server, [a box in Docker](docker-box.md). A machine is in one group; for a
second, run another node with its own folder and port
(`-dir ~/.yggstore-2 -port 7401 -name laptop-b`).

People without an invite apply at `get.DOMAIN/apply/` with their name,
email and a note. The web node passes it to the admin, who sees it under
**Applications** on their dashboard and in their mailbox. **Approve** makes
an invite and emails them the line above; **Decline** emails a short no.
Nobody joins without the admin's yes.

### Adding nodes

The admin node keeps everyone's `peers.json` in step: a change on its
dashboard, or by hand, reaches every node within a minute, without
restarts. **Add a node** on the dashboard walks through a new machine. A
Raspberry Pi is set up from the admin's machine:
`scripts/flash-pi.sh /dev/sdX` writes its card and `scripts/provision-pi.sh`
installs it and joins it ([hub](hub.md#setting-up-a-box-in-the-factory)).
Nodes on old software show "update yggstore" and aren't sent lists.

## Sharing

Each person's dashboard holds their stubs, so it is never shared; the nodes
are. To send an item, add the person under **Sharing → Contacts** with
their sharing code (`ys1…`) and press **Share**: you get a `.ysend` file
only they can open, which they drop in their outbox. If the sender deletes
the item, it's gone for everyone. Your sharing key is
`~/.yggstore/sharing.key`; lose it and people need your new code.

**Give and use** on the dashboard shows what each person (`"owner"` in
peers.json) stores in the group and holds for others. It is accounting
only.

## Outbox folder

```sh
bin/yggstore dashboard -peers peers.json -outfiles outfiles
```

```
outfiles/files/      files and folders put here are stored, each replaced by its .ystub
outfiles/whole/      a folder put here is stored as one item
outfiles/restore/    a .ystub put here is rebuilt into restored/
outfiles/restored/   what was rebuilt, in the folders it had in files/
outfiles/delete/     a .ystub put here is removed from every node
outfiles/received/   items people sent you
outfiles/websites/   your websites, a folder per domain (see sites.md)
```

An item is taken once it has stopped changing; half-copied files are left
alone. The original is deleted only after the stored copy has been read
back and matches. A file put back beside its stub is a new version
([history](history.md)). A restore never overwrites. A deletion waits for
nodes that are down and can't be undone.

## Dashboard

```sh
bin/yggstore dashboard -peers peers.json   # http://127.0.0.1:7480
```

It shows which nodes answer, and for each file whether every shard is there
(**healthy**), enough to rebuild it (**degraded**), or not (**lost**).
**Verify** spends a challenge per shard. It listens on this machine only,
unless you open it to your own devices ([remote dashboard](remote-dashboard.md)).

`scripts/local-demo.sh` runs six nodes on one machine, stores a file, stops
two and restores it. Tests: `go test -race ./...`

## Docker test cluster

```sh
scripts/testnet.sh up      # test nodes t1..t5, each with its own Yggdrasil
docker stop yggtest-t2-1 yggtest-t4-1
scripts/testnet.sh down    # `clean` deletes everything
```

The host firewall must let the bridge in (`sudo ufw allow in on
br-yggtest`). Test files never reach the real nodes. All run on one disk,
so this tests behaviour, not redundancy.

## Limits

- Stubs live where they were made and in the [recovery copy](recovery.md);
  with that turned off, a dead disk leaves files nobody can decrypt.
- Membership is the admin's `peers.json`; there is no vouching yet.
- Repair runs on each member's dashboard for their own files, so they wait
  while it's off. A box repairs for its people.
- Challenges are run by hand and produce no receipts.
- Plain HTTP inside Yggdrasil's encryption; shards are ciphertext anyway.
