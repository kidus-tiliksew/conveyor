# Local evidence for task 261107-3d9daa

Task branch head: `d51e74982a93655231359ffcc2e1c5b0eb23d665` (base `ed37722dc8a72e9969cc77404cad3fecbefa4b06`).

- `fails-before-fix.log`: the final store conformance test
  (`store-test.patch`) run on the volatile store with the production sources
  at the base commit. Every binding and unknown-run case fails with the generic
  `verification access refused`; the first case,
  `BindingMismatch/workspace`, fails at verification_edges.go:363.
- `passes-after-fix.log`: the same store test and the httpapi test at the
  task head, both passing.
- `store-test.patch`: the test diff applied for both runs.

Run `sh verify.sh` to check SHA256SUMS and the recorded outcomes.
