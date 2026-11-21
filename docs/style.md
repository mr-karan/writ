# Writing style

Use this guide when writing or editing any user-facing text: the docs, the README, the website, and the control UI.

## Voice

Write like an engineer on the team explaining Writ to another engineer who knows OpenTofu and is deciding whether to run it in production. Be direct, specific, and plain about limits. Address the reader as "you".

## Rules for accuracy

- Every claim must match the code at this commit. Check flags, config keys, defaults, and error messages against `cmd/` and `internal/`.
- Every terminal output block must come from a real run. Put a one-line source caption under the block (for example: `Output of TestTofuApproveApply, 2026-11-16.`).
- Never include company names, private hostnames, or real account IDs in docs or examples.
- Every number needs a source and what it measured. Speed numbers come only from `bench/RESULTS.md`. Facts about other tools need a source and date.
- State limits on the page for the feature they limit, and roll them up in [Supported platforms and limits](status.md). Leave roadmap items out of user docs.
- Use one term per concept from the [glossary](glossary.md) and the [Terms](#terms) table. Write commands and flags as typed (`writ plan --remote`, `--remote`). Write dates as `2026-11-16`.
- `docs/json-output.md` is a contract. Do not change its meaning when editing wording.

## Rules for prose

- **Lead with what the reader needs**: what the feature does, how to use it, or what a failure means. Never open with "This page", "In this guide", "Below you'll", or a recap of another page.
- **Keep sentences short and active.** One idea per sentence. Split sentences that need multiple relative clauses ("whose", "which", "that").
- **Keep paragraphs short.** One idea per paragraph, typically one to three sentences. Keep bullet items to one or two sentences.
- **Show, don't narrate.** Prefer a command, config snippet, or table over prose describing one.
- **Say it once.** Delete repeated explanations and link to the single page that owns the topic.
- **Use contractions** in prose and UI text (`doesn't`, `can't`, `you'll`).
- **Use possessives and compounds** instead of "of" chains ("a commit's plan", "its own uid", not "the plan of a commit", "a uid of its own").
- **State positives directly** unless negation is the point ("This plan changes nothing", not "This plan changes no resource").
- **Leave out build history.** No stage numbers, internal review names, or pre-release history in user docs.
- **Explain, then state limits.** Say what a feature does before stating what it cannot do.
- **Gloss or link Writ terms** the first time a page uses them (for example, `[sealed](glossary.md)`).

### Cut these patterns

- Em dashes (`—`, `--` as punctuation) and exclamation marks.
- Marketing adjectives and filler: "seamless", "powerful", "robust", "effortlessly", "comprehensive", "calm", "battle-tested".
- Rhetorical questions in body text (questions are fine as headings when they match what the reader is asking).
- Triplets used only for rhythm, and "not X, but Y" contrasts used for style.
- Filler openers ("Here's how", "Think of", "Say you want to", "You shouldn't have to take X on trust").
- Hedges on verified facts, or praise for Writ.

### Words to swap

Keep Writ's nouns. Use ordinary verbs.

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
| forge (in prose) | Git host (keep `forge` for the config block) |
| sealed | keep, and gloss once per page (encrypted with the environment's key) |
| refuses | keep for system decisions, at most once per sentence |

### Headings

Use sentence case with no trailing period. Task headings start with a verb ("Add a repository"). Concept headings are plain noun phrases or reader questions ("Who can approve a plan?"). Avoid vague labels like "Overview" or "The model". Keep existing heading anchors stable so links do not break.

## Terms

The [glossary](glossary.md) defines each term. Use the first column, not the second.

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
| run | job, execution (for one apply) |
| resource (declaration) | object |
| object (real infrastructure) | resource (when the distinction matters) |
| provider | plugin |
| address | ID, name, or path (for a resource) |
| variable | param, input, argument |
| output | export |
| module | package, library |
| policy | guardrail, rule set, or approval rule |
| approval rule | policy |
| adoption (`writ state adopt`) | import |
| import (bringing objects into a stack) | adoption |
| risk class | risk level, severity |
| secret | sensitive value (except OpenTofu's `sensitive` mark) |
| seal | encrypt (for state, saved plan, or secret set) |
| drift | skew, divergence |
| check | validate, lint |
| agent | AI, bot, assistant |

Write "Terraform and OpenTofu" when true of both; name one tool when true of only that tool.

## Page types

| Type | Answers | Shape |
| --- | --- | --- |
| Start | What is this, and how do I try it? | Short. One path. Real output. |
| Concept | How does it work, and why? | Mechanism, example, limits, how it's tested. |
| Guide | How do I do one thing? | Imperative title, prerequisites, numbered steps, expected output, troubleshooting. |
| Reference | What is the exact flag, key, route, or error? | Tables and terse entries. No narrative. |
| Project | How is Writ built and tested? | Architecture and contributor docs. |

## By surface

- **Landing page:** Headline is a verifiable claim. One or two sentences below it. Concrete nouns and numbers over adjectives. Every claim states its limit nearby and links to docs. Keep Writ jargon (`sealed`, `masked plan`, `grants`, `risk classes`) in the docs where simpler phrasing works on the landing page.
- **Concept docs:** State the mechanism in two or three sentences, give one concrete example, state limits, and explain how it is tested.
- **Guides:** Imperative title, short prerequisites list, numbered steps starting with verbs, expected output, inline failure recovery ("If you see X, do Y"), and a brief troubleshooting list.
- **Reference docs:** Terse and complete. Fragments are fine. State purpose, type/values, default, and limit in that order.
- **UI microcopy:** State, reason, and next action in at most two short sentences. Buttons use verb + object ("Approve plan"). Errors state what happened, why, and what to do without blaming the user. Refer to `writ serve` only when the user must act on the server; otherwise say Writ.
- **Error messages:** First line states what failed with IDs; second line states what to do. Keep the class prefix (`error: saved-plan:`).
