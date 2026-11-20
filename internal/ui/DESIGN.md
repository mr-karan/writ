# Control UI design

The control UI is where a team sees what waits and what works, reviews a masked plan, approves it, and watches its run. It is a working tool read every day, so it is calm: neutral greys, soft cards, and color only where something needs a person.

## Direction

- **Calm and light.** A pale grey page, white cards with a 1 px border and rounded corners, and no shadow. A card is the one box of the UI: a table, a list of rows, a set of facts, and a group of changes each sit in one, and none sits inside another, so a note inside a card is a line, not a box. The dark theme is the same layout in dark greys. Both follow `prefers-color-scheme` until the settings page sets a theme.
- **A frame of two parts.** A sidebar and the page. The page starts with the trail of what is above it. The website's blueprint look stays on the website.
- **Tones.** Green ended well or adds, amber waits for a person or changes, red failed or deletes, blue is at work. Grey has nothing to say. Each tone is also a shape and sits beside a word: a dot at work has a halo, a failed dot is square, a quiet one is a ring, and every status pill names its status. Color is never the only sign.
- **One scale.** Six text sizes, `--fs-1` to `--fs-6` in `static/writ.css`: notes, table heads, chips, and times in rows at 12 px; secondary text, controls, mono text, and the labels of facts at 13 px; body text, tables, and card headings at 14 px; a section's heading outside a card at 15 px; the page's title at 20 px; and a stat's figure at 24 px. `--gap` is the space between two cards, `--inset` the side padding of a card, which every row, head, and cell of the card starts at, and `--ctl` the height of every control. A column of numbers is set right in tabular figures, and a time at the right of a row.
- **Type.** The system's own sans-serif for text and its monospace for ids, commits, addresses, and values. The UI loads no font, so its Content Security Policy has no `font-src`: the system faces are what the calm look wants, and they cost no bytes.

## The frame

| Part | What it shows | Where its data comes from |
| --- | --- | --- |
| Sidebar sections | Overview, Runs, Plans, Approvals, Repositories, Access, Audit, Settings, each with its icon from the page's one inline sprite. Approvals counts the plans that wait, Runs the plans that apply, Repositories the repositories. A zero shows no count. A count from a list writ serve cut short carries a `+`. | `GET /v1/overview` |
| Filter | Under the label Stacks, a GET form: `?find=` narrows the tree to stacks whose name holds the text. It keeps the rest of the page's query. | The request |
| Tree | Each environment the reader may read, folded with `<details>`. Each environment shows its dot and how many stacks it lists, a floor with a `+` when writ serve read only some of its plans. Under it each listed stack: the first 200 whose state writ serve keeps or reads, and those a live plan names before their first apply. A dot and words per stack, such as `1 pending · 1 applying`, `drifted`, `check failed`, `not checked`, `no state yet`, or `0+ plans · clean` when writ serve read none of its plans. A stack with nothing to say, its dot all clear and no live plan, keeps its words for a screen reader only. An environment whose stacks with state the list left out says how many, and never `No stack yet`. The stack of the page is marked current. | `GET /v1/overview` |
| Sidebar footer | When a repository was last polled, or how many polls failed; then who is signed in, a link to Who you are, and a Sign out button. | `GET /v1/overview`, the session |
| Trail | The pages above this one, such as `prod / app`, over the title. The page itself is the title, not a step of the trail, and a page with nothing above it has no trail. | The page |
| Footer | One line: when the page loaded, and how often it reloads while something on it is in progress; at the right the requests behind the page and the CLI command that prints the same document, folded under API, and the JSON link. | The page |

The sidebar costs a page one read, `GET /v1/overview`, which answers what every environment the reader may read holds. A page asks writ serve for each document once: `fetch` keeps every answer by its target, so the overview, and the environment tabs of a list, reuse the sidebar's read. The sidebar's own read stays out of the footer, which lists the requests behind the page's content. A page that fetched nothing, an error page or sign-in, shows the sections and no tree. When the read of the overview fails, the sidebar says "Writ could not read the environments." in place of the filter, the tree, and its footer, and no section shows a count: a failed read is never shown as zero environments or zero stacks that match.

On a screen narrower than 64rem the sidebar is a bar with a Menu button, a closed `<details>`, that opens the sections and the tree. On a wide screen CSS shows the menu with `::details-content` and `writ.js` opens it, so a screen reader hears it expanded. A browser without `::details-content` keeps the Menu button.

## The overview

Every number comes from `GET /v1/overview`, the page's one read. Nothing is estimated. A plan counts under the status `GET /v1/plans/{id}` shows, which its approvals and the approval rules of now give it, not the status writ serve stored when it last changed. Failed runs and drift count every run and stack the reader may read, past the 200 the page lists. Plans are read 200 of each status in each environment, so when an environment holds more, each plan count carries a `+`.

Three rules hold for every value, and [Every value of the overview](#every-value-of-the-overview) lists how each keeps them:

1. A value is computed only from rows the reader may read. Every read drops the rows the reader may not read before it counts, cuts at a limit, or sets a `more` flag, so no row the reader may not read changes any field.
2. A value is exact, or marked partial: a `+` on a floor, or words that say writ serve left some out. No all-clear and no "No X" shows when a value it depends on is partial.
3. "None" and "not shown" stay apart. A stack with state that a list left out is counted and said to be left out, never shown as a stack with no state.

| Part | What it counts |
| --- | --- |
| Waiting for approval | Plans with status `pending`, and how many are the reader's |
| Applying | Plans with status `applying`, and how many are `planning` |
| Failed | Runs with status `failed` that ended in the last 24 hours |
| Drifted | Stacks whose last drift check found drift, of those checked |
| Alerts | Drift found, a drift check that failed, a state written outside writ serve, a failed run, a pending or approved plan that expires within 2 hours or a quarter of the plan lifetime, whichever is shorter, and a repository poll that failed. Each environment lists the drift alerts of at most 200 stacks and its newest 200 failed runs, and the count carries a `+` past that. The page says all is clear only when no list it reads was cut: with a `+` on the plan counts, a plan about to expire may be missing, and the page says so. |
| Applying now, Waiting for approval, Planning now | One row a plan: a dot, the `stack@env`, the commit and the author, and its age. A row of Applying now links the run and dates from its start; the others link the plan. |
| Stacks with state | Every stack with its backend, resources, and drift |

## Every value of the overview

Each field of `writ.overview/v0`, and each number, list, and all-clear the sidebar and the overview render from it. "Readable" is a stack@env the reader may read; every read drops the others first.

Every number the two render goes through template `n`, from a `figure`: a count and whether it is a floor, which `n` marks with a `+`. Every sentence that says a thing is absent or all is well comes from func `none`, which says one of the sentences of `nones` only when each figure it rests on is known to be 0, and a floor never is. [Every count and absence of every page](#every-count-and-absence-of-every-page) says how the other pages keep the same two rules, and [The render check](#the-render-check) how a test holds every page to them.

`TestOverviewProperty` in `overview_property_test.go` checks the three rules over estates of more than 200 stacks, plans, alerts, and failed runs, plans of every status, and stacks whose only plans a cut leaves out, for readers of different grants, on SQLite and Postgres. Its `checkRender`, in `overview_render_test.go`, renders the overview of every environment and of each one, and the sidebar unfiltered and filtered for a stack it lists, a stack whose plans were cut, and a stack that does not exist. It holds every number of `n` in the main part and the sidebar, the table of stacks with state included, every tree note and dot, and the filter's count, to a brute-force count of the readable rows by the rule in this table, and fails a number of `n` or a dot it has no rule for. It runs the render check on each page, with each sentence of `nones` judged by its guard. The two estates whose lists pass 200 rows run under `TestOverviewPropertyCrowded`, so that a race run of each test stays short.

| Value | Source | Exact or partial | What makes it partial | How the UI shows it |
| --- | --- | --- | --- | --- |
| `envs[].name` | The config's environments where the reader holds a read grant | Exact | Nothing | Tree, environment tabs |
| `envs[].stacks`, `checked`, `drifted`, `check_failed` | `DriftTargets`: one narrow row per readable stack with state and its last check | Exact | Nothing | Drifted card, tree count and dot |
| `envs[].failed` | `FailedRuns`: every readable run of the environment that failed in 24 hours | Exact | Nothing | Failed card |
| `envs[].plans` | `Plans` per stored live status, readable plans only, at most 200 each, sorted by the status of now | Floor | More than 200 readable plans of one status: `plans_more` | `+` on every plan count of the environment |
| `envs[].plans_more` | Whether a read of plans left a readable plan out | Exact | Nothing | Drops the all-clear and "No plan" lines |
| `envs[].alerts_more` | Whether the environment's drift alerts (200 stacks) or failed runs (newest 200) left a readable one out | Exact | Nothing | `+` on the alert count, a note, no all-clear |
| `stacks[]` with `drift` | `DriftStacks`: the first 200 readable stacks with state, by env and stack | Partial | More than 200: `stacks_more` | Tree and table say how many more, from `envs[].stacks` less those listed |
| `stacks[]` with `drift` `null` | Stacks a read plan names that `DriftTargets` holds no state for | Floor | `plans_more` of its environment | Tree note `no state yet`, its plan counts with `+` |
| `stacks[].plans` | The read plans of the stack | Floor | `plans_more` of its environment | `+` on the stack's note |
| `stacks_more` | Whether `DriftStacks` left a readable stack out | Exact | Nothing | "The tree shows the first 200 stacks with state" |
| `applying`, `pending`, `planning` | The read plans by status of now | Partial | `plans_more` | `+` on headings and cards; an empty list says what writ serve read |
| `applying[].run` and the others | `PlanRuns`: the run of each listed plan | Exact | Nothing | Applying now links the run |
| `alerts` of drift, a failed check, an outside write | `DriftAlerts` per environment, 200 stacks each | Partial | `envs[].alerts_more` | As `alerts_more` |
| `alerts` of a failed run | `FailedRuns` per environment, newest 200 | Partial | `envs[].alerts_more` | As `alerts_more` |
| `alerts` of a plan that expires | The read pending and approved plans | Partial | `envs[].plans_more` | Note that a plan about to expire may not show; no all-clear |
| `alerts` of a failed poll, `repos` | Every repository, for a reader with any read grant, as `GET /v1/repos` | Exact | Nothing | Footer, Repositories count, alerts |
| Sidebar Approvals and Runs counts | `pending` and `applying`, every environment | Floor | Any `plans_more` | `+` |
| Tree environment dot | `envs[]` counts: plans, `check_failed`, `drifted`, `checked` | Exact but for plan floors, which are not 0 when their status was cut | `plans_more` | The dot and its words; never all clear while plans are cut |
| Tree environment count | `envs[].stacks` and the stacks a read plan names that have no state | Floor or exact | `plans_more` | `+` on the count |
| Tree "N more with state, not listed" | `envs[].stacks` less its stacks listed with state | Exact | Nothing | Shown when not 0, in place of "No stack yet" |
| Tree stack dot and words | The stack's drift and `stacks[].plans` | Exact but for plan floors | `plans_more` of its environment | `0+ plans` beside its drift when no plan of it was read, and never all clear |
| Tree "No stack yet" | No listed stack and none left out | Exact | `plans_more`: a stack a cut plan names may be left out | Shown only when exact; else a note that a stack a plan names may not be listed |
| Filter "N stacks match" | Listed stacks whose name holds the text | Exact, or of the listed ones | `stacks_more`, or any `plans_more`: a stack a cut plan names is not listed | "N+ listed stacks match"; an environment whose plans were cut stays in the filtered tree |
| Overview cards | Waiting, Applying, Planning from the lists; Failed, Drifted, Checked from `envs[]` | Floor; exact | `plans_more` | `+` on the floors, also on "by others" and "yours" |
| Alerts count and list | `alerts` of the environments shown | Partial | `alerts_more` or `plans_more` of an environment shown | `+`, and a note |
| All-clear line | No alert listed | Exact only when nothing is cut | Any `plans_more` or `alerts_more` shown | Shown only when neither is set; else a note says what may be missing |
| "Nothing waits" note | No plan of a live status, approved ones included, and no stack with state | Exact | `plans_more` | Not shown while plans are cut |
| Stacks with state | `envs[].stacks` and the listed stacks | Exact count, partial table | `stacks_more` | "N more here are not listed", never "no stack yet" while some have state |

## Every count and absence of every page

Each page outside the overview keeps the overview's rules: a count goes through `n` and is a floor when the read behind it was cut, a sentence that says a thing is absent goes through `none` and is said only when that is known, and a list that shows part of what writ serve holds says so and links the rest.

| Page | Count or sentence | Source | When it is partial | How the page shows it |
| --- | --- | --- | --- | --- |
| Approvals | Plans by others, Yours | `GET /v1/plans?status=pending`, the newest 200 readable plans that wait | `next` is set: more wait | `+` on both counts, a note that the newest 200 plans that wait are listed with a link to the older ones, and in place of "No plan by someone else waits here." or "No plan of yours waits here." a line that one may wait among the older ones |
| Plans | The plans that match | `GET /v1/plans`, 50 a page | `next` is set | "The newest N plans that match" in the card's foot and a link to the next page. "No plan matches." only on the newest page |
| Runs, a stack's Runs | The runs Writ started | `GET /v1/applies`, the newest 50 | `more` is set | `+` on the count and "Older runs are left out." "Writ has started no run here." only when the list is empty |
| A stack's Plans | The plans of the stack | `GET /v1/plans?stack=`, 50 a page | `next` is set | "The newest N plans of this stack" and a link to the rest on Plans |
| A stack's Resources | The resources, or why there are none | `GET /v1/index` and the drift record | The drift record's read failed | "Writ keeps or reads no state of this stack yet." with no drift record, "has not indexed it yet" with one, "The state index holds no resource of this stack." with an empty index, and that writ serve could not read the drift record when its read failed |
| A stack's drift | Changes outside OpenTofu and in the code, those shown | The newest drift check | Nothing; the table shows the first 200 changes and says so | Each through `n`; "No drift check of this stack has run." only with no check |
| Repositories | Stacks of each repository | The stacks found at its head | No commit read | `not read` in place of a count |
| A repository | Stacks found, files of the commit read | `GET /v1/repos/{name}` | Nothing | Through `n`; "The commit read holds no stack" only when it holds none |
| Getting started | Stacks found and served, plans | Each repository, and the newest 200 plans of each environment | A repository's read failed, the overview that names the environments failed, an environment's plans failed, or an environment holds more plans | `+`; "No plan yet." only when every read was whole and found none, else that writ serve could not read every environment's plans |
| Access, Who you are | Grants, teams, your grants | `GET /v1/config`, `GET /v1/whoami` | Nothing | Through `n`; "None. No team's rule matches your claims" and "No grant names one of your teams." only when that list is empty |
| The plan, the run | Changes, modules, outputs, the tally, approvals, diagnostics, resources | The plan's or the run's own document | Nothing | Through `n`. Their words describe one record, so no sentence of theirs goes through `none` |

A read that fails is never read as empty. A page whose main document failed shows writ serve's refusal with its status. Approvals, Plans, and Runs with no `?env=` take their environment from the overview, and show the overview's refusal when it failed. A run's page shows the refusal of the run or its plan when either read failed with anything but 404. A plan's page offers no adopt command when the drift record that names the stack's backend failed to read, since the command's flag depends on it.

## The render check

`checkWords` in `render_check_test.go` is the check every page passes. `checkPage` runs it on each page `TestNoScriptWalk` opens, which fails when a route or a template is left that no request reached, so every page and every part of one is read. The OpenTofu pages' helper runs it on each page of real plans, runs, and drift, `checkRender` on the overview, and `TestPagesProperty` on the pages above.

- **Digits.** Every digit of the page's text, tables and captions included, comes from a number of `n`, or from text the check allows: a timestamp or a duration (`time`); an id, a commit, a merge request, a command, an address, a version, a port, an HTTP status, or a configured value (`code`, `pre`, `.id`, `.id-full`); a name the test gives (the environments and stacks of its estate); the filter's own text, quoted as `“text”` exactly as `?find=` gave it; or a phrase the templates write in their own words, such as "the newest 200" or "every 30 seconds". A name set in prose, such as a person's display name or a project (`.ref`), and text writ serve or OpenTofu wrote, such as a diagnostic, an error, or an alert's detail (`.msg`), hold a digit only by the rules of served text below. A number of `n` must read as a count with an optional `+`. The check fails any other digit, a quoted one included.
- **Served text.** An element of `.msg` or `.ref` whose text holds a digit passes only by these rules, in this order:
  1. Its text holds a letter. A text with no letter is a number, and fails, whatever its markup.
  2. Its whole text is a name the test gives, and passes. Else it passes only by 3 to 6:
  3. Its `data-field` names a field of its class: a message or an error for `.msg`, a display name, a project, or a repository for `.ref`. A step that ends in `[]` goes through each element of a list.
  4. It or an ancestor has a `data-record`. A missing one fails. A malformed one fails: one that does not parse as a query, or holds no pair, an empty key or value, or a key twice.
  5. The `data-record` picks exactly one record of the documents the page's handler read. A record is a JSON object whose value at each key, a path of fields joined by dots such as `run.id`, is the string the pair holds. Copies of one record equal in every field, such as a plan the plan list and the sidebar's overview both hold, count as one. No record or two fail.
  6. The record holds the element's whole text at its `data-field`.

  The documents are the answers writ serve gave the page's handler, recorded by `TraceFetches` in `export_test.go` as `TraceTemplates` records templates. The check reads no other: not the footer's list, not the page's `?as=json` link, and no document a test fetches after.
- **Records.** Every `data-record` of every page parses, and names a record by values the page shows: each value is in the page's text or one of its links. Only a value that says what type a record is, not which one, is exempt: `kind`, one of the six kinds of an alert, which the page says in words, and `format`, one of `writ.remote-plan/v0`, `writ.tofu-apply/v0`, `writ.health/v0`, and `writ.error/v0`, or the format of a document the page's handler read, as a refusal names the document it came in. It names the document a part of the page renders, such as a plan's page with its policy results and diagnostics. So a plan is named by its id, or by its ref before it has one, and a row that links a run names its plan by `run.id`.
- **What a person reads but does not see as text.** The page's `<title>`, its trail, and the attributes a person reads or hears, `aria-label`, `aria-description`, `aria-valuetext`, `title`, `alt`, `placeholder`, and the value of a field that shows it, hold a digit only in a given name or in the whole text of an id or `code` element of the page, such as the plan id in "Plan 2fbc4036", or in the filter's own text, which its field holds. Any other quotation allows nothing. A count cannot go through `n` there, so none may stand there. Every other attribute is for the browser: a URL, an id, a class, a geometry.
- **Absence.** In the sidebar of every page, and in the main part of the overview, Approvals, Plans, Runs, a stack, Repositories, a repository, Access, Audit, Who you are, Settings, and Getting started, no word of "no", "nothing", "none", "never", "clean", or "all clear" is said but in a sentence of `nones`, in a listing or a field (a table, a list of rows or alerts, a `dl`, a tree note), or in the text the digit rule allows, such as writ serve's own message or a command. The term "clean work tree" is allowed. A page of one plan, one run, one drift check, sign-in, an error, or the repository form describes one record or one request, and only its sidebar is held. The UI ships in English only, and the check reads English: a sentence in another language would pass it.
- **Guards.** Where the test counts the rows, each sentence of `nones` the page says is held to them: `checkRender` by the overview's guards, `TestPagesProperty` by the counts the page rests on. `TestPagesSayNoneOnlyThroughNone` fails a sentence of `nones` with no guard, and a word of the list above in a page template's own text outside a listing, a field, or a message. `TestRenderCheckCatchesInjectedCounts` holds the check itself to these rules: it feeds it a count quoted in text and in a title, beside the filter's text, as a `.msg`, as a `.ref`, as part of a served message, as a served message with no field, no `data-record`, or of another record, as a served id set as a message or a name, as a number alone, with a malformed `data-record`, one that picks two records or none, one that names a value the page does not show, and a document only the footer names, and each must fail. `TestRecordNamesWhatThePageShows` holds a finished plan's pages to its id. `TestRenderCheckVouchesByField` does the same on a real overview whose plan came from pipeline 12345.

`TestPagesProperty` in `pages_property_test.go` renders, for each reader and estate of `TestOverviewProperty` and one where the reader's own plan waits behind 200 newer plans of others, on SQLite and Postgres, the approvals, the plans of every status and of four single statuses, and the runs of each environment, the views of a stack with state, a stack a plan names, and a stack that does not exist, Getting started, and, for each reader of the first estate only since they read nothing of an estate, the repositories and each one, Access, Who you are, and Settings. It holds every number of `n` in each main part to a brute-force count of the readable rows, or to the config, the session, or the repository the page reads, each list to its number, and the link to more to whether more exist. A plan's changes and a commit's files are fields of one record, which the check reads as fields. The two crowded estates run under `TestPagesPropertyCrowdsStaging` and `TestPagesPropertyCrowdsEveryList`.

## Components

`static/writ.css` holds every component. Its tokens are the custom properties on `:root`: the colors as `light-dark()` pairs, the type scale, the space steps, the radii, and the twelve colors of the categories of a stack's resources.

| Component | What it shows |
| --- | --- |
| Page head (`.crumbs`, `.page-head`, `p.meta`) | The trail of the pages above, the title with its status pill and the page's actions at the right, then at most one line of facts split by ` · `. |
| Card (`.card`, `.card-head`, `.card-foot`) | The one box. A head of 44 px holds a heading and its count, then a tally or a line of facts, then actions at the right. A foot says how much of a list shows and links the rest. |
| Table (`table.list`) | A hairline under each row, grey heads, a time or a duration at the right. On a phone each row is a block, the cell that names it first and each other cell with its column's name before it. A caption on every table, for a screen reader only. |
| Rows (`.rows`, template `rows`) | A live list in a card: a dot or a tone icon, a title, a grey line under it, and a time at the right. No chevron. |
| Facts (`dl.kv`, `.kv-2`) | A label and its value on each row, every label in one column. `.kv-2` sets two pairs a row on a wide screen. |
| Status pill (`.stamp`, template `status`) | A status, in one of five tones from `tones` in `status.go`: run, wait, ok, bad, void. Void is grey, ringed, and struck through. |
| Dot (`.dot`) | A tone as a shape, always beside words. |
| Count (`.count`) | A number of `n` beside a heading, a tab, a section, or an environment. |
| Chip (`.chip`, `.chips`) | A risk class or a mark beside a change. |
| Bar (`.bar`) | One row of controls over a list, each `--ctl` high: a filter field (`.find`), pickers, a segmented control, and at the right what acts on the whole list. A filter field sends on Enter, with no Filter button. |
| Picker (`details.pick`, template `pick`) | A control that shows its current value and opens to a list of links, one a value, so it needs no script and no form. `writ.js` closes it on Escape and on a click outside. |
| Segmented control (`.seg`) | A mode of a list, or the theme. |
| Tabs (`.tabs`, template `env-filter`) | The views of a page, or the environment a list is of, underlined. |
| Stations (`.steps`, template `steps`) | Where a plan or a run is: plan, approve, apply. Green is done, a blue ring is at work, amber waits for a person, a red square failed, dashed will not happen. `planSteps` and `runSteps` decide them. |
| Tally (`.tally`) | The counts of a plan or a run on one line in its card's head, a zero in grey. |
| Stat cards (`.stats`) | The overview's four counts in one card, each a link to its list. |
| Disclosure (`details.change`, `.fold`, `.same`, `.v`, the footer's API) | One CSS chevron for everything that opens. A change opens to its attributes; a fold is a card that is one heading until opened; a long value is one line until opened. |
| Diagnostics (`ul.diags`, templates `diags`, `plan-diags`) | One row a diagnostic: its severity as a chip, its class in grey, its message. A failed plan whose state has no secret set words it in the adopt note, so its diagnostic is one short line. |
| Resource row (`details.res`, template `res-row`) | A resource of a stack's state index: its address, its type in words with acronyms in capitals (`IAM role`, `LB listener`), its ID, and its module as a chip when it is not the root. Under a heading of its category or module the address drops its module and type, which stay for a screen reader and a copy. The flat view shows it whole. An ID that is the resource's ARN shows as the name the ARN ends with. A long address breaks after `/`, `:`, `.`, or `[`. Opened, the row shows the whole address, the provider without its registry host, the ID unless it is the ARN, the ARN, and the region from the index or the ARN. |
| Note (`.note`) | A state the reader must act on, with a tone icon. Never what a pill, a station, or the footer already says. |
| Empty (`.empty`), all clear (`.calm`) | A quiet centred sentence in a card, with at most one button under it; a green dot and the all-clear sentence. |
| Command (`code.cmd`, `.cmd-line`) | A command in a 1 px box, with a copy button `writ.js` adds. |
| Icons (template `sprite`, template `icon`) | One inline sprite at the top of the page. `{{template "icon" "warn"}}` draws one by its name. |
| Started by (`.by`, template `started-by`) | What started a plan: a grey person or CI icon before its author in every list of plans and on the plan's head line, with the words for a screen reader, and "Started by" in the plan's facts. The plans list filters on it with the Started by picker. A drift check's card is headed with the timer icon, "Drift check", and the interval `GET /v1/drift` gives. An audit row marks who sent it with the same icons, from its `principal_kind`. |

## The plan page

The plan page answers, in this order: what is this plan, where is it, what must happen next, and what does it change.

1. The title with its status, and one line: the stacks, the environment, the author, and the origin.
2. The head card. The stations say where the plan is. Under them are the approvals needed and given, then on a band of its own the next step: the approve button, the reason there is none, or the apply command with its copy button. The facts close the card under a hairline.
3. The changes, with the tally in their head. Risks, the policy's outcome, and the OpenTofu version are facts of the head card. A policy with results, or one that denies, has its own card.
4. The changes, grouped by module: one row a change, with its sign, address, action, and marks. A row opens to its attributes. A page that lists at most 5 changes shows them open, and a page of more shows them folded (`unfoldedChanges` in `tofu.go`). The table of attributes shows only the columns a change has: a create has no before, a delete no after. The attribute names take one column of a set width, so they line up from change to change. Before is washed red and after green. A value the mask hid (`(sensitive)`) is hatched and dashed, so it never reads as a literal, and a value known only after the apply is set in italics. A long value shows one line until a reader opens it, and is set over lines only then.
5. What changed outside OpenTofu, folded into one section with its count. Opened, it lists one row an object, with the names of the attributes that changed, and each row opens as a change does.
6. The outputs, and the plan as text.

The page takes its structure from the run pages of HCP Terraform and Spacelift, which list a plan's resources as rows that open, and from the status bar of Argo CD, which says in one place what state an application is in.

## Rules the pages keep

- Server-rendered, no framework, no build step. Every page reads and every form works without JavaScript. `writ.js` only adds local times as titles, keeps the "Loaded" time relative, opens the sidebar on a wide screen, adds a copy button beside a command, adds a button that opens or folds all the changes of a plan, and closes a picker on Escape or a click outside it. A change, a long value, and the section of outside changes fold with `<details>`, so each opens without the script.
- Nothing from another origin. The Content Security Policy allows only this origin's scripts, styles, and images.
- Every page keeps its JSON link, and its API and CLI lines folded under "API", in a one-line footer.
- No sentence says what a page, a table, or a control is for, or repeats a status the page shows. A note is for a state the reader must act on.
- A missing time is a blank, not a word. A list shows a time as how long ago, with the instant in `datetime`; a page of one record shows the instant in UTC.
- The product is Writ. A page names `writ serve` only where the reader has to act on the server.
- No `style` attribute, which the Content Security Policy refuses, and no `title` that holds a digit.
- One `h1`, landmarks, a skip link first, a caption on every table, for a screen reader only, a scope on every header cell, a label on every field, and a visible focus ring, drawn inside a box that clips.
- Every color is a `light-dark()` pair of hex values in `static/writ.css`. `TestContrast` checks every text color on every ground it sits on, the sidebar's included, in both themes, at 4.5:1, and the focus ring, the tones, and field borders at 3:1.
- No secret reaches any answer. `TestNoScriptWalk` and `TestTofuSensitiveValuesNeverRender` read every answer their browsers get, and each fails when a route of `routes` in `ui.go` or a template (any `{{define}}`, a page or a part of one) is left that it never reached, so a new page or part joins both or fails them. `Mount` hands that table to `server.MountUI`, which takes it once and is the only way a route under `/ui/` reaches writ serve: any other registration of one panics at start. Each route's handler is an `http.HandlerFunc`, not any `http.Handler`, so a router such as an `http.ServeMux` cannot sit in the table and serve paths it never lists. The type stops a router, not a func that serves its own paths by reading `r.URL.Path`: review keeps each handler to the paths its pattern names.
- The stylesheets and scripts stay under 24 KB gzipped (`TestAssetSize`).

## Seeing every page

`showcase_test.go`, behind the `uishowcase` build tag, serves two dev control planes on loopback: one with a plan and a run in every state, and one empty. It is in no binary and no normal test run.

```
go test -c -tags uishowcase -o showcase.test ./internal/ui
cd internal/ui && WRIT_SHOWCASE_OUT=DIR ../../showcase.test -test.run '^TestShowcase$'
```

It writes `DIR/pages.json`, with each site's address, a one-time sign-in link, and its pages, and serves until `DIR/stop` exists. Before it writes the file it signs in to each site and fails unless every page it lists serves. `TestShowcaseServes` runs the same setup and returns, and `just test-serve` runs it, so the showcase cannot break unseen.

## Left as it is

- On a phone, every table sets its rows as blocks. A browser may then drop the table's roles for a screen reader, and each cell carries its column's name only visually. Not tested with a screen reader.
- Oat 1.0.0 still gives the base of buttons and fields. Only its stylesheet is embedded: no page uses a part of Oat that needs its script.
