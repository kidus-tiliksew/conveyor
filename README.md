# Local evidence for Conveyor task 261106-2b47db

The repeat runs and `make fmt-check` ran at task head `ba561ade162d8a8b62769b1362fe7d6721270cbd`, with a clean tree. Each of those logs records the head and tree status in its `#` header lines. The delayed-mock logs ran from the same checkout against throwaway spec copies; their README says where each copy came from.

| File | What it is | Result |
| --- | --- | --- |
| inbox-keys-repeat-10.log / .json | Full output of the corrected inbox test, `--repeat-each=10 --retries=0`, one worker | 10 passed, exit 0 |
| github-app-secret-repeat-10.log / .json | Full output of the corrected webhook secret test, same flags | 10 passed, exit 0 |
| fmt-check.log | `make fmt-check` | exit 0 |
| delayed-mock/ | Before-and-after reproduction of both races with delayed mocks | see delayed-mock/README.md |

Both shortcut presses corrected under item 1 are in the one inbox test named by inbox-keys-repeat-10.

## Fetch and check

```sh
git fetch origin evidence/261106-2b47db
mkdir evidence-261106-2b47db
git archive FETCH_HEAD | tar -x -C evidence-261106-2b47db
cd evidence-261106-2b47db && sha256sum -c SHA256SUMS
grep -h '^# head\|^# exit' *.log
```
