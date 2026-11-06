# Delayed-mock reproduction (supporting evidence, not the exit gate)

Throwaway untracked copies of each spec, deleted after the run and never committed.
Each copy adds one 1500 ms wait before its mock acts, opening the race on purpose:

- inbox copies: in `serve`, before the `/v1/tasks/{id}/next-action` fulfil.
- github-app copies: before `state.secretPosts++` in the webhook-secret route.

`prefix` copies come from `41fbb0ea` (the base) and `postfix` copies from `ba561ade162d8a8b62769b1362fe7d6721270cbd`.
Each ran with `--repeat-each=3 --retries=0` through `make test-playwright-shard`.

| Log | Result |
| --- | --- |
| inbox-prefix.log | 3 failed: getByLabel('Direction'), element(s) not found |
| inbox-postfix.log | 3 passed |
| github-prefix.log | 3 failed: expected value must be a string or regular expression, received undefined |
| github-postfix.log | 3 passed |
