# Authoring a testing-strategy System Design document

A testing-strategy System Design document tells reviewers how to judge
verification for a defined scope. Confirmed DEC-29 makes that document the
standard for verification adequacy, while confirmed DEC-28 requires the result
to be dense, decidable, and tool-neutral. Discover the repository first; a
strategy inferred from generic testing practice is boilerplate, not authority.

## Discover before writing

Read the repository surfaces that actually define verification and record what
each one proves:

1. Inventory build and test targets in `Makefile` and any nested build files.
   Follow target dependencies rather than trusting target names.
2. Inventory package scripts and tool configuration. Separate compilation or
   typechecking, lint and formatting, executable tests, and generated-output
   freshness checks; none is evidence for another category unless the command
   really composes it.
3. Read every CI workflow that protects the scope. Record its job boundaries,
   environment and service wiring, and whether CI runs a different entrypoint
   from local development.
4. Locate fixtures, `testdata`, browser scenarios, fake servers, database
   setup, and other integration entrypoints. State whether they reproduce the
   relevant boundary or replace it with a narrower fake.
5. Map the scoped change kinds to the checks that exercise them. Mark missing
   coverage explicitly instead of assigning a plausible command.

For this repository, the inventory begins with these concrete surfaces:

| Tier | Entrypoint | What it establishes |
| --- | --- | --- |
| Build and static analysis | `make build`, `make vet`, `make fmt-check` | The Go binaries and dashboard build; Go vet; Go formatting. The dashboard build performs TypeScript checking and Vite compilation. |
| Ordinary tests | `make test` | Compose-isolation validation, generated-dashboard freshness, release-install fixtures, Go tests with PostgreSQL disabled, Biome lint/format checks, and Playwright browser scenarios. |
| Focused web checks | `make test-web` and `web/package.json` | Explicit TypeScript typechecking, Biome lint/format checking, and Playwright end-to-end tests. These checks do not establish PostgreSQL behavior. |
| Local integration | `make test-integration` | The PostgreSQL suites in `cmd/conveyor`, `internal/store/postgres`, and `internal/dispatch` against an isolated per-worktree database that the target starts and removes. |
| CI integration | `make test-integration-ci` | The same PostgreSQL suites against the database URL and PostgreSQL service supplied by CI; the target deliberately rejects a missing URL. |

`.github/workflows/ci.yml` composes those into separate build/static,
Go/web/Playwright, and PostgreSQL integration jobs. `web/tests`,
`cmd/conveyor/testdata`, Go `httptest` servers, temporary Git repositories, and
the database-backed suites are reproducible fixtures worth naming when they
cover the proposed scope. Typecheck, lint, formatting, and build success find
structural defects; only executable tests establish behavior, and only the
PostgreSQL tier establishes database-backed behavior. Anything outside those
surfaces remains unverified until an authenticated surface or reproducible
fixture exists.

## Use a stable document skeleton

Write the System Design document as a description of the current verification
mechanism, not a rollout plan. Keep these sections, omitting only examples that
do not exist for the scope:

1. **Purpose and scope** — the behavior and repository surfaces whose testing
   strategy the document governs.
2. **Test taxonomy and commands** — each tier, its exact supported entrypoint,
   prerequisites, fixtures, and isolation boundary.
3. **Verification by change kind** — the minimum meaningful tiers for schema,
   persistence, handler/API, CLI, UI, CI/configuration, documentation, and
   cross-layer changes that occur in the scope.
4. **Evidence and reviewer judgment** — what output or artifact lets a reviewer
   establish that the relevant tier ran against the intended surface.
5. **Known gaps** — behaviors no current command, authenticated surface, or
   reproducible fixture can establish, plus the narrower claims reviewers may
   still accept.
6. **Governed paths** — exactly one fence of this shape, containing only the
   narrow paths described by the document:

```conveyor:governs
- repo: conveyor
  paths:
    - path/to/tests/**
    - .github/workflows/relevant-ci.yml
```

The fence is mandatory and unique. Governed scopes arm Conveyor's drift signal:
a merge touching a scoped path without a proposed design revision is surfaced
as drift. Prefer test directories, relevant CI files, fixtures, and the exact
implementation surfaces whose strategy the prose prescribes. Do not use a
whole-repository glob as a claim that one testing document understands every
verification obligation.

## Define what verified means

State the judgment rule per real change kind. A useful matrix names the change,
the required tier, any conditional tier, and the remaining uncertainty. For
example:

- A schema, query, transaction, or queue change needs the PostgreSQL integration
  tier; an in-memory or PostgreSQL-disabled Go test cannot establish database
  semantics.
- A handler or CLI change needs focused Go behavior tests and the ordinary
  aggregate; add database integration when the path crosses persistence and a
  browser scenario when user-visible behavior crosses the dashboard boundary.
- A UI change needs typechecking and Biome for static confidence, plus a
  Playwright scenario for behavior. Static checks alone do not verify rendering
  or interaction.
- A CI or build change needs the affected target locally where reproducible and
  the actual CI job result when service wiring or runner behavior is material.
- A documentation-only change may be verified by repository consistency and
  formatting checks only when it makes no executable claim; say so explicitly.

Phrase evidence as reviewer guidance: name the command, relevant result, fixture
or authenticated response, and relationship to the changed surface. Evidence
must come from repository-backed reproducible fixtures or the deployment's own
authenticated API, MCP, metrics, logs, or response metadata. Never require
deployment-host access. If a production-shaped quantity is necessary but not
surfaced, exposing it or defining a reproducible fixture is work to be planned;
the reviewer must not demand impossible evidence (REQ-7 and AC-7.1/AC-7.2).

## Be honest about gaps

List unavailable browsers or platforms, unexercised failure modes, external
provider behavior replaced by fakes, absent performance or migration fixtures,
and authenticated observations the product does not expose. For each gap,
identify the claim that cannot be made and any narrower evidence that remains
valid. A known-gaps section protects judgment quality; it is not permission to
describe an unrun tier as passing.

## Preserve the decision boundary

The document guides human and agent reviewer judgment. It is not a mechanical
gate and must not grow an enforcement grammar. DEC-29 explicitly rejected
mechanical per-scope enforcement for now; changing that posture requires a new
confirmed decision, not inventive fields in a testing document.

Use DEC-28's house style in this playbook and every document it produces:
prefer narrow scope, citable and decidable claims, and a dense normative core.
For creation, revision, proposal, and operator confirmation mechanics, follow
[`conveyor-planning.md`](conveyor-planning.md). Do not duplicate its endpoints
or bypass its propose-to-confirm authority boundary.
