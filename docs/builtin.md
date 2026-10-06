# Built-in Yggdrasil

A node can run Yggdrasil inside yggstore instead of using the Yggdrasil
daemon: no separate install, no TUN device, no root, and nothing of the
machine's own network stack on the overlay. The idea comes from
[yggmail](https://github.com/neilalexander/yggmail); how it differs is
below.

    yggstore serve -transport builtin

`yggstore join` uses it by itself on a machine with no Yggdrasil running,
linked through the Yggdrasil peers in the invite.

## How it works

Nodes talk HTTP/3, which is QUIC over UDP. With the built-in Yggdrasil the
UDP datagrams go straight into Yggdrasil's encrypted sessions, wrapped in
the same IPv6 and UDP headers a TUN device would carry. So a node that still
runs the daemon reaches a built-in one with an ordinary UDP socket on its
200::/7 address, and the other way round: a group can mix both.

Node IDs stay Yggdrasil addresses, derived from the node's key as before.
A caller is still known by its address, which must match the key the
packets came from; the TLS inside QUIC is only there because QUIC requires
it.

yggmail (through [yggquic](https://github.com/yggdrasil-network/yggquic))
sends bare QUIC packets addressed by public key. yggstore adds the IPv6 and
UDP headers so built-in nodes and nodes on the daemon can reach each other,
and keeps addresses as node IDs so nothing in a group's files changes. The
cost: as with the daemon, a caller is known by its address, which holds only
part of its key, where yggquic knows the whole key.

Nodes on the daemon keep answering plain HTTP over TCP as well, for nodes
running an older yggstore. When one calls another, it tries both and uses
HTTP/3 if the other node answers it.

## Options (serve)

| Option | Default | |
|---|---|---|
| `-ygg-key` | `~/.yggstore/ygg.key` | The node's Yggdrasil key, made if missing. This sets the node's address. |
| `-ygg-peers` | three public peers | Comma-separated Yggdrasil peers to link to, e.g. `tls://203.0.113.5:9001`. |
| `-ygg-listen` | none | Take links from other Yggdrasil nodes, e.g. `tls://0.0.0.0:9001`. List it as the node's `ygg_listen` in the peer list so members link to it directly (see [mesh](mesh.md)). |
| `-ygg-lan` | on | Find and link to Yggdrasil nodes on the local network. |
| `-ygg-proxy` | `127.0.0.1:7402` | Where the dashboard and commands on this machine reach the group through this node. |

The [mesh](mesh.md) works as with the daemon, without needing the admin
socket.

## The dashboard and commands

They have no Yggdrasil of their own. They find the node's proxy on
`127.0.0.1:7402` (or `$YGGSTORE_PROXY`) and go through it, so to the group
they are this node, as they are with the daemon's TUN device. With no proxy
there they use the daemon, if the machine has one.

## Moving a machine off the daemon, keeping its address

The key can be read from the daemon's config, so the node keeps its address
and nothing in the group changes:

    sudo systemctl disable --now yggdrasil
    yggstore serve -transport builtin -ygg-key /etc/yggdrasil.conf -ygg-peers 'tls://...' ...

Don't run the daemon and a built-in node with the same key at the same time.
The node needs to be able to read the file (or copy the key into
`~/.yggstore/ygg.key` as 128 hex digits, mode 0600). Copy the daemon's
`Peers` into `-ygg-peers`, and its `Listen` into `-ygg-listen` if other
machines link to it.

## Nodes on the daemon: open UDP

A node on the daemon answers HTTP/3 on UDP, on its overlay address and the
same port as TCP (7400). If the machine has a firewall that lets TCP 7400 in
on the Yggdrasil interface, let UDP 7400 in too, or built-in nodes can't
reach it (it can still reach them). With ufw:

    sudo ufw allow in on tun0 to any port 7400 proto udp

(Check the interface name with `ip link`.)
