# Writing style

Use this page when you write or review any text a user reads: the docs, the README, the website, and the control UI. It sets Writ's voice, the rules that keep the text true, and the terms.

## The voice

Write like an engineer on the team explaining Writ to another engineer who knows OpenTofu and is deciding whether to trust it with production. Be direct, be specific, and be plain about what Writ can't do. Talk to the reader.

## Rules about truth

These don't change.

- Every claim is true at this commit.
- Every output shown came from a real run. Say where it came from.
- No details of any organization.
- Every number has a source and says what it measured. Speed numbers come only from `bench/RESULTS.md`. Every fact about another tool has a source and a date.
- State each limit on the page of the feature it limits.
- No praise for Writ, no hype words, and no hedge on a fact you've verified.
- One term for each Writ concept, from the glossary and the table below. Write commands and flags as the user types them. Write dates as `2026-11-16`.

`docs/json-output.md` is a contract. Change its wording only when the meaning stays the same.

## Rules about voice

| # | Rule | Don't | Do |
| --- | --- | --- | --- |
| 1 | Write to "you". The reader is the subject when they act. | "A person approves from a laptop with the session of `writ login`." | "You approve from your laptop, with your `writ login` session." |
| 2 | Use contractions in prose and UI text. | "It does not run Terraform." | "It doesn't run Terraform." |
| 3 | Open with what the thing does for the reader. Never open with "This page". | "This page says how `writ serve` keeps the secrets of an OpenTofu run out of everything it stores and shows" | "`tofu plan` and `tofu apply` print secrets. `writ serve` keeps none of that raw output." |
| 4 | Use the ordinary verb. "Name" is for names. | "a read-only token that the config names" | "a read-only token that the config points to" |
| 5 | Use a possessive or a compound, not an "of" chain. | "a uid of its own", "the plan of a commit" | "its own uid", "a commit's plan" |
| 6 | Say it in the positive unless the negative is the point. | "This plan changes no resource." | "This plan doesn't change any resources." |
| 7 | One idea a sentence, or two joined by "because", "so", or "but". Split any sentence that needs a second "whose", "which", or "that". | "only an apply that its approval rules allowed, or that an authorized person overrode with a recorded reason, gets the apply role" | "Only an apply gets the apply role: one that its approval rules allowed, or one that an authorized person overrode with a recorded reason." |
| 8 | Explain, then rule, then show. Start a section with one or two sentences on what happens in the normal case and why. Put rules after that, and give one example. | Seven bullets of rules under "Who may approve". | "Anyone with `approve` on the stack can approve a plan, except its author. The details below cover CI jobs and renames." Then the rules. |
| 9 | Keep paragraphs to about five sentences and bullets to one or two. A bullet that needs more is a subsection. | A 229-word numbered item. | A heading, three short paragraphs. |
| 10 | Leave the build history out. No stage numbers, review names, or build dates in user docs. | "Stage S11 of the pivot built it on 2026-11-15." | Nothing. |
| 11 | Put provenance in a caption under the block. | "Real output, 2026-11-16, from `TestTofuApproveApply`, of the text of the plan of" | Block first, then: "Output of `TestTofuApproveApply`, 2026-11-16." |
| 12 | Say what it is, then what it isn't. A limit gets its own sentence or a "Limits" block, after the reader knows what the feature is. | "Writ has no published release yet." as the first sentence of Install. | "Build Writ from source, or use Nix. There's no published release yet, so the install script and the image work only from a local build." |
| 13 | Gloss a Writ term the first time a page uses it, or link it to the glossary. | "applies the sealed plan it kept" | "applies the [sealed](glossary.md) plan it kept" |
| 14 | Say a fact once per page. Link to it after that. | The plan id's hash, written out in four places on one page. | Once, with the figure. |

### Words to swap

Keep Writ's nouns. Use plain verbs.

| Instead of | Write |
| --- | --- |
| names (verb) | lists, sets, points to, refers to, matches |
| holds | has, contains |
| judges | evaluates, checks |
| answers 409 | returns 409 |
| rules ask for | rules require |
| may (ability) | can |
| judged apart | evaluated separately |
| made | created |
| forge | Git host. Keep `forge` as the config keyword. |
| sealed | Keep it, and gloss it once per page: encrypted with the environment's key. |
| refuses | Keep it for the system's decisions. One per sentence. |

### Punctuation and patterns

- No em dashes and no exclamation marks. Semicolons are allowed and rarely needed.
- A question is fine as a heading when it's the reader's question. Don't ask rhetorical questions in body text.
- Don't use an opener that delays the point, a hedge that weakens a fact you've verified, a contrast built only for rhythm ("not X, but Y"), a list of three where the items aren't three separate facts, praise for Writ, or a new name for a thing that has a name.

### Headings

Use sentence case, with no closing period. A task heading starts with a verb: "Add a repository". A concept heading is a plain noun phrase or the reader's question: "Who can approve a plan?" Don't use a bare label such as "The model" or "The input". Say what it is: "How approvals work", "What a policy can read".

## Terms

The [glossary](glossary.md) defines each term. Use the term in the first column, and none of the words in the second. The table is for Writ's own nouns. For verbs, use the plain word from [Words to swap](#words-to-swap).

| Write | Don't write |
| --- | --- |
| stack | project, workspace, configuration, root module |
| environment | workspace, stage, overlay |
| state | state file, statefile |
| state version | revision, snapshot |
| row | entry, record |
| audit log | audit trail, journal, history |
| lock | lease |
| plan | preview, dry run |
| masked plan | redacted plan, sanitized plan |
| saved plan | plan file, planfile |
| apply | deploy, up |
| run | job, execution, for one apply |
| resource (the declaration) | object |
| object (the real thing) | resource, when the difference matters |
| provider | plugin |
| address | ID, name, or path, for a resource |
| variable | param, input, argument |
| output | export |
| module | package, library |
| policy | guardrail, rule set, or for an approval rule |
| approval rule | policy |
| adoption, for `writ state adopt` only | import |
| import, for objects taken into a stack | adoption |
| risk class | risk level, severity |
| secret | sensitive value, except for OpenTofu's `sensitive` mark |
| seal | encrypt, for a state, a saved plan, or a secret set |
| drift | skew, divergence |
| to check | validate, lint |
| agent | AI, bot, assistant |

Write "Terraform and OpenTofu" when a statement is true of both tools. Name one tool when it's true of that tool only, as for a benchmark that ran OpenTofu.

Write a command as the user types it: `writ plan --remote`, not "the plan command". Write a flag with its dashes: `--remote`.

## Pages

Each page is one type. The docs index and the site's navigation group pages by type.

| Type | Answers | Shape |
| --- | --- | --- |
| Start | What is this, and can I try it in five minutes? | Short. One path. Real output. |
| Concept | How does it work, and why is it built this way? | Problem, mechanism, example, limits, how it's tested. |
| Guide | How do I do one thing? | Imperative title, before you begin, numbered steps, check, next. |
| Reference | What exactly is the value, flag, route, or message? | Tables and entries. Complete. No narrative. |
| Project | How is Writ built, and how do I change it? | Architecture and contributor docs. |

- Each concept page ends with links to its guide and its reference. Each guide starts by linking its concept, for readers who want the why.
- Limits live in two places: a short "Limits" section at the end of the page a reader uses the feature from, and [Supported platforms and limits](status.md), which rolls them up. Roadmap that isn't a limit stays out of the user docs.
- Evidence has a fixed home. "How it's tested" closes a concept page. Each output block gets a one-line caption under it with its source.
- Link a Writ term to the [glossary](glossary.md) the first time a page uses it.
- End a page that has a natural next page with a "Next steps" list.

## By surface

**Landing copy.** A headline is a claim the reader can check. Under it, one or two sentences to "you". One figure. At most three bullets a section, about 20 words each, with at most one code span a bullet and no config keys. Each section ends with one plain sentence on its limit, at body size, and a link to the docs for the rest. Prefer a number or a concrete noun to an adjective.

**Concept docs.** Open with the problem or the definition in two or three sentences. Then how it works, with one example or an analogy the reader already has. Then limits. Then how it's tested, which is one of Writ's real strengths and belongs here, not in the opening. End with links to the guide and the reference.

**Task docs.** The title is an imperative. Start with "Before you begin" as a short list. Use numbered steps, one action each, and start each step with the verb. Show the output the reader should see. Put a likely failure inside the step where it happens: "If you see X, do Y." End by saying how to check it worked, and what to do next.

**Reference docs.** Terse, complete, and consistent. Fragments are fine. Each entry says what it does, its type or values, its default, and its limit, in that order. No narrative, no history, and no "whose".

**UI microcopy.** State, reason, next action, in at most two short sentences. Say "you". An empty state says what's missing and how to get the first one: "No plans yet. Run `writ plan --remote` in a clean work tree." A button is a verb and an object: "Approve plan". An error says what happened, why, and what to do, and never blames. Name `writ serve` only when the reader has to act on the server. Elsewhere the product is Writ.

**Error messages.** First line: what failed, with the ids. Second line: what to do. Keep the class prefix (`error: saved-plan:`), because scripts and docs rely on it.
