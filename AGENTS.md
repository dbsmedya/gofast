# AGENTS.md

Conventions for AI agents and contributors working in **gofast**
(`github.com/dbsmedya/gofast`). Engineering rules live in
[CLAUDE.md](CLAUDE.md); this file covers where things go on disk.

## `.ayder/` — internal scratch, never tracked

`.ayder/` is the **internal scrap directory**. It holds working material that
belongs to the team, not to the published repository: planning docs, product
notes, salvaged drafts, release notes, and agent workflow artifacts.

**It must never be tracked by git.** It is ignored via `.gitignore`
(`.ayder/`), and that rule must stay. Specifically:

- Never `git add` a path under `.ayder/`, and never `git add -f` one.
- Never remove or narrow the `.ayder/` entry in `.gitignore`.
- Never move a file out of `.ayder/` into a tracked directory just to make it
  committable. If content genuinely belongs to the public repo, rewrite it as
  public documentation under `docs/` — don't relocate an internal draft.
- Treat its contents as internal: they may quote customers, infrastructure, or
  unreleased plans.

If a tool reports a file under `.ayder/` as untracked or ignored, that is the
correct state, not a problem to fix.

## `docs/` — public documentation only

`docs/` is part of the published repository. It contains **only** documentation
written for users and integrators:

| file | covers |
|---|---|
| `README_DIGEST.md` | the `digest` report — flags, columns, JSON schema, limits |
| `README_SQL.md` | schema, indexes, and example SQL for `query` |
| `README_API.md` | the read-only machine REST API |
| `README_PARSER.md` | parser internals and fingerprinting |

Do not add specs, implementation plans, brainstorming output, TODO lists,
status reports, or session notes to `docs/`. Those are internal — see below.

## Superpowers artifacts — `.ayder/superpowers_<YYYYMMDD>/`

Specs and plans produced by the superpowers workflow (brainstorming →
writing-plans → executing-plans) are **workflow artifacts, not documentation**.
They live under a dated directory in the scratch area:

```
.ayder/superpowers_<YYYYMMDD>/
├── specs/YYYY-MM-DD-<topic>-design.md
└── plans/YYYY-MM-DD-<feature-name>.md
```

for example `.ayder/superpowers_20260725/specs/2026-07-25-pt-query-digest-design.md`.

The `<YYYYMMDD>` suffix is the date the batch of work started, so successive
efforts archive side by side rather than overwriting each other.

**They do not go in `docs/`,** and they do not go in `docs/superpowers/` — that
was the previous convention and is retired. The `.gitignore` entry for
`docs/superpowers/` is kept only as a guard against tools that still default
there.

Because these artifacts are untracked, **nothing in tracked code or docs may
link to them.** A source comment or public doc that cites a spec path would
dangle for anyone who clones the repo. Cite the public doc instead — e.g.
`internal/digest` points at `docs/README_DIGEST.md`, not at its design spec.

## Release notes

Per-version notes are internal drafts at `.ayder/version_<X.Y.Z>.md`, following
the format of the existing files there. They are the source for the published
release description; they are not committed.

## Before you touch code

The full rules are in [CLAUDE.md](CLAUDE.md). The ones that break a build if
missed:

- **`pkg/` is the public contract.** Exported-signature or behavior changes are
  versioned events (breaking = major, additive = minor). Don't change it
  casually.
- **Build requires `CGO_ENABLED=1`** (DuckDB uses cgo). Toolchain **go 1.27.4**.
- **The stable contract is the REST machine API + `config.yaml` schema**,
  specified by `pkg/api/openapi.yaml`. `scripts/run-contract-check.sh` must stay
  green.
- **No secrets; `gitleaks` must pass.**
