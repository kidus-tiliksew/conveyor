# Authoring a testing-strategy System Design document

Citation scope: the REQ, AC, DEC, and component IDs in this guidance, and its
other requirement, design, and reference IDs, cite the Conveyor product's own
design corpus, not the corpus of the factory you work in. Task work cites the
working factory's own confirmed corpus, as the delivered contract directs.

A testing-strategy System Design document tells reviewers how to judge
verification for a defined scope. Confirmed DEC-53 makes reviewers judge
verification adequacy against that document, and against sealed verify-stage
evidence when a task's frozen `verify_stage` is on, while confirmed DEC-28
requires the result
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

The tables below are this repository's example, not a template: they record the
Conveyor repository's own surfaces. Its testing strategy,
`component-verification-strategy`, is the model document for the result. It
names each tier, the CI job that evidences it, and what a green job proves.
Discover another repository's surfaces instead of copying these rows.

| Tier | Entrypoint | What it establishes |
| --- | --- | --- |
| Build and static analysis | `make build`, `make vet`, `make fmt-check` | The Go binaries and dashboard build; Go vet; Go formatting. The dashboard build performs TypeScript checking and Vite compilation. |
| Ordinary tests | `make test` | Its prerequisites (`compose-check`, `dashboard-fresh`, `test-release`, `test-validation`, `test-validation-environment`, `vk10-runtime`), then Go tests with both database URLs empty, Biome lint/format checks, and Playwright browser scenarios. |
| CI split of ordinary tests | `make test-go`, `make test-playwright-shard PLAYWRIGHT_SHARD=<i>` | `make test-go` runs the same prerequisites, the Go tests, and Biome lint. The four Playwright shards together run the Playwright suite. |
| Focused web checks | `make test-web` and `web/package.json` | Explicit TypeScript typechecking, Biome lint/format checking, and Playwright end-to-end tests. These checks do not establish database behavior. |
| PostgreSQL integration | `make test-integration`, `make test-integration-ci`, `POSTGRES_INTEGRATION_SHARD` | The PostgreSQL suites in three shards: `store` (`internal/store/postgres`), `cli` (`cmd/conveyor`), and `daemon` (`cmd/conveyord` and `internal/dispatch`). `make test-integration` runs them against a container one managed validation invocation creates, seals in a durable inventory, and removes by ID. `make test-integration-ci` runs them against the database URL and service CI supplies and rejects a missing URL. |
| SingleStore integration | `make test-integration-singlestore-ci`, `SINGLESTORE_INTEGRATION_SHARD` | The SingleStore suites in four shards against a configured SingleStore server: `conformance-1` and `conformance-2` (complementary halves of the `internal/store/singlestore` conformance test), `packages` (`internal/eventlog/s2log`, `internal/store/storetest`, and the rest of `internal/store/singlestore`), and `commands` (the SingleStore command tests in `cmd/conveyor` and `cmd/conveyord`). The target rejects a missing URL. |
| Image | `make test-image IMAGE=<tag>` | The container image reports its version and carries `conveyor` and `git` but not `gh`. |
| Validation resources | `make test-validation`, `make test-validation-docker` | Owned process-group, container, network, database, and temporary-path lifecycles: concurrent isolation, cleanup on every exit class, explicit recovery after forced termination, and refusal of active or identity-mismatched resources. CI runs `make test-validation` through `make test-go`, and does not run `make test-validation-docker`. |
| Local aggregate | `make validate` | One shared dependency install and bundle build for build, vet, formatting and the complete ordinary aggregate in one local session; standalone target contracts remain intact. |

`.github/workflows/ci.yml` runs these jobs on every pull request and every push
to `main`. The Makefile is the only definition of the shards: the `plan` job
prints its shard lists with `make ci-shard-matrix`, and each matrix entry is
its own job record.

| Job (`ci.yml` key) | Matrix | Runs |
| --- | --- | --- |
| Build container image (`container-image`) | None | The image build, then `make test-image` |
| Build, vet, and formatting (`build-and-static`) | None | `make build`, `make vet`, `make fmt-check` |
| Plan CI shards (`plan`) | None | `make ci-shard-matrix` |
| Go tests and web lint (`go-test`) | None | `make test-go` |
| Playwright tests (`playwright`) | Shards `1` to `4` | `make test-playwright-shard` |
| PostgreSQL integration tests (`postgres-integration`) | `store`, `cli`, `daemon` | `make test-integration-ci` for the shard |
| SingleStore integration tests (`singlestore-integration`) | `conformance-1`, `conformance-2`, `packages`, `commands` | `make test-integration-singlestore-ci` for the shard |

For every gate a CI job runs, exact-head CI is the review evidence: the record
of every `ci.yml` job on the task's pull request at the exact reviewed head
commit (DEC-72). Implementers do not run `make validate`,
`make test-integration`, or `make test-integration-singlestore-ci` locally to
satisfy review. Local runs serve focused iteration, CI-failure diagnosis, and
gates CI does not run, such as `make test-validation-docker`. A green job
proves only what its row states.

`web/tests`, `cmd/conveyor/testdata`, Go `httptest` servers, temporary Git
repositories, and the database-backed suites are reproducible fixtures worth
naming when they cover the proposed scope. Typecheck, lint, formatting, and
build success find structural defects; only executable tests establish
behavior, and only the PostgreSQL and SingleStore tiers establish
database-backed behavior on their backends. Anything outside those surfaces
remains unverified until an authenticated surface or reproducible fixture
exists.

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
the reviewer must not demand impossible evidence (`req-review-gates-evidence`
REQ-7, AC-7.1 and AC-7.2).

For local evidence equivalence, document the audited input boundary and use the
repository's own record/check/bind evidence workflow, such as
`docs/validation-evidence.md` in the Conveyor repository. Distinguish
a fresh execution from a reused result bound to a pushed head. Require matching
before/after and current inputs, complete tool/runtime and configuration
inventories, durable logs, and a reason Git metadata is irrelevant before
carrying evidence across a commit. Unknown inputs, changed dependencies or
bundles, authored conflicts, or missing evidence require fresh validation.
Database, CI, and independent-review boundaries remain separate; an evidence
manifest does not change the permitted verification-evidence artifact role.

## Be honest about gaps

List unavailable browsers or platforms, unexercised failure modes, external
provider behavior replaced by fakes, absent performance or migration fixtures,
and authenticated observations the product does not expose. For each gap,
identify the claim that cannot be made and any narrower evidence that remains
valid. A known-gaps section protects judgment quality; it is not permission to
describe an unrun tier as passing.

## Preserve the decision boundary

The document guides human and agent reviewer judgment. It is not a mechanical
gate and must not grow an enforcement grammar. DEC-53 keeps adequacy a
reviewer judgment and rejects a mandatory evidence gate on every submission;
changing that posture requires a new confirmed decision, not inventive fields
in a testing document.

Use DEC-28's house style in this playbook and every document it produces:
prefer narrow scope, citable and decidable claims, and a dense normative core.
For creation, revision, proposal, and operator confirmation mechanics, follow
the `conveyor-plan` skill's `conveyor-planning.md` playbook. Do not duplicate
its endpoints or bypass its propose-to-confirm authority boundary.
