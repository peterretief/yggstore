# Photos and files on a person's website

Status: approved and built on 2026-10-22 (`internal/site/upload.go`,
`internal/hub/siteuploads.go`, the starter's Gallery page); not yet
deployed. Decided with it: anyone can see what's uploaded (it's part of the
public site), and only people the site's owner picks can upload.

## What it's for

Someone on a box, such as Kath, has a website (`kath.withcare.co.za`). She
wants people she chooses (family, a class, a club) to add photos and files
to it, each with a small preview, and anyone visiting to see them. The
people adding them sign in with the group's login, so there are no
passwords and no accounts on the site.

## What people see

- **On the site**, a gallery page shows the previews, newest first, with
  who added each and when. A preview opens the photo, or downloads the
  file.
- **Someone she picked**, signed in with **Sign in** on the site, gets an
  **Add photos** button. They choose photos or files, add a caption if they
  like, and press Upload. The photo appears in the gallery within a minute
  or two.
- **Anyone else signed in** sees "Ask Kath if you'd like to add photos".
  Someone not signed in sees the gallery only.
- **Kath**, on her **Your website and messages** page, has a list **Who
  may add photos**: she adds people by their group address
  (`tim@group.example`) and can take them off again. On her files page,
  under **Your websites**, the uploads are in `uploads/`, where she can
  delete any of them.

## How it works

It follows the contact form's path, so nothing new travels outside the
group, and an upload made while Kath's box is off arrives when it's back.

1. **The browser makes the preview.** Before uploading, the page draws each
   photo on a canvas at about 300 px and saves that as a small JPEG, so the
   box never has to resize photos. Files that aren't photos get an icon
   instead.
2. **A web node takes the upload** at `/_yggstore/upload` on the site:
   the file, its preview and the caption, with the visitor's access token
   from the group's login. It checks the token (the web nodes signed it
   themselves), that the person is on the site's list, the size (10 MB a
   file, to start with) and the rate (as the contact form's: per visitor and
   per site).
3. **It seals the upload for the site's owner** and stores it in the group
   (`mail.Node.Take`), marked as an upload, exactly as a contact form's
   message is sealed for her and stored. The web node keeps nothing.
4. **Her box collects it like mail.** The hub, which already moves messages
   that open with a person's key into their Messages (`sortMail`), sees the
   mark and puts it in her site instead:
   - `uploads/ID.EXT`, the file;
   - `uploads/ID.thumb.jpg`, the preview;
   - `uploads/ID.json`: who added it (name and group address), when, its
     caption, size and type.
5. **The box keeps `uploads/index.json`**, the gallery's list, made from
   the `ID.json` files. When Kath deletes an upload on her files page, the
   next list leaves it out (and its preview and `ID.json` go too). Then the
   hub publishes the site, as for any change to it.

`ID.json` and the index name who added each upload, not their address,
since the site is public; the box keeps the address in a log of its own
(`uploads.log` beside the person's mailbox).

The list of who may upload is kept with the site's other settings on the
box (its `site.json`, beside the contact form's), and goes to the web
nodes with the site's announcement, as the contact form's sharing code
does. The box checks it again on arrival, so a web node that's behind
can't add someone Kath took off.

### Why the box checks the token

Mail to Kath's address is sealed for the same key as an upload, so anyone
could email her something that looks like one. The envelope therefore
carries the uploader's access token, and the box files it only if the
group's login signed that token (the box asks the web nodes for their
keys), for this site (`aud`), for someone on the list, and not more than
30 days ago (the box may have been off). Anything else goes to her
Messages as ordinary mail.

## Keeping people safe

- **Location in photos.** Phone photos often carry where they were taken.
  The box removes a JPEG's EXIF, XMP and IPTC blocks before it goes on the
  site, keeping only which way up it goes, so phone photos don't turn
  sideways. The browser's preview has none to begin with. PNG, WebP and
  GIF files are kept as they are (phones save photos as JPEG; iPhones
  convert HEIC to JPEG when a browser uploads one).
- **What can be uploaded:** photos (JPEG, PNG, WebP, GIF) and PDFs to
  start with. Nothing that runs in a browser (HTML, SVG, JavaScript) is
  accepted, so an upload can't change how the site behaves. Files are
  served with the type their extension gives, and `nosniff` as now.
- **Kath answers for her site**, as she does for her contact form. She
  picks who may add things and can delete anything.
- **Room.** Uploads count towards the box's storage, like the rest of her
  folder.

## In the starter

The starter site gains a **Gallery** page: the grid of previews read from
`/uploads/index.json`, and the **Add photos** form for people on the list.
It uses the sign-in that's there already.

## What gets built

| Stage | Where | What |
| --- | --- | --- |
| 1 | `internal/site/upload.go` | `/_yggstore/upload`: token check, list check, limits, sealing, `Take`; `ReadUpload` |
| 2 | `internal/hub/siteuploads.go` | checking the token again, filing into `uploads/`, `index.json`, removing photo metadata |
| 3 | `internal/hub` (`SetUploaders`, `site.html`) | **Who may add photos** on her page; the list in `site.json` and the announcement |
| 4 | the starter | the Gallery page and the upload form |

Tests: `TestUpload`, `TestUploadersAnnounced`, `TestSafeName` (site),
`TestSiteUploads` (hub). Web nodes, the box and the site all need the new
version before it works end to end.

## Questions

1. **10 MB a file** enough? Phone photos are mostly 2 to 6 MB.
2. **Members' own sites** (a node of their own, not a box) aren't covered:
   their node has no hub to file uploads. Later, if wanted.
