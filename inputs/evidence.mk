# Local-only exit gates for Conveyor task 261030-8cfc34, run from the task
# worktree through scripts/validation_evidence.py. Each recipe keeps the
# Playwright exit status and always prints the JSON report summary.
SHELL := /bin/sh
INPUTS := /home/user/.local/state/conveyor-inputs/261030-8cfc34
FOUR := primary navigation places Decisions directly after System Design|the context checklist lists each proposed document checked, and Apply attaches them all|a returning user signs in with a password|500 open and 2,000 finished tasks
SETTINGS := a later identity read never overwrites|never editable before it shows

.PHONY: repeat20 negative-decisions negative-inbox

repeat20:
	cd web && status=0; PLAYWRIGHT_JSON_OUTPUT_NAME="$$TMPDIR/repeat20.json" npx playwright test tests/decisions.spec.ts tests/inbox.spec.ts tests/sign-in-and-session.spec.ts tests/board-map.spec.ts -g '$(FOUR)|$(SETTINGS)' --repeat-each=20 --reporter=list,json || status=$$?; python3 -I $(INPUTS)/summarize.py "$$TMPDIR/repeat20.json"; exit $$status

negative-decisions:
	git --no-pager diff -- web/src
	cd web && status=0; PLAYWRIGHT_JSON_OUTPUT_NAME="$$TMPDIR/negative-decisions.json" npx playwright test tests/decisions.spec.ts -g 'primary navigation places Decisions directly after System Design' --reporter=list,json || status=$$?; python3 -I $(INPUTS)/summarize.py "$$TMPDIR/negative-decisions.json"; exit $$status

negative-inbox:
	git --no-pager diff -- web/src
	cd web && status=0; PLAYWRIGHT_JSON_OUTPUT_NAME="$$TMPDIR/negative-inbox.json" npx playwright test tests/inbox.spec.ts -g 'the context checklist lists each proposed document checked, and Apply attaches them all' --reporter=list,json || status=$$?; python3 -I $(INPUTS)/summarize.py "$$TMPDIR/negative-inbox.json"; exit $$status
