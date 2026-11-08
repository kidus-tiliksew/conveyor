# Evidence for Conveyor task 261107-e47216

Raw `go test` output for the task's local-only Exit results. Each log's first
lines name the head commit, Go version, host, date and exact command.

| Log | Head | Handler |
| --- | --- | --- |
| `unfixed-count50-race.log` | 02d877bb (reproduction commit) | Unfixed: the first three cases must fail |
| `fixed-count50-race.log` | 6b84df58 (fix commit) | Fixed: all five cases must pass |
| `fixed-count50-race-loaded.log` | 6b84df58 (fix commit) | Fixed, with 32 busy `yes` loops on 4 CPUs |

Command in every log:
`go test ./cmd/conveyor -run 'TestKitRelayCancel' -count=50 -race -v`

Run `./verify.sh` to check `SHA256SUMS` and print each test's pass and fail
counts per log.
