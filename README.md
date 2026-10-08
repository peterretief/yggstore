# yggstore

Your files, mail and websites kept among people and organisations who pool
their own machines, instead of on a provider's servers. Each member of the
group runs a node on their own box, linked over the
[Yggdrasil](https://yggdrasil-network.github.io/) overlay with no central
server or coordinator. Together the nodes hold everything for everyone,
encrypted, so nothing depends on one machine and no provider keeps your data.

- **Storage**: each file is encrypted, cut into pieces and spread over
  everyone's machines, so it survives machines failing. The small stub left
  behind holds the key, so sending someone a stub (sealed for them) is a
  way to send them the file.
- **Email** for your own domain, encrypted for you as it arrives, read on
  the dashboard or in your mail program. (It comes in
  through Cloudflare Email Routing and goes out through an SMTP relay;
  neither keeps it.)
- **Messaging** between nodes, **websites** served by several machines at
  once, and an **S3 gateway** for people without a box.

It's open source ([AGPL-3.0](LICENSE)): use it as you want, for
yourselves or commercially, and use as much or as little of it as you need.
If you change it and let others use your version over a network, share your
changes.

**Status: experimental.** The cryptography has not been reviewed by anyone
independent. Don't make it the only copy of anything you can't lose.

- New to this? [Set up a storage box](docs/storage-box-guide.md) (step by
  step, for Windows users with a second-hand mini PC).
- No box? The [gateway](docs/gateway.md) lets people pay a monthly fee and
  use the group's storage from any S3 program (rclone, Cyberduck, Duplicati).
  Members choose whether their box holds customers' data, and earn credit.
- Every version is kept: [history](docs/history.md) lets you restore an
  older version of a file, or a folder as it was on a given day. A new
  version only stores the parts that changed.
- Nodes can [message each other](docs/messaging.md): direct messages, and
  topics any node can publish to and follow, delivered even to nodes that
  were off at the time. Programs can use it too, through a local API.
- No Yggdrasil to install: a node can run it [built in](docs/builtin.md),
  with no daemon, TUN device or root. It talks to nodes on the daemon too.
- The [mesh](docs/mesh.md) keeps members' Yggdrasil linked directly to each
  other, so losing one tunnel or public peer doesn't cut anyone off.
- [Websites](docs/sites.md) can live on the group: publish a folder, and
  several machines serve it, so a site stays up when one of them goes down.
  A site's [contact form](docs/sites.md#a-contact-form) sends to your
  mailbox on the group.
- [Email](docs/mail.md) to your domain can arrive on the group: each message
  is encrypted for you as it comes in and read on your dashboard, where you
  can also write and reply (sent through an SMTP relay the web nodes hold).
  Thunderbird or any mail program can use it too, through a local
  [bridge](docs/mail.md#thunderbird-and-other-mail-programs) (IMAP and SMTP).
- [Leases](docs/leases.md) let a node see which shards nobody has wanted for
  months (a lost stub, a delete that missed a node), without reading them.
- Licence: see [LICENSE](LICENSE). Contributions: [CONTRIBUTING.md](CONTRIBUTING.md).
  Security issues: [SECURITY.md](SECURITY.md).

## An example

A shop, a web developer and an NGO share one network, each with a small box
running a node:

- **The shop** has its website and email on the group. When products or
  prices change, the owner edits the site's files and publishes them again
  (`yggstore site publish`); within seconds every web node serves the new
  version. No hosting provider in between.
- **The developer** hosts several clients' websites and email domains on
  the same network.
- **The NGO** keeps its website and email there too.

Their files are spread across all the boxes, encrypted, so each survives
another box failing and none can read another's. The websites keep serving
when a box goes down.

**Messaging** comes with it. A node publishes to a topic, and every node
following it gets the message, even one that was off at the time. A door
sensor or camera script can publish to `alarms`, and every box following
it hears; a disk filling up, a backup finishing or a site going down can be
messages too. Scripts use the node's local API
([messaging](docs/messaging.md)).

Sites are static (HTML, CSS, images, JavaScript), apart from a contact form
whose messages arrive in the owner's mailbox on the group: taking payments
online needs a server of its own for that part. Every member can follow any topic,
so anything private goes as a direct message.

## Trust

A group can run on trust between its members, or rely on what the
software enforces, which holds either way. Nodes hold only encrypted pieces
they can't read, and only the node that stored a piece can delete it. Each
node sets how much space it lends, and storage challenges check that peers
still hold what they were given. Node IDs come from public keys, members
join by invite, and each node only answers the group.

## How it works

1. `put` splits the file into 4 MiB chunks, encrypts each with AES-256-GCM
   (one random key per file, fresh nonce per chunk), and erasure-codes
   (Reed-Solomon) each chunk into data and parity shards. How many depends
   on how many separate machines are online ([repair](docs/repair.md)):
   **2+2** with 2, **4+2** with 3, **2+2** with 4, **3+2** with 5, **4+2**
   with 6-8 and **6+3** from 9, so that with 4 or more no machine holds two
   shards of a chunk. An upload that couldn't survive losing a machine
   (one machine online) is refused.
2. Shards are content-addressed (filename = SHA-256) and spread over the
   online peers, as few per machine as the machines allow.
3. A stub `FILE.ystub` holds the manifest, **including the key**. Anyone with
   the stub and access to the peers can read the file.
4. `get` fetches shards in parallel, rejects any whose hash doesn't match,
   and rebuilds each chunk from any data-count of its shards (4 of 6 for
   4+2).
5. Shards on a node that has been down for a day, or has left the group,
   are rebuilt on nodes that are up, and chunks stored while machines were
   down are spread onto them when they return ([repair](docs/repair.md)).
6. At upload time, while it still has the shards, the uploader precomputes
   20 single-use challenges per shard into `FILE.ystub.challenges.json`
   (keep this private). `verify` sends one per shard: "hash this byte range
   with this nonce", and checks the answer.
7. Each upload tells the nodes a lease for its shards, which anyone
   holding the stub renews once a day. A node can see which shards nobody
   has wanted for months ([leases](docs/leases.md)); nothing is deleted
   for it.

## Identity and access

A Yggdrasil address (200::/7) is derived from the node's public key, so the
source address of a connection over the overlay identifies the caller. The
address is the node ID. Each server only answers callers whose address is in
`peers.json` (default deny), binds only to its overlay address (or, with the
[built-in Yggdrasil](docs/builtin.md), has nothing on the machine's network
at all), and only lets the original writer delete a shard.

## Usage

Download yggstore for your system from the
[releases](https://github.com/peterretief/yggstore/releases) page (Linux on
PCs, Raspberry Pis and arm64 boards, Windows, macOS), and check it against
`SHA256SUMS`. Or build it yourself with Go 1.24 or later:

```sh
go build -o bin/yggstore ./cmd/yggstore

bin/yggstore id                      # prints your node ID and peers.json entry
# collect one entry from every machine into peers.json, copy it to each one
bin/yggstore serve -peers peers.json # on every machine (port 7400 by default)

bin/yggstore status -peers peers.json
bin/yggstore put    -peers peers.json photo.jpg   # -> photo.jpg.ystub
bin/yggstore verify photo.jpg.ystub
bin/yggstore get -o copy.jpg photo.jpg.ystub
bin/yggstore rm photo.jpg.ystub                   # delete its shards
```

`peers.json`:

```json
[
  {"name": "desktop", "addr": "[200:1234:5678::1]:7400", "admin": true},
  {"name": "pi",      "addr": "[201:abcd::1]:7400", "slow": true}
]
```

Each chunk is 6 shards, and no node gets more than 2 of them, so losing any
one node never loses data. A node marked `"slow": true` gets one shard per
chunk while the others have room for the rest, so it doesn't hold uploads
back. Only the uploading machine's peers.json decides this.

Nodes that run on the same machine (Docker containers, say) should share a
`"host"` tag, e.g. `{"name": "t1", "addr": "...", "host": "desktop"}`. The
2-shard limit then applies per machine, and uploads need 3 machines online,
not just 3 nodes.

### Joining a group

On an admin dashboard, open **Invite someone**, type their name and press
**Make invite**. You get a message to send them, with a one-time invite
(`yggjoin1:…`, valid 7 days). They need Yggdrasil running and connected
(the message lists public peers), and the `yggstore` program; then:

```sh
yggstore join -service yggjoin1:…        # -name NODE -me NAME -quota GB to change the defaults
```

`join` asks the admin node to add this machine (over Yggdrasil, presenting the
invite), saves the member list, adds the inviter as a contact, and with
`-service` starts the node and dashboard as user services. On the admin side
the newcomer is added to peers.json (owner: their name) and to your contacts,
and the list sync tells every node. An invite works once; the admin node
only keeps a hash of it. A node on a machine that is already a member is
tagged with that machine automatically; otherwise tag it with `"host"` by
hand if you know two nodes share a box.

The join endpoint is the only call a non-member can make, and it does
nothing without a valid invite (rate-limited to 6 tries a minute).

### Adding nodes

An `"admin": true` node (the desktop) keeps every node's list in step. Each
node reports which list it holds; the dashboard sends its own peers.json to any
node that holds another one, so a node added on the dashboard, or a hand edit
of the desktop's peers.json, reaches every node within a minute, with no
restarts. A node keeps a pushed list in its data folder
(`peers.pushed.json`), and uses whichever of that and its own `-peers` file
changed last. Only admins may push, and a push must keep the sender admin.

To add a machine: open **Add a node** on the dashboard and follow the steps.
In short, the new machine starts with a peers.json holding just the desktop's
entry, runs `yggstore serve`, and you paste what `yggstore id -name NAME`
prints. New uploads use it straight away; files already stored stay put.

Nodes on older software show "update yggstore" on the dashboard and are not
sent lists. Install the new binary on them and restart them; a node that has
never been sent a list also needs `"admin": true` on the admin node's entry in
its own peers.json, so that it accepts lists from it.

## Sharing

Every person runs their own node and their own dashboard. The dashboard
listens on localhost only and holds your stubs, which carry the keys to your
files, so it is never shared. What people share is the network of nodes.

To send someone an item, add them under **Sharing → Contacts** with their
sharing code (`ys1…`, shown on their dashboard), then press **Share** on the
item. You get a `.ysend` file to send by email or any other way. Only that
person can open it; the item's name, your note and its key are sealed inside,
and opening it proves which sharing code sent it. They drop it into their
`outfiles`, and it appears under **Shared with you**, where they can restore
it. They need a node in this network, since nodes serve shards to members
only.

- A received item is still yours to lose: if the sender deletes it, it is
  gone for everyone they shared it with. **Remove** on a received item only
  forgets it on your side.
- Received stubs may only point at Yggdrasil addresses or nodes in your own
  peers.json, so a crafted file cannot make your restore contact other
  machines.
- Your private sharing key is `~/.yggstore/sharing.key` (contacts in
  `~/.yggstore/contacts.json`). Losing it means you can't open what people
  send you any more, and they need your new code.
- The dashboard refuses actions without its own `X-Yggstore` header, and
  requests that name another host, so other web pages in your browser
  cannot press its buttons.

### Give and use

Each node reports how many bytes each uploader has stored on it. The dashboard
totals this per person (set with `"owner"` in peers.json; defaults to the
machine): what they use across the network, and what their nodes hold for
others. The aim is to hold about 1.5× what you use. For now it is accounting
only; nothing is enforced.

## Outbox folder

```sh
bin/yggstore dashboard -peers peers.json -outfiles outfiles   # dashboard + watcher
bin/yggstore watch     -peers peers.json -dir outfiles        # watcher only, logs to stdout
```

```
outfiles/            drop files in, or folders of them → each file is sharded, then
                     replaced by NAME.ystub beside it, so the folder tree stays
outfiles/received/   items people sent you (drop their .ysend anywhere in outfiles)
outfiles/whole/      drop a file or folder in → stored as one item (a folder becomes
                     one archive, restored as a whole)
outfiles/restore/    drop any .ystub in → rebuilt into restored/
outfiles/restored/   restored files and folders
outfiles/delete/     drop a .ystub in → the item is removed from every node, then its stub
outfiles/.yggstore/  stubs that have been restored (kept for reference)
```

- An item is picked up once it has looked the same on two polls (5 s apart)
  and was last changed more than 3 s ago, so half-copied items are left alone.
  Names ending in `.part`, `.crdownload`, `.tmp` and the like are ignored.
- In folders, every file is its own item: it can be restored or deleted on its
  own, and new files added to the folder later are picked up too. Folders in
  `whole/` are streamed as a tar archive and stored as one item. On restore,
  only regular files and folders are written, and paths that would escape the
  destination are rejected. Symlinks are skipped.
- A file put back next to its own stub (say, restored, edited and moved back)
  is stored as a new version, and the stub then points to it; older versions
  are kept (see [history](docs/history.md)). An unchanged copy isn't stored
  again.
- At least 3 machines must be online, so that no machine holds more than 2 of
  a chunk's 6 shards. Otherwise the item stays put and is retried every 2 minutes.
- After uploading, the watcher downloads the item again and compares SHA-256
  hashes. **Only then is the original deleted** (`-keep` keeps it anyway).
- Each stub's challenges are kept in a hidden `.NAME.ystub.challenges.json`
  next to it.
- Deleting (the `delete/` folder, the dashboard's Delete button, or
  `yggstore rm STUB`) removes every shard first. Only when all are gone are the
  stubs and challenges removed; if a node is down, the stub stays in `delete/`
  and is retried every 2 minutes, so no shard is left without a record.
  Deleting cannot be undone.
- The dashboard's Restore button rebuilds any listed stub into `restored/`.
  A restore never overwrites: a second copy becomes `name (2).ext`.

## Dashboard

```sh
bin/yggstore dashboard -peers peers.json -stubs stubs   # http://127.0.0.1:7480
```

Every 3 seconds the dashboard asks each peer for `/v1/info` (up/down, round
trip, shard count, disk use) and checks, for every `.ystub` under `-stubs`,
which shards their holders still have (`HEAD /shards/{hash}`, nothing is
downloaded). A file is **healthy** when every chunk has 6/6 shards,
**degraded** below that while 4 remain, and **lost** below 4. The Verify
button spends one stored challenge per shard. The activity log records nodes
going up or down, shard counts changing and file status changes. It listens on
localhost only by default.

To try it on one machine: `scripts/local-demo.sh` runs six nodes on
different ports of your Yggdrasil address, stores a file, stops two nodes and
restores it. `scripts/local-demo.sh loopback` does the same on `::1`.

Tests: `go test -race ./...`

## Docker test cluster

```sh
scripts/testnet.sh up      # five test nodes t1..t5 in Docker, each with its own Yggdrasil
scripts/testnet.sh status
bin/yggstore put -peers testnet/shared/peers.json FILE
docker stop yggtest-t2-1 yggtest-t4-1   # simulate two nodes failing
scripts/testnet.sh down    # stop (keys and shards kept); `clean` deletes everything
```

The containers sit on their own bridge (`br-yggtest`) and find this machine's
Yggdrasil by link-local multicast, as LAN nodes do. The host firewall must let
the bridge in (`sudo ufw allow in on br-yggtest`). The test peer list is the
desktop plus t1..t5, so test files never land on the real nodes. All six share
one machine and disk: this tests behaviour, not real redundancy.

## Limits of this proof of concept

- No machine holds more than 2 of a chunk's 6 shards, so any one machine
  can fail. Surviving two failures at once needs 6 or more machines.
- Stubs live only where they were made. If that machine's disk dies, the
  shards are still in the group but nobody can decrypt them: back up your
  stubs and `sharing.key`.
- Membership is the admin's `peers.json`, pushed to every node; there is
  no gossip or vouching yet.
- No repair: if a peer disappears, nothing re-creates its shards.
- Challenges are run by hand, are not signed and produce no receipts.
- Plain HTTP; confidentiality in transit comes from Yggdrasil's end-to-end
  encryption, and shards are ciphertext anyway.
- Chunks are buffered in memory (4 MiB plus shards), not streamed.
- The Tailscale transport from the plan is not implemented; `transport.Transport`
  is the seam for it.
