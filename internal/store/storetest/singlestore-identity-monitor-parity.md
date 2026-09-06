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
