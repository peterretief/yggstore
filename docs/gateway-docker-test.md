# Testing the gateway in Docker

A checklist for the first real run of `scripts/gateway-docker.sh` on a VPS
(or another machine with ports 80 and 443 open to the internet). Work down
the list and tick each box. Where a result differs from **Expect**, stop and
keep the output of `scripts/gateway-docker.sh logs` for that step.

Allow about an hour, plus a check the next day (part 8), or 2 hours later
if you use a short trial (see 4.1).

- [Before you start](#before-you-start)
- [1. Setup](#1-setup)
- [2. Mark it as the gateway](#2-mark-it-as-the-gateway)
- [3. HTTPS and status](#3-https-and-status)
- [4. A trial customer and the key link](#4-a-trial-customer-and-the-key-link)
- [5. Upload and download from another network](#5-upload-and-download-from-another-network)
- [6. Suspend, resume, report](#6-suspend-resume-report)
- [7. Restart, rebuild, backup](#7-restart-rebuild-backup)
- [8. Next day: the trial ends](#8-next-day-the-trial-ends)
- [9. Clean up](#9-clean-up)

---

## Before you start

| What | Check |
|---|---|
| **A machine** with a public IP: a small VPS (1 CPU, 1 GB RAM is enough) running Linux | `ssh` into it works |
| **Docker** on it | `docker compose version` prints a version |
| **Ports 80 and 443** open to the internet in the VPS provider's firewall, and in `ufw` if it's on | `sudo ufw status` lists 80 and 443, or ufw is inactive |
| **A domain name** for the gateway, e.g. `s3.yourgroup.example`, with an `A` record pointing at the VPS's IP | `dig +short s3.yourgroup.example` prints the VPS's IP |
| **The yggstore source** on the VPS. The repo is private, so either log in to GitHub there (`gh auth login`) or copy it from the desktop: `rsync -a --exclude testnet --exclude outfiles --exclude 'peers.json*' --exclude bin YGGSTORE/ vps:yggstore/` | `ls yggstore/scripts/gateway-docker.sh` |
| **A Windows PC on a different network** for part 5 (a laptop on a phone hotspot will do), with [rclone](https://rclone.org/downloads/) | `rclone version` |
| **Your desktop node and dashboard running**, since the desktop is the admin | The dashboard opens at http://127.0.0.1:7480 |

> [!IMPORTANT]
> **Opted-in machines.** An upload needs at least 3 separate machines whose
> owners opted in to customers' data. Right now only the desktop's test
> nodes (t1–t5) are opted in, and they all count as one machine. Until the
> Pis and the office node are updated and opted in, set `MIN_MACHINES=1` in
> `deploy/gateway/.env` (step 1.4). It is for testing only: data stored
> that way doesn't survive the desktop failing.

---

## 1. Setup

1.1. On the desktop dashboard, open **Invite someone** and make an invite for
"gateway". Copy the whole `yggjoin1:…` line.

1.2. On the VPS:

```sh
cd yggstore
scripts/gateway-docker.sh setup
```

Answer: the domain; your name; paste the invite; press Enter for extra
peers.

**Expect:**
- [ ] The build finishes (the first one takes a few minutes).
- [ ] "waiting for the gateway to join the group…", then a status list that
  includes the desktop as `UP` and **gateway** as `UP`.
- [ ] A message asking you to mark the entry as the gateway, with its
  address (`[2xx:…]:7400`). Copy that address.

If it says "Not in the group yet", run `scripts/gateway-docker.sh logs gateway`:

| Log says | Meaning |
|---|---|
| `warning: no Yggdrasil peers` | The invite has no Yggdrasil peers. Put some in `YGG_PEERS` in `deploy/gateway/.env`, then `scripts/gateway-docker.sh up` |
| `could not reach the group` (repeating) | Yggdrasil on the VPS can't reach the desktop. Check the peers; give it a couple of minutes |
| `the group did not accept this machine` | The invite was used or expired. Make a new one, put it in `deploy/gateway/data/invite` (`sudo` needed), then `up` |

1.3. On the desktop, check that the invite now shows as used in the
dashboard, and that a new **gateway** entry is in `peers.json`.

- [ ] Invite used, entry present.

1.4. *(While fewer than 3 machines are opted in.)* On the VPS, edit
`deploy/gateway/.env`, set `MIN_MACHINES=1`, then:

```sh
scripts/gateway-docker.sh up
```

- [ ] Done.

---

## 2. Mark it as the gateway

2.1. On the desktop, open `peers.json` and add `"gateway": true` to the
gateway's entry:

```json
{"name": "gateway", "addr": "[2xx:…]:7400", "owner": "Your name", "gateway": true}
```

2.2. Wait a minute (the dashboard sends the list to every node), then on
the VPS:

```sh
scripts/gateway-docker.sh logs gateway
```

**Expect:**
- [ ] Within about a minute and a half: `gateway serving S3 on http://0.0.0.0:9000`.
- [ ] Before it: `the gateway stopped; trying again in 30 seconds` lines.
  That's normal until the change arrives.

Press Ctrl+C to stop following the logs; the gateway keeps running.

---

## 3. HTTPS and status

3.1. On the VPS:

```sh
scripts/gateway-docker.sh status
```

**Expect:**
- [ ] `HTTPS works: customers use https://s3.yourgroup.example`
- [ ] No "mark this entry" message any more.

If it says the certificate isn't valid yet, see
`scripts/gateway-docker.sh logs caddy`. The usual cause is DNS not pointing
at the VPS yet, or port 80 blocked (Let's Encrypt checks over port 80).

3.2. From any browser, open `https://s3.yourgroup.example`.

- [ ] A padlock (valid certificate) and an XML "AccessDenied" answer. That
  is correct: nobody without keys gets in.

3.3. The gateway's own port must not be reachable from outside. From the
desktop:

```sh
curl -m 5 http://s3.yourgroup.example:9000/
```

- [ ] It fails (times out or is refused).

---

## 4. A trial customer and the key link

4.1. On the VPS, make a trial that ends tomorrow, so part 8 can check the end
of a trial. (`-trial 2h` also works, for a trial that ends in 2 hours.)

```sh
scripts/gateway-docker.sh customer add -name "Test Customer" -email you@example.com -trial 1d
```

**Expect:**
- [ ] A welcome message with a link `https://s3.yourgroup.example/_keys/…`.
- [ ] An ID in brackets at the end, e.g. `(… customer link abcd1234)`. Note it: it's **WHO** below.

> Commands the gateway prints, like `yggstore gateway customer link …`,
> are run on Docker as `scripts/gateway-docker.sh customer link …`.

4.2. Send the link to yourself on WhatsApp (or paste it into an email), and
let the preview load. Then open the link on the **Windows PC**.

- [ ] The page says "Your storage keys, Test Customer" with a **Show my
  keys** button. The preview didn't use the link up.

4.3. Press **Show my keys**.

- [ ] Endpoint, access key, secret key, "Free trial until" tomorrow, and an
  rclone config. Save it into a text file for part 5.

4.4. Reload the page, or open the link again.

- [ ] "This link was already used, on …".

4.5. On the VPS, `scripts/gateway-docker.sh logs gateway` shows:

- [ ] `key link for Test Customer (WHO) opened from <the PC's public IP>`.
  If it shows a `172.x` address instead, the client address isn't coming
  through Caddy: note it.

---

## 5. Upload and download from another network

Do this on the Windows PC, on a **different network** from the VPS and
from your home (a phone hotspot is ideal).

5.1. Make the rclone config. In PowerShell:

```powershell
rclone config file
```

Open the file it names (create it if it doesn't exist) and paste the config
from the key page. Replace the `password = RUN: …` line with the output of:

```powershell
rclone obscure "a long test passphrase"
```

5.2. Make a test file of about 20 MB and note its hash:

```powershell
fsutil file createnew test20.bin 20000000
Get-FileHash test20.bin
```

> `fsutil` makes a file of zeros. For a more realistic test, use any
> photo or zip of 10–50 MB instead.

5.3. Create a bucket and upload, through the encrypted remote:

```powershell
rclone mkdir group:my-backup
rclone copy test20.bin safe: -P
rclone ls safe:
rclone ls group:my-backup
```

**Expect:**
- [ ] The upload finishes. The speed depends on the slowest opted-in machine.
- [ ] `rclone ls safe:` shows `test20.bin`.
- [ ] `rclone ls group:my-backup` shows the same file under a scrambled
  name in `encrypted/`. The gateway only ever sees encrypted data.

5.4. Download it again and compare:

```powershell
rclone copy safe:test20.bin down\ -P
Get-FileHash down\test20.bin
```

- [ ] The hash matches step 5.2.

5.5. On the desktop dashboard:

- [ ] The gateway node shows as up.
- [ ] The opted-in nodes show a little more space used, as customers' data.

5.6. Quota check. On the VPS, shrink the space to 10 MB:

```sh
scripts/gateway-docker.sh customer quota WHO 0.01
```

On the PC, upload the 20 MB file under another name:

```powershell
rclone copyto test20.bin safe:second.bin
```

- [ ] rclone reports an error instead of storing it.

Put the space back: `scripts/gateway-docker.sh customer quota WHO 5`.

---

## 6. Suspend, resume, report

6.1. On the VPS:

```sh
scripts/gateway-docker.sh customer suspend WHO
```

On the PC:

```powershell
rclone ls safe:
```

- [ ] Refused (access denied).

6.2. Resume, and try again:

```sh
scripts/gateway-docker.sh customer resume WHO
```

- [ ] `rclone ls safe:` works again.

6.3. The report:

```sh
scripts/gateway-docker.sh report
```

- [ ] Test Customer is listed, marked **(trial)**, with the GB stored, up
  and down roughly matching parts 5.3 and 5.4.

---

## 7. Restart, rebuild, backup

7.1. Stop and start, and check that nothing is lost:

```sh
scripts/gateway-docker.sh down
scripts/gateway-docker.sh up
```

- [ ] The **same Yggdrasil address** as in part 1 (the key is kept).
- [ ] `HTTPS works` again, without a new certificate being fetched (Caddy keeps it).
- [ ] On the PC, `rclone ls safe:` still lists the file.

7.2. Reboot the VPS (`sudo reboot`). After it's back, without running
anything:

- [ ] `scripts/gateway-docker.sh status` shows the gateway up and HTTPS
  working. The containers restart by themselves.

7.3. Rebuild, as after an update:

```sh
git pull        # or rsync the source again
scripts/gateway-docker.sh up
```

- [ ] It rebuilds and comes back with the same address and customers.

7.4. Backup:

```sh
scripts/gateway-docker.sh backup
```

- [ ] It writes `deploy/gateway/backups/gateway-….tar.gz`.
- [ ] Copy it off the VPS (e.g. `scp` to the desktop) and list it:
  `tar tzf gateway-….tar.gz` shows `gateway/customers.json` and
  `gateway/objects/…`.

> The backup holds every customer's keys. Keep it private and off the VPS.

---

## 8. Next day: the trial ends

After the trial has ended, on the PC:

```powershell
rclone copy test20.bin safe:again\ -P
rclone copy safe:test20.bin down2\ -P
```

- [ ] The upload is **refused**.
- [ ] The download **still works** (read-only for 30 days after a trial).

Then on the VPS:

```sh
scripts/gateway-docker.sh customer paid WHO -plan "test plan" -quota 10
```

- [ ] On the PC, uploads work again.

---

## 9. Clean up

When the test is done:

9.1. Close the test account. This deletes its files from the members' boxes:

```sh
scripts/gateway-docker.sh customer close WHO -yes
```

- [ ] Within an hour, the space it used is freed on the opted-in nodes.

9.2. Either keep the gateway running for real customers (then set
`MIN_MACHINES=3` again once 3 machines are opted in), **or** take it down:

- On the VPS: `scripts/gateway-docker.sh down`, then delete
  `deploy/gateway/data` and `deploy/gateway/caddy` (`sudo rm -rf`).
- On the desktop: remove the gateway's entry from `peers.json`.
- Remove the domain's DNS record.

---

## Results

| Part | Result | Notes |
|---|---|---|
| 1. Setup | | |
| 2. Mark as gateway | | |
| 3. HTTPS and status | | |
| 4. Key link | | |
| 5. Upload / download | | |
| 6. Suspend / report | | |
| 7. Restart / backup | | |
| 8. Trial end | | |
| 9. Clean up | | |
