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

This step goes through the group's admin for now, and is the least
finished part: someone on a box has one name, and a domain of their own
isn't built for them yet ([the plan](developer-areas.md)). Until it is, the
site at the domain is published from your own node, and the admin sets up
its mail by hand. Send them the domain and who it's for. They:

1. add the domain to the group's Cloudflare account, and give you two
   nameservers;
2. route the domain to the group's web nodes, and its mail to the group.

You change the domain's nameservers at its registrar to those two. Before
you do, list any records the domain still needs, such as mail it keeps
elsewhere: the admin copies them over first, or that mail stops.

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
