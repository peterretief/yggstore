# Writing the docs

Based on Google's developer documentation style guide, the Diátaxis
framework and plain-language guidelines.

**One kind of page, for one reader need:**

| Kind | The reader wants to | Write |
| --- | --- | --- |
| Tutorial | learn, following along | one path from start to a working result |
| How-to | get a task done | numbered steps toward a goal, nothing else |
| Reference | look something up | complete, neutral facts: commands, options, files |
| Design | understand why, or a plan | reasons, trade-offs, what's decided and what isn't |

Don't mix them: a how-to links to the reference for every option, and to
the design for the reasons.

**Sentences:**

- Start each page with what it lets the reader do, in one sentence.
- Address the reader as "you". Use the active voice and the present tense.
- Keep sentences short, one idea each. Say each fact once, and link to it
  elsewhere.
- Use the same word for the same thing on every page.
- Describe what's built. Plans go in design pages, marked with their
  status, never as if they work.

**Layout:**

- Headings in sentence case, naming the task or the thing.
- Numbered lists for steps, one action per step, the condition before the
  action ("If the box has no screen, ..."). Say what the reader sees when it
  worked.
- Bullets for other lists; tables for things compared side by side.
- Commands, paths and file names in `code`; buttons and menus in **bold**.
- Link text says where it goes: [publishing with git push](sites.md#publishing-with-git-push),
  not "here".
