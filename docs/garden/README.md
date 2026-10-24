# The garden, step by step

The site built in [Make a website on yggstore, step by step](../howto-website.md),
as it stands after each step. The steps not here don't change the folder.

| Folder | After step | What it has |
| --- | --- | --- |
| [step-1](step-1/) | 1–2 | one page, `index.html` |
| [step-3](step-3/) | 3 | Markdown pages in one layout |
| [step-4](step-4/) | 4–5 | a style and a picture |
| [step-6](step-6/) | 6 | a project: live preview, the open days from its database |
| [step-7](step-7/) | 7 | Go in the browser: the compost calculator |
| [step-8](step-8/) | 8–10 | the work-day sign-up form, kept on the web nodes |

To start again from one, in `/media/peter/storage1/YGGSITES`:

```sh
rm -r garden.withcare.co.za && cp -r ../YGGSTORE/docs/garden/step-6 garden.withcare.co.za
```

These folders are made from the how-to by `scripts/garden-steps.py`, so
they say what it says: change the how-to, then run the script again.
