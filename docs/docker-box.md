# A box in Docker

Run a yggstore box in a container on any machine with Docker, and join the
group with an invite. It's the same box as a Raspberry Pi: its own
Yggdrasil, node, dashboard, hub and Syncthing, set up from its status
screen.

## With a ready-made file

The group's admin makes a file for you, with your invite in it
(`scripts/docker-invite.sh NAME` on the admin's machine). With Docker
installed (below), that file is all you need:

```sh
docker compose -f NAME.compose.yml up -d
docker compose -f NAME.compose.yml exec -it box yggstore screen
```

The first builds the box from GitHub and joins the group by itself, which
takes a few minutes. The second opens its screen: press **Enter** to set it
up ([setting up a new box](box-setup.md#set-up-a-new-box)). Keep the file
to yourself: its invite works once, for 7 days.

## Set it up by hand

You need Docker with Compose, this repository, and an invite
(`yggjoin1:…`) from the group's admin.

- **Docker on Ubuntu or Debian:**

  ```sh
  sudo apt install -y docker.io docker-compose-v2
  sudo usermod -aG docker $USER    # docker without sudo, from your next login
  ```

  On other systems, see [Docker's install guide](https://docs.docker.com/engine/install/).
  Docker Desktop on a Mac or Windows should work too, but hasn't been tried.
- **The repository:** `git clone https://github.com/peterretief/yggstore && cd yggstore`

1. Start the box, named as the group will see it:

   ```sh
   BOX_NAME=anna-box docker compose -f docker/box/compose.yml up -d --build
   ```

   The first build takes a few minutes. The log
   (`docker compose -f docker/box/compose.yml logs`) shows its Yggdrasil
   address, then "Not in a group yet".
2. Join the group:

   ```sh
   docker compose -f docker/box/compose.yml exec box box join 'yggjoin1:…'
   ```

   It links the box's Yggdrasil to the group through the invite's peers and
   joins, giving 20 GB; add a name and a size to change them
   (`box join 'yggjoin1:…' anna-box 50`). It ends with "You're in".
3. Open the box's screen and set it up, as on a Pi:

   ```sh
   docker compose -f docker/box/compose.yml exec -it box yggstore screen
   ```

   Press **Enter**, then give your first name, a password and a name for
   your website and email ([setting up a new box](box-setup.md#set-up-a-new-box)).
   **A** opens the admin. To leave the screen, close the terminal; the box
   keeps running.

The screen then shows your website, email and where to sign in. Restart and
shut down aren't offered: Docker restarts the box if it stops.

## Use it

- **Your files and website:** sign in on the web, as the screen shows, or
  copy files into the container's `/data/files` and
  `/data/yggstore-hub/users/NAME`.
- **Publish a site** from a folder you mount into the container, or with
  `git push` from your own machine
  ([publishing with git push](sites.md#publishing-with-git-push)), which
  needs Yggdrasil there too.
- **Any yggstore command:**
  `docker compose -f docker/box/compose.yml exec box yggstore …`.

## Keep it

Everything is in the `box-data` volume: the box's Yggdrasil key (so its
address), its pieces of the group's files and people's folders. A new
image (`up -d --build` after `git pull`) keeps it. Removing the volume
(`down -v`) removes the box from the group's point of view: tell the
group's admin first, so its pieces are moved off.

## Limits

- The container needs `NET_ADMIN` and `/dev/net/tun` for Yggdrasil, as set
  in `compose.yml`.
- Network drives aren't set up in the container, and devices on your local
  network don't find its hub page.
- Keep it running: the group stores pieces on it, and counts on it being
  there.
