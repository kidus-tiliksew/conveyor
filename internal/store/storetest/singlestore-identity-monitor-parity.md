# SingleStore identity and monitor parity review

Task `260905-25e2bf` implements the identity and monitor aggregates under
DEC-38, component-persistence v4, component-identity-membership v3, and
component-monitor-drift v1. PostgreSQL remains unchanged and defines existing
reference behavior. This note records source comparisons and proposed suite
additions; it does not establish SingleStore SQL conformance.

## Existing assertions and reference behavior

The Identity, Membership, InvitationSessions, and Tokens assertions in
`identity.go` agree with the PostgreSQL paths inspected for bootstrap rotation,
normalized provisioning, role capabilities, the last-operator guard, invitation
redemption, one-use links, password proof, revocation, forge encryption, and
redaction. No failing PostgreSQL assertion in these suites motivated a contract
change. The implemented SingleStore factory enables those capabilities and
removes those four names from its experimental Skip list.

Several reference details are not pinned by those assertions:

- PostgreSQL derives PAT scope from current bindings but returns persisted
  bearer scope during verification. Deployment authorization consults live
  bindings again. SingleStore preserves those separate decisions.
- `GrantWorkspaceRole` returns email and role without populating the other
  `MembershipGrant` fields, even after creating an invitation. SingleStore
  preserves that result. A future assertion should pin it before callers rely
  on additional fields.
- Listing human PATs includes revoked credentials and orders by creation time
  descending, then id ascending. The query does not select a hash. SingleStore
  uses the same projection and ordering.
- Issuing a sign-in link rotates every unredeemed link for that email.
  `RedeemSignInLink` returns the common invalid-credential error for every
  transaction failure, including an audit failure. SingleStore preserves both
  behaviors.
- The password limiter lives in `internal/httpapi/password_auth.go`, not in
  either store. Its five-attempt, fifteen-minute, hashed-email and source-address
  behavior remains covered by HTTP tests in the ordinary local gate.

## Monitor workspace discrepancy and proposed tightening

`component-monitor-drift` states that stores refuse a record whose workspace
is different from the request context. The memory implementation checks the
record's `WorkspaceID`. PostgreSQL's `Observe` and `RecordDrift` use only
`workspace(ctx)` in SQL, ignoring the record field. The current Monitor suite
always supplies matching values and cannot detect that discrepancy.

SingleStore rejects an explicitly mismatched record workspace, as the approved
plan's workspace-consistency rule requires. A future shared case should cover
matching, missing, and mismatched record workspaces. Enabling that case across
all backends requires settling the PostgreSQL discrepancy through the existing
reference-behavior process; this task does not change PostgreSQL to fit it.

## Additional conformance proposals

Move the following backend-specific assertions into shared suites once their
public-API setup can prove the same behavior on every backend:

- Parallel provisioning of a normalized email produces one account.
- Concurrent demotion or revocation cannot remove both remaining operators.
- Rotating a sign-in link invalidates the earlier value, and concurrent
  redemption has one winner.
- Session refresh advances expiry by seven days; inactive owners and
  cross-owner session identifiers fail authentication and profile mutation.
- A forge key is defensively copied, independent between Store instances,
  and safe under concurrent configuration and use. Owner-bound authenticated
  encryption refuses changed owners, damaged ciphertext, and invalid nonces.
- An audit insertion failure rolls back its identity mutation.

SingleStore's added tests cover these cases where their fixture permits them.
Pure rule tests cover the deployment-credential constraint, singleton
organization, and sixth-unresolved-drift refusal. The integration target is
still required to establish the SQL behavior.

## Deferred sibling-dependent coverage

The shared Monitor suite constructs a task before testing any monitor method.
TaskFilter, TaskAssigneeMembership, and ForgeAuthorIdentity also require
sibling-owned task and publication methods. They remain explicitly skipped,
following operator feedback. SingleStore has independent monitor integration
tests for observations, deduplication, status, activity, isolation, and signal
locks. They do not substitute fake task implementations or seed task rows to
bypass the missing task lifecycle.

ProductionCapable remains false. The admission task owns removing the final
Skip list after the sibling aggregates have merged.

## Implement-5 validation, 2026-09-06

The resumed order validated the preserved implementation at `0bc5121d`, which
contains implementation commit `456a4acc` and main's PR 856 calendar clock fix.
No PostgreSQL source, migration, admission flag, or sibling-owned API changed.
The only foundation-file change is the operator-authorized embedded
`forgeEncryptionState` field in `Store`.

Validation follows component-verification-strategy v4 and DEC-29. The current
served persistence baseline is v8; identity-membership v3 and monitor-drift v1
govern the aggregate behavior. Existing component-persistence proposal v7,
proposal event `339419`, belongs to this task. The delivered snapshot marks it
confirmed and records v8 superseding it. This resumed order references v7
without creating a duplicate proposal. No new identity behavior required an
identity-membership proposal.

| Check | Result |
| --- | --- |
| `make fmt-check`, `make build`, `make vet` | Passed |
| `make test-singlestore-unit` | Passed |
| `make test` with standalone Node 24.13.0 on PATH | Passed; Go, installer, bundle, Biome and all 241 Playwright tests, 1.9m browser time |
| `make test-integration-singlestore-ci` | Passed; s2log 4.137s, SingleStore 153.510s |
| `GOFLAGS='-run=TestPostgresConformanceIntegration -v' make test-integration` | Passed; RunAll 43.861s, PostgreSQL package 44.408s |
| `make test-integration` | Passed; CLI 93.860s, PostgreSQL 131.066s, dispatch 2.258s |
| `GOFLAGS='-race -v -run=TestForgeEncryption\|TestMemoryConformance -count=1' make test-singlestore-unit` | Passed; memory RunAll 5.714s and forge-key race tests passed |

The SingleStore target used the operator-named `ff-infra-singlestore-1` on
`127.0.0.1:25901`. The initial run found the required `conveyor_test` database
missing. Initializing that empty test database allowed the unchanged target
to pass. Each fixture created and dropped its own `conveyor_*_test` database.
The root password remained in process memory and was never written to an
artifact. No additional SingleStore container was started.

The default Node 26.3.1 run passed the Go tier but Vite aborted with
`Lazy deopt after a fast API call with return value is unsupported` during
Playwright. The installed Homebrew Node 22 could not start because its linked
`libsimdjson.29.dylib` was missing. The successful full rerun used
`PATH="$HOME/.nvm/versions/node/v24.13.0/bin:$PATH" make test`. No repository
runtime configuration or web source changed to work around the host failures.

The predecessor recorded the inherited PostgreSQL five-minute package timeout
and `TestClaimedVerificationEvidenceUploadIntegration` failure at
`phase47_integration_test.go:585`, `expired claim error=<nil>`. Main CI run
[34016242660](https://github.com/kidus-tiliksew/conveyor/actions/runs/34016242660)
independently shows the package timeout on `ecf7ce9a`. That log does not prove
the expired-claim assertion; its evidence is the predecessor's local run and
the operator's explicit baseline classification. Both failures are historical:
this order's full PostgreSQL gate passed, and the latest main run
[34020846321](https://github.com/kidus-tiliksew/conveyor/actions/runs/34020846321)
also passed PostgreSQL on `d16a1940`.

The SingleStore job in that latest main run failed before checkout because
`ROOT_PASSWORD` was absent when creating its service container. The operator
reports the repository secret `SINGLESTORE_ROOT_PASSWORD` is unset. Local SQL
validation is the SingleStore evidence until the operator configures that
secret; this change does not claim a green SingleStore CI job or production
admission.

The plan's owned-method, singleton, credential, session, encryption, audit,
membership and independent-monitor criteria have implementation and passing
tests. Sixth-unresolved-drift refusal has a pure rule test; task-dependent
drift linking and resolution remain deferred under the operator's explicit
coverage decision. TaskFilter, TaskAssigneeMembership, ForgeAuthorIdentity and
the shared Monitor suite remain in Skip. The parity findings and proposed
shared-suite additions above preserve PostgreSQL reference behavior. No child
tasks or decomposition were created.
