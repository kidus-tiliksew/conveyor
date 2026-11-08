# Evidence for task 261107-885029

Raw local-only output retained for review, recorded at task head
d9a77a9692 (branch conveyor/task-261107-885029).

- `item5-tests-pass.log`: the item 5 tests on the unmodified tree (helper
  agreement test, store conformance AssertionTiming on memory, cmd/conveyor
  runner and explanation tests).
- `mutation-remove-<kind>.log`: the helper's `case verificationGap<kind>` is
  removed, so that gap becomes Unnamed; the agreement test fails.
- `mutation-drop-<kind>.log`: the case body is emptied, so the helper drops
  that gap silently; the agreement test fails.

Check with `./verify.sh`.
