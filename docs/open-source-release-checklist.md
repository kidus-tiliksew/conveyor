# Open-source release checklist

Audit snapshot: 2026-08-19.

This is the go-live checklist for publishing Conveyor's source. Do not change
the repository visibility until every release blocker is checked and the final
candidate has passed the release gate.

## Current posture

- [ ] Choose the publication path. Prefer a new public repository seeded from
  a reviewed commit. Making the existing repository public would also expose
  335 branches, 334 closed pull requests, 314 issues, Actions history, and the
  existing release assets. If preserving that history is required, audit all
  of it before changing visibility.
- [ ] Sync and freeze the exact release candidate. The audited local checkout
  was at `bbc238f275364f10e705c396cb8b35cfa1278f4f`; GitHub `main` was 42 commits
  ahead at `db92c0886945869dce2517edd8bdfde1d1835455`.
- [ ] Keep the release branch clean. The audited checkout had an untracked
  19 MiB root-level `conveyor` executable. Remove it from the release checkout
  and ignore `/conveyor` so it cannot be committed by accident.

GitHub reported the repository as private, 44,329 KiB, and 28% complete by its
community-profile measure. It had no detected license, no branch protection,
and no enabled Dependabot, code-scanning, or secret-scanning alerts. The latest
release was `v0.6.0`; release assets were mutable.

## Release blockers

### Legal ownership and licensing

- [x] Use the MIT License, approved by the project owner.
- [x] Add `LICENSE` at the repository root and include it in every source and
  binary release archive.
- [ ] Decide whether source files require SPDX identifiers or copyright
  headers, then apply the rule consistently.
- [ ] Confirm that every contributor has the right to license their work.
  Choose DCO sign-off, a CLA, or an explicit no-CLA policy and document it.
- [ ] Audit third-party code, generated dashboard bundles, fonts, icons, and
  image assets. Record required attributions in `NOTICE` or a third-party
  notices file. The web dependency tree includes OFL-1.1 fonts and a package
  available under MPL-2.0 or Apache-2.0, so this cannot be treated as an
  MIT-only inventory without review.
- [ ] Define the trademark policy for the Conveyor name, wordmark, and logo.
  A source-code license does not grant trademark rights by itself.

### Public-history and privacy review

- [ ] Run a full-history secret scan across every ref intended for publication
  with at least two maintained scanners, such as Gitleaks and TruffleHog.
  Review findings manually and rotate any credential that ever entered Git,
  even if a later commit deleted it.
- [ ] Audit content that Git scanners do not cover: issues, pull requests,
  review comments, Actions logs and artifacts, release descriptions and
  assets, wiki/project content, and branch names.
- [ ] Audit commit authorship and contributor consent before publishing names
  and email addresses.
- [x] Remove the obsolete `design-qa.md` report and its local machine paths.
- [ ] Remove or rewrite private operational links and reconciliation evidence
  that will not be available to public contributors.
- [ ] Delete stale remote task branches before an in-place visibility change,
  or exclude them by publishing a clean repository.

The current tracked tree had no match for the small credential-pattern set
used during this audit. That check was not a substitute for a full-history
scanner. No secret values were read from the ignored local `.env` file.

### Make the design authority public

- [ ] Decide how public contributors can read the confirmed Requirements,
  System Design documents, and DEC decisions that govern the code. They are
  the project's design authority, but they are not present as a complete
  public corpus in this repository.
- [ ] Publish a versioned, read-only export of the confirmed corpus, or operate
  a public read-only Conveyor workspace with durable URLs.
- [ ] Add a documented contribution path for proposing authority changes.
  Explain what contributors can submit, who confirms proposals, and how a
  change proceeds when an external contributor cannot access the operator
  environment.
- [ ] Make the public corpus and code release refer to the same version. A
  contributor must be able to identify the exact requirements and decisions
  that governed a tagged release.

### Security before exposure

- [ ] Add `SECURITY.md` with supported versions, a private reporting channel,
  response targets, disclosure expectations, and credit policy. Verify that
  the reporting channel works before publishing it.
- [ ] Write a deployment-hardening guide. Cover TLS termination, reverse-proxy
  trust, database isolation and backups, token creation and rotation, the
  plaintext local credential-store model, forge machine accounts, least
  privilege, log retention, and safe worker placement.
- [ ] Publish a threat model for the REST API, MCP endpoint, dashboard,
  invitation and sign-in links, personal tokens, deployment credentials,
  agent-supplied artifacts, GitHub access, worktrees, and model-provider calls.
- [ ] Verify that production configuration fails closed. Example credentials
  must remain local-only, services must not become internet-facing by default,
  and logs and error responses must not expose tokens, prompts, or attachment
  contents.
- [ ] Enable GitHub secret scanning and push protection before accepting public
  contributions.
- [ ] Enable Dependabot alerts and security updates for Go, npm, GitHub
  Actions, and Docker dependencies.
- [ ] Add CodeQL or an equivalent Go and JavaScript/TypeScript code-scanning
  workflow. Add `govulncheck`, `npm audit` or OSV scanning, and a secret scan to
  CI.
- [ ] Pin every third-party GitHub Action to a reviewed commit SHA. The release
  workflow does this; the CI workflow currently uses floating major tags.
- [ ] Restrict allowed Actions and require SHA pinning in repository settings.

### Protect changes and releases

- [ ] Protect `main`. Require the build/static, Go/web/Playwright, and
  PostgreSQL integration checks; require an up-to-date branch and at least one
  non-author approval; dismiss stale approvals; block force pushes and branch
  deletion; apply the rules to administrators.
- [ ] Protect release tags or use a release environment with required approval.
- [ ] Make the release workflow publish only a commit that passed the complete
  CI gate. A pushed `v*` tag currently builds and publishes independently of
  the pull-request and `main` test jobs.
- [ ] Test the exact archives that will be uploaded, not a second local build.
  Exercise both binaries on all four advertised targets or document the limits
  of cross-platform validation.
- [ ] Include `LICENSE`, `NOTICE`, the relevant README, and release metadata in
  each archive. Current archives contain only `conveyor` and `conveyord`.
- [ ] Generate an SPDX or CycloneDX SBOM for each release.
- [ ] Publish signed provenance and sign the archives or checksum manifest.
  A checksum downloaded from the same mutable GitHub release is useful for
  transfer errors but does not establish publisher identity.
- [ ] Make published releases immutable after the release process and rollback
  procedure have been tested.
- [ ] Document who may tag, publish, yank, or deprecate a release. Require
  hardware-backed MFA for maintainers with release access.

## Contributor-facing repository

### Community files

- [ ] Add `CONTRIBUTING.md` with prerequisites, setup, architecture entry
  points, generated-file rules, the dedicated-worktree convention, test
  commands, commit expectations, and the pull-request/review path.
- [ ] Add a code of conduct and name its enforcement contacts.
- [ ] Add `GOVERNANCE.md` describing maintainers, decision rights, document
  confirmation, reviewer independence, release authority, and succession.
- [ ] Add `SUPPORT.md` separating community support, security reports, and
  operational incidents.
- [ ] Add `CODEOWNERS`, issue forms, a pull-request template, and a security
  issue redirect. Templates should ask for requirement/design links and test
  evidence without assuming access to a private factory.
- [ ] Decide whether to enable Discussions and what belongs there instead of
  Issues.

### README and public documentation

- [ ] Correct the README's statement that tasks have no assignees. Current
  policy allows an assignee as a claim-eligibility constraint, without using
  assignment for queue ordering.
- [ ] State the maturity and compatibility promise plainly. Define what
  `v0.x` means for database migrations, REST/MCP contracts, configuration, and
  CLI behavior.
- [ ] Add a fresh-clone quickstart tested by someone without the maintainer's
  local configuration, credentials, cached dependencies, or existing
  database.
- [ ] Add a configuration reference with safe development defaults and a
  separate production example. Explain every credential and which process
  owns it.
- [ ] Add production deployment, backup, restore, upgrade, rollback, and
  disaster-recovery guides. State whether containers or managed deployment
  artifacts are supported; the current Compose files provide PostgreSQL, not
  a complete application image.
- [ ] Publish REST and MCP compatibility/versioning documentation and a stable
  command reference.
- [ ] Add a changelog and release notes policy. Define semantic-versioning and
  deprecation rules.
- [ ] Add a privacy and telemetry statement. If Conveyor sends no telemetry,
  say so and distinguish that from data sent to configured model providers and
  GitHub.
- [ ] Replace or explain internal task IDs, phase language, and dogfood links
  where they obscure the public setup or maintenance model.

## Candidate validation

- [ ] Run `make fmt-check`.
- [ ] Run `make build`.
- [ ] Run `make vet`.
- [ ] Run `make test`.
- [ ] Run `make test-integration` against a disposable PostgreSQL instance.
- [ ] Run `make plugin-check`.
- [ ] Run `make test-release`.
- [ ] Run `git diff --check` and verify a clean worktree.
- [ ] Run the dependency-license, vulnerability, secret, and CodeQL gates on
  the exact candidate commit.
- [ ] Install the candidate archives on clean Linux amd64, Linux arm64, macOS
  amd64, and macOS arm64 environments. Verify checksums/signatures, first-run
  setup, migration, CLI authentication, MCP registration, worker pairing, and
  uninstall/rollback behavior.
- [ ] Upgrade a copy of a supported existing database to the candidate, verify
  its data and event lineage, then test the documented rollback boundary.
- [ ] Have an external contributor follow the public docs and submit a small
  change without private knowledge or maintainer help.

Audit-time checks on the older local commit produced these results:

- `git diff --check`, `make fmt-check`, and `make plugin-check` passed.
- `make test-release` passed, including release pinning and checksum-failure
  behavior.
- `go test ./...` failed in `cmd/conveyor` on three skill-installation tests.
  The current GitHub `main` commit was 42 commits newer and its latest CI run
  passed, so rerun the local failure on the synchronized release candidate
  before classifying it as a product defect.

## Go-live gate

The repository is ready to become public only when all of the following are
true:

- [ ] The license, contribution rights, trademark policy, and third-party
  notices are approved.
- [ ] The chosen Git history and all associated GitHub metadata have passed
  secret, privacy, and rights review.
- [ ] Public contributors can read and propose changes to the governing
  document corpus.
- [ ] Security reporting, scanning, branch protection, and release controls
  are active.
- [ ] The exact candidate commit and release artifacts pass every validation
  item above.
- [ ] A maintainer who did not prepare the release independently signs off on
  the evidence.
- [ ] The visibility change or clean-repository publication has a rehearsed
  rollback and communications plan.
