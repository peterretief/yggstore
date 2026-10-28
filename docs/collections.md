# Collections: films, books and music shared within the group

Status: proposed 2026-10-28. Nothing is built.

A collection is a library kept in the group, such as "Films", "Books" or
"Music", that a chosen list of members can browse, play and add to. Each
item is stored once, however many people can see it.

## What exists already

- **Every file is stored once**, encrypted and in pieces across the
  members' machines. Its key is in its stub.
- **Sharing one item** with one person: a `.ysend` file seals the item's
  stub for their sharing code ([README](../README.md#sharing)).
- **Team folders** on a box: several people on that box sync one folder
  ([hub.md](hub.md#team-folders)). They only work for people on the same
  box.

A collection works across the whole group, for any number of people.

## How it works

- **A collection has its own key.** Its index (the list of items, with
  each item's stub) is stored in the group like any other file, encrypted
  with that key.
- **The key is sealed for each person on the list**, for their sharing
  code, the same way a `.ysend` is. Adding someone means sealing the key
  for them. Nothing is uploaded again.
- **The owner decides who can add items:** only themselves, or everyone
  on the list. Each item records who added it.
- **Nodes still see only encrypted pieces.** A node that holds a film's
  pieces can't tell it's a film, or which collection it belongs to.

### Taking someone off the list

The collection gets a new key, sealed for everyone still on the list, and
new items use it. The person taken off no longer sees the index or
anything added afterwards. What they already downloaded, or could have
copied while they had the key, can't be taken back. The page says so when
you remove someone.

## Using a collection

- **In the browser** (the dashboard, or a box's `/files` page): a page per
  collection with covers, titles and who added what. Films and music play
  in the page. Files are stored in 4 MiB chunks, so jumping to the middle
  of a film fetches only the chunks around that point. Books open in the
  browser or download.
- **In the synced folder:** `yggstore/collections/NAME/` lists every item,
  and an item is fetched only when it's opened, so a 2 TB library doesn't
  fill a laptop's disk.
- **On a phone:** through the browser page.

## Yggs

- **Storage:** the collection's owner pays for what it takes up, like their
  own files ([ygg.md](ygg.md)). Members who add a lot could be asked to
  share the cost; the owner chooses.
- **Rewards:** a ygg button on each item, as on members' web pages. A
  reward goes to whoever the item names as its maker, or to the member who
  added it if it names nobody.

## Copyright, and paying makers in yggs

Your own recordings, writing and photos, and anything under a free licence
(Creative Commons, public domain, many lectures and audiobooks), can go in
a collection for anyone.

Films, music and books bought from a shop are usually licensed for one
household. Putting them in a collection that other members can see is
copying them to those members, which the licence doesn't allow, even in a
private group. Paying yggs for them doesn't change that on its own: only
the person who holds the rights can give permission.

Yggs can pay makers who agree to it, in very small amounts. So
collections support items with **the group's standard terms**, which a
maker agrees to once:

- **Each play or download pays the maker a very small amount,** in the
  spirit of the ygg button: 1 ygg (a tenth of a US cent) a play, say, and
  a little more for a download. Each play costs the listener almost
  nothing, and the maker is paid for every one.
- **The maker agrees to the terms** for their work: by putting it in
  themselves, or by telling the group they agree to it being there. Without
  that, a bought film, album or book stays out.
- **A maker may set their own price instead**, per play, per download or
  once per member. Local musicians, writers and film-makers can sell to the
  group this way, without a shop in between.
- **The price is charged when a member plays or downloads it**, and goes to
  the maker through the ledger: netted if they're a member, paid to their
  wallet if they're not ([ygg.md](ygg.md#inside-the-group-and-outside-it)).
- **The item shows its terms:** whose it is, the price, and that the maker
  agreed.
- **For other items**, the person adding one confirms it's their own work,
  freely licensed, or shared with the maker's agreement. The group's admin
  can take down an item that isn't.

A catalogue from a publisher or label would need an agreement with them.
That's possible later, through the same terms, but it isn't something a
member can set up alone.

## Still to decide

- **Who can make a collection:** any member, or only with the admin's
  agreement.
- **People on a box:** whether they can be on a collection's list, through
  their box.
- **Video formats:** play only what browsers handle (MP4/H.264, WebM), or
  convert other formats on a member's machine ([compute.md](compute.md)).
- **The standard amounts:** how many yggs a play and a download pay; and a
  cap on makers' own prices, so one film can't cost a member thousands of
  yggs by mistake.
- **Take-down:** who decides, and how a maker asks for something to come
  down.

## Build order

1. Collections with their own key, sealed for each member on the list; add
   and remove items and people, from the CLI and the dashboard.
2. The browser page: browse, play films and music with seeking, open books.
3. Collections in the synced folder, fetched on open.
4. Storage in the ledger for collections, and the ygg button on items.
5. Maker terms: the standard small amounts per play and download, makers'
   own prices, and the maker's agreement recorded with each item.
