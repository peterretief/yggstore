# Host your first client

You look after a client's website and email. This guide puts them on the
network: their site at their own domain, their mail, and a sign-in for them
in the browser. It takes an hour or so, most of it waiting for DNS.

## What you need

- A place in the group: an invite from the group's admin, or
  [apply to join](https://get.withcare.co.za/apply/).
- A box, which is where your clients live. It can be a
  [Raspberry Pi](storage-box-guide.md), any machine, or
  [Docker on a server](docker-box.md).
- Access to your client's domain at its registrar.

## 1. Set up your box

Join the group and set the box up from its screen
([setting up a new box](box-setup.md#set-up-a-new-box)). You're now the box's
owner: it holds your clients' folders, and only you run it.

## 2. Add your client

On your box's dashboard, under **People on this box**, add them by name.
Set a limit if you like (`yggstore hub person limit acme 20 3`: 20 GB, 3
devices). The box shows a temporary password: give it to them by phone or
in person, never by email.

Give them a name in the group under **Give them a website and email
address**, such as `acme.withcare.co.za`. It's given at once if it's free,
and they get a first page, a contact form and `acme@withcare.co.za`.

## 3. Build their site

Work on your own machine, and publish when it's ready:

- **A folder:** copy it into their `websites/` folder on the box. A change
  goes online within a minute.
- **A project:** `git push`
  ([publishing with git push](sites.md#publishing-with-git-push)).
- **With code and data:** [build a site app](howto-site-app.md).

Try a change first with a [preview](sites.md#preview-before-going-live),
which you can send your client before it goes live.

## 4. Move their domain

On your box's dashboard, in your client's row, open **Add their own
domain**, type it and press **Ask the group**. When the group's admin
approves it, the domain is on the group's Cloudflare account, its website
is routed to the web nodes, and the row shows two nameservers.

The site is the `websites/` folder of that name in your client's folder,
and only your box can publish it.

Before you change anything at the registrar, tell the admin what the domain
still needs, such as mail kept elsewhere: only the website's records are
added, so anything else stops until it's copied over.

For the domain's mail, ask the admin to press **Take its mail too**. Every
address at the domain then goes to one mailbox, your client's, on their
mail page, and they can send from any address at it. A reply goes from the
address the message came to. They can sign in with any address at the domain
too. Mail is sorted by the address it came to: `info@` lands in a folder
called info, made the first time, up to 20, and contact form messages in
Contact forms. There's no Inbox on the page: All mail shows everything, with
unread counts on each folder. New domains can land in spam at first.

Then set the domain's nameservers at its registrar to those two.
**Is it live?** in the row checks it.

The change takes from a few minutes to a few hours. Then check the site
at its domain, and send it a test message.

## 5. Hand over to your client

Send them where to sign in: `https://my.withcare.co.za`, with the
temporary password you gave them. They choose their own, and then have
their files, mail and website in the browser. Nothing is installed. On
Windows, the box's [network drive](box-setup.md#network-drives) works too.

## What to tell your client

- **If a machine fails,** their site and mail carry on, because everything
  is spread over several members' devices.
- **You can read their mail.** Their key is kept on your box, so its owner
  could. Other members can't: they hold only encrypted pieces.
- **Cloudflare sees mail and pages as they pass through**, as it does for
  any site on it.
- **yggstore is experimental.** Keep a copy of anything they can't lose.

## When something's wrong

See [checking it works](sites.md#checking-it-works) for sites and
[when something goes wrong](mail.md#when-something-goes-wrong) for mail.
On your box, `yggstore hub people` shows each client and what they store.
