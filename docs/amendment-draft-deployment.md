# DRAFT amendment — §21.54 v2.14 — Deployment & multi-user (Phase 7)

**Status: accepted August 1, 2026 — appended to
[conveyor-spec.md](../conveyor-spec.md) as §21.54 (v2.14), with one
revision at acceptance: deployment lands as Phase 8, sequenced after
the Phase 7 memory store (memory keeps its number; the draft text below
predates that swap and says Phase 7 throughout).** The spec
text is authoritative; this file is retained as the review record,
including the non-normative Appendix A prior-art survey of qm's
identity and token model, which was dropped on acceptance — per the
precedent of
[amendment-draft-state-machines.md](amendment-draft-state-machines.md).

---

### 21.54 v2.14 — Deployment & multi-user: identity, embedded worker, delivery tiers (August 1, 2026)

Conveyor today deploys as a developer artifact, not a product. The gaps
are structural, not cosmetic:

- **There is no user.** One shared bearer (`CONVEYOR_API_TOKEN`)
  authenticates every REST and MCP caller; `events.actor` and
  `interventions` can only ever say "the operator." §18.1 promised a
  pluggable identity interface and actor/role on every event as the
  enterprise hook; nothing yet fulfills it. Two people cannot share an
  installation accountably.
- **The control plane is half-packaged.** The worker earned launchd/
  systemd service management in Phase 5.5 (§6.5); `conveyord` is still
  a foreground process started by hand from a source checkout, with no
  release, upgrade, or first-run story beyond the README.
- **A solo install pays multi-machine costs.** The §6.4 worker was
  designed for the case where execution machines are distinct from the
  control plane. A single developer on one laptop must still issue a
  pairing token to enroll a worker against `localhost` — pure ceremony.
- **GitHub is a de-facto hard dependency.** §11's factory coordination
  is well-bounded (fine-grained PAT, no GitHub App), but the pipeline's
  terminal states assume a PR exists. The surveyed agent-harness field
  (Codex CLI, Claude Code) treats the local repository as the primary
  object and the forge as optional; Conveyor should too, since its
  review gate is already in-factory (§13.1, Phase 5.2/5.4) rather than
  PR-native.

This amendment accepts **Phase 7 — deployment & multi-user**, sequenced
immediately after Phase 6. The deferred phases renumber, as they did
when Phase 6 was inserted (§21.46): memory → 8, flywheel → 9,
managed-execution reintroduction → 10, enterprise → 11 — all unstarted,
so the renumbering costs nothing — and change 8 re-scopes the
enterprise phase to what remains genuinely enterprise. Two deployment personas are served by one design:
the solo operator on their own machine (zero-login, zero-pairing) and
the small team sharing a server install (named users, simple roles).
Multi-tenancy is a non-goal: one deployment is one organization.

Eight changes:

1. **Phase 7 enters the roadmap (§19); deferred phases renumber.**
   Sub-phases, in order:

   - **7.1 Embedded worker** — smallest slice, immediately removes
     solo-install ceremony; no schema dependency on identity.
   - **7.2 Identity, grants, and credentials** — users, admin grants,
     sessions/PATs, the credential-class boundary, actor threading.
   - **7.3 Delivery tiers** — GitHub becomes one of three per-repo
     delivery modes; lifecycle terminal states defined without a PR.
   - **7.4 Packaging & first-run** — `conveyord` service install,
     versioned releases, `conveyor init`, server compose stack.

   The roadmap table gains a Phase 7 row with these deliverables; the
   rows for memory, flywheel, managed execution, and enterprise
   renumber to 8–11, and every body reference to those phase numbers
   (§7.2, §12, §13.1, §14, §15, §18.1, §19) updates accordingly — a
   mechanical renumbering with no scope change, mirroring §21.46. The
   enterprise row (now Phase 11) is re-scoped by change 8. §19's
   sequence paragraph records that 7.1 may land during late Phase 6
   (it touches only §6.4 supervision), while 7.2–7.4 follow Phase 6
   closure.

2. **Users and admin grants (§10, §16, §18.1).** A `users` table joins
   the core schema: id, email (unique, case-insensitive), display name,
   status (`active` / `deactivated`), optional password hash, created
   timestamp. Authorization is deliberately minimal — two effective
   roles:

   - **Member** is the default state of existing: any active user may
     do everything the single operator can do today *except* the
     admin-only operations below.
   - **Admin is a grant, not a column**: an `admin_grants` row
     `(user_id, granted_by, created_at)`, revocable, with every grant
     and revocation writing an audit event naming both parties. The
     admin-only surface: user creation/deactivation, grant management,
     workspace creation/deletion, deployment-level config, and worker
     credential revocation. Workspace config writes (§2.1) remain
     member operations — the team runs the factory; admins govern who
     is on the team.
   - **Bootstrap is a seed, not a signup.** Deployment config (or
     `conveyor init`, change 7) names the initial admin; the seed
     applies only when the user store is empty. There is no
     self-registration surface.
   - **Deactivation, not deletion.** A deactivated user's credentials
     all fail closed at next use; their history (events, interventions,
     grants given) is immutable record and never cascades.

   `events.actor` becomes typed and honest: `user:<id>` for
   human-credentialed calls, `agent:<label>` for MCP agent actors,
   `worker:<id>` for enrolled workers, `system` for control-plane
   machinery. `interventions` gains the acting user. This fulfills the
   §18.1 promise ("every actor and event carries a role for later RBAC
   enforcement") with real identities behind the actor strings.
   Existing rows keep their historical actor values; no backfill.

3. **Authentication (§17.3, new §17.5).** Two deployment modes, chosen
   at first run and recorded in deployment config:

   - **Solo mode** — the zero-login path for one person on their own
     machine. `conveyord` binds loopback only (refusing to start
     otherwise in this mode), and loopback requests authenticate as the
     seeded owner without a login step, exactly as local developer
     tools behave. Switching a solo install to server mode is a
     supported, one-way `conveyor init --server` migration that forces
     real credentials before the bind address may widen.
   - **Server mode** — named users sign in. Two credential styles,
     chosen at init: **passwords** (argon2id, no external
     dependencies) or **emailed one-time sign-in links** (no password
     storage; requires a configured SMTP/API sender). One-time links
     are single-use through a Postgres-backed claim so a restart can
     never resurrect a spent link, and expire within minutes. Login is
     rate-limited per user and per source address.

   Session and API credentials are **opaque tokens, stored server-side
   only as hashes** — never JWTs. Conveyor is one binary plus the
   Postgres it already requires; database-backed tokens are instantly
   revocable and need no signing-key rotation machinery. Dashboard
   sessions are cookie-carried with idle and absolute expiry; CLI and
   MCP callers use named, per-user **personal access tokens**, listed
   and revocable on the user's settings surface. TLS is deliberately
   out of scope: server installs terminate TLS at a fronting reverse
   proxy, and the docs say so plainly.

   `CONVEYOR_API_KEY` (the deployment's model key for in-process AI,
   §10 item 1) is untouched. The shared operator bearer
   `CONVEYOR_API_TOKEN` retires as a deployment-wide secret: on
   migration it is honored as a PAT belonging to the seeded admin until
   first login, then must be rotated to per-user tokens. The worker's
   child-only `CONVEYOR_API_TOKEN` environment contract (§5.2, §10
   item 3) is unchanged — the variable name survives; the value becomes
   a per-credential token rather than a shared one.

4. **The credential-class boundary (§13.2, §17.4, §18).** Three
   credential classes exist after change 3 — **human** (session or
   PAT), **agent** (MCP bearer), **worker** (enrollment credential) —
   and the class is part of the route contract, declared per route in
   one table and enforced at the protocol boundary, not per-handler.
   Operations reserved to human credentials: gate decisions (approve /
   request-changes / reject), cancel, hold, config writes, recovery
   operations, dependency unlink (§6.3), pairing-token issuance, user
   and grant management, and PAT issuance. An agent or worker
   credential is *structurally* unable to reach these — a refusal, not
   a permission check that configuration could open. This generalizes
   the existing posture ("agents file tasks but cannot cancel them,"
   §13.2) into the system's prompt-injection firewall: no matter what
   untrusted task or PR content persuades an agent to attempt, the
   surfaces that govern people, gates, and credentials answer only to
   a signed-in human. The §17.4 MCP tool list is unchanged — it is
   already exactly the agent-class surface.

5. **Embedded worker (§6.4, §6.6 new).** `conveyord` can supervise one
   worker itself: the **same §6.4 supervisor loop, in-process** — not a
   second protocol, not a shortcut past the work-order lifecycle. The
   embedded worker enrolls through the same code path with an
   internally-issued credential (no pairing token — the exchange
   pairing exists to secure is intra-process), appears on the Workspace
   page as a worker labeled `embedded`, heartbeats, probes harnesses,
   claims through `claim_work_order` with every guard intact (hold,
   self-review, serviceability, dependency gating §6.3), spawns
   harness children with per-order fresh identity/token pairs (§6.4),
   and honors stall, first-activity, and retry-suppression semantics
   unchanged. Jobs record `dispatch: worker` with the embedded worker's
   identity. Config: `worker.embedded.enabled` with the standard
   stage-aware capacity settings; default **on** in solo mode, **off**
   in server mode (a server operator opts in, understanding the
   control-plane host then needs harness CLIs and model credentials
   installed). External workers pair and operate exactly as before;
   embedded and external workers coexist under ordinary claim
   competition. What §21.31 forbade stays forbidden: this is
   supervision placement, not a new execution plane.

6. **Delivery tiers — GitHub becomes optional (§8, §11, §12, §16).**
   Each repo declares a `delivery` mode; `github` reproduces today's
   behavior exactly, so existing workspaces migrate as-is.

   - **`github`** — factory-coordinated forge delivery, §11 unchanged:
     PR opened at `submit_for_review`, commit-status + deterministic
     comment projections, mergeability reads, merge/auto-merge via the
     forge, issue mirroring (Phase 5.3), monitor forge signals (5.6).
   - **`remote`** — a git remote, no forge API. Agents push task
     branches with the **host's ambient git credentials** (SSH keys,
     credential helper — Conveyor stores no forge token and never
     prompts for one). `submit_for_review` dispatches the in-factory
     review panel against the pushed branch head; no PR, no status
     projection. Merging is the operator's act, wherever they merge:
     completion is **ancestry-detected** — the task terminates `merged`
     when its approved head becomes an ancestor of the fetched base
     ref. With the merge gate off, approval simply marks readiness;
     there is nothing remote for the factory to merge without a forge
     API, and it does not try.
   - **`local`** — the repo URL is a filesystem path; no remote need
     exist at all. Task worktrees already share the repository's ref
     store (§8.2), so a committed task branch is visible without any
     push. Review runs against the branch head as in `remote`. Merge:
     when the base branch is **not checked out in any working tree**
     (the normal case for a server-hosted clone), the control plane
     performs a recorded ref merge itself; when it is (the solo
     operator's own checkout — §21.8's containment stands, the primary
     checkout is never mutated), approval hands off to the same
     ancestry detection as `remote`, and the operator merges in their
     own tree.

   Invariants that do not vary by tier: approval binds to the reviewed
   head SHA and merge/completion requires the current head to equal it
   (§11.2); history is never rewritten (§8.2); the evidence gate
   (§12 item 2, Phase 5.4) and adversarial panel are the review
   spine. §18's first bullet restates the trust boundary as **the
   recorded task branch head** — pushed or shared — which every gate
   judges. §12 item 1 is scoped honestly: repository CI as mechanical
   verifier is a `github`-tier property; the other tiers leant on
   evidence and the panel until the verification agent arrives with
   managed execution (Phase 10 after renumbering). §9
   webhook/poll intake and §11 forge error categories apply to
   `github`-tier repos only. Schema (§16): `repos` gains `delivery`;
   `github` coordinates and forge identifiers become optional,
   required only by the `github` tier.

7. **Packaging and first-run (§6.5, §17.1, §17.2).** The control plane
   gets the treatment the worker already has: `conveyord install |
   uninstall | status` writes a launchd agent / systemd user unit with
   restart-on-failure, start-on-boot, and documented log paths —
   supervision only, mirroring §6.5's contract, converging on repeated
   install, refusing to touch unrecognized definitions. Distribution
   becomes **versioned releases** (single static binaries per platform,
   Homebrew tap and tarball; the dashboard stays embedded via
   `go:embed`), with schema migrations applied on startup exactly as
   today — an upgrade is: replace binary, restart service. §17.2's
   first-run sequence is superseded by **`conveyor init`**: choose solo
   or server mode (change 3), seed the admin (change 2), create the
   first workspace, add a repo by local path (`local` delivery is the
   zero-config default for solo) or URL, and — in solo mode — start
   with the embedded worker already live. The solo quickstart is
   therefore: install binary, start Postgres (the documented compose
   file), `conveyor init`, create a task. No pairing, no login, no
   forge token. Server installs get a maintained compose stack
   (conveyord + Postgres) plus the reverse-proxy TLS guidance from
   change 3.

8. **The enterprise phase re-scoped (§18.1, §19).** What Phase 7 pulls
   forward is exactly the identity substrate §18.1 reserved; what
   remains deferred and demand-triggered in the enterprise phase (11
   after renumbering) is the genuinely enterprise layer **built atop
   it**: SSO/OIDC and SAML, SCIM provisioning (change 2's deactivation
   records are its landing site), RBAC beyond the two-role model
   (per-workspace roles, custom roles), and HA/backup hardening.
   §18.1's text updates to describe the two-role grant system as
   shipped groundwork rather than a future hook.

**Non-goals, stated to stay honest:** no multi-tenancy (one deployment,
one organization — teams wanting isolation run two installs); no
per-resource ACLs (the workspace is team-visible by construction; qm's
scope/grant machinery solves a private-workspace problem Conveyor does
not have); no self-registration or email-verification flows beyond the
one-time sign-in link; no built-in TLS termination; no change to the
§17.4 tool surface, the §6.4 worker contract, or the §13.1 two-gate
model.

**Migration.** Existing deployments: the user store starts empty, so the
config seed creates the admin on first post-upgrade boot; the shared
`CONVEYOR_API_TOKEN` maps to that admin's PAT (change 3); existing repos
migrate to `delivery: github`; external workers keep their enrollments;
`events` history is untouched. A solo-mode default is never inferred —
mode is an explicit `conveyor init` / config choice, and absent one the
deployment behaves as server mode with the seeded admin.

---

## Appendix A — prior art: qm's identity model (non-normative)

Surveyed August 1, 2026 ([yc-software/qm](https://github.com/yc-software/qm)),
a multiplayer agent harness — one server install, multiple humans,
agents acting on their behalf. Findings that shaped this draft:

- **One role, held as grant rows.** qm's only role is `org_admin`,
  stored as `(principal, scope, role, grantedBy, createdAt)` grants
  with env-seeded bootstrap applied solely to an empty store; everyone
  else is a member by default. Adopted (change 2).
- **Admin mutations are portal-only.** The API hard-rejects
  agent-originated calls to admin routes with reasons like "admin grant
  changes are portal-only — the agent cannot manage who governs the
  org." Adopted as the credential-class boundary (change 4).
- **Per-route auth declarations.** Every route names its auth mode
  (`source` / `public` / `either` / audience) in the route table;
  enforcement lives in one server chokepoint. Adopted (change 4).
- **Passwordless one-time links with a Postgres replay store.** The
  built-in auth broker emails single-use sign-in links whose claims go
  through a durable dedupe — the endpoint refuses to operate without
  `DATABASE_URL`. Adopted as the server-mode email option (change 3).
- **Stateless HS256 capability tokens with audiences and key
  rotation.** Declined: qm needs stateless tokens because it is
  multi-service (portal, egress proxy, blob transfer, credential
  broker). Conveyor is one binary plus Postgres; opaque hashed tokens
  are simpler and instantly revocable (change 3).
- **Scoped per-resource ACL grants** (personal/channel/group/org
  scopes, no transitive re-share). Declined: solves private per-person
  workspaces, which Conveyor deliberately does not have (non-goals).
