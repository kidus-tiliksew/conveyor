# Conveyor plain-English profile

Citation scope: the REQ, AC, DEC, and component IDs in this guidance, and its
other requirement, design, and reference IDs, cite the Conveyor product's own
design corpus, not the corpus of the factory you work in. Task work cites the
working factory's own confirmed corpus, as the delivered contract directs.

This playbook defines "plain language" for Conveyor guidance (DEC-126(1)).
The profile is a small subset of ASD-STE100 Simplified Technical English.
It adds a glossary of Conveyor names that the profile accepts as written.
A reader with only this playbook can decide if a sentence meets each rule.

## Scope

The profile applies to new and changed text of these kinds (DEC-126(2)):

- Timeline text: progress messages, release and checkpoint reasons, verdict
  summaries, decision reasons, and verification summaries.
- Questions, gate offers, and status lines that a skill sends to an operator,
  in chat or through an out-of-band channel.
- The text of embedded skill wrappers and playbooks.
- The interface text of the Conveyor web dashboard.
- The messages that the `conveyor` and `conveyord` commands print.

The profile does not apply to these kinds of text (DEC-126(3)):

- Requirements, System Design documents, decisions, and reference documents.
- Task bodies.

Apply the profile to new and changed text only. Do not rewrite old text only
to meet the profile. A later change to old text brings that text under the
profile.

## Rules

Each rule is a test that a reader can apply to one sentence or one paragraph.

1. **Instruction length.** Write an instruction in the imperative. Use at most
   20 words in an instruction.
2. **Description length.** Use at most 25 words in a sentence that describes
   or explains.
3. **Paragraph size.** Give each paragraph one topic. Use at most six
   sentences in a paragraph.
4. **Active voice.** Use the active voice. Name the actor that does the
   action.
5. **Helping verbs.** Use only "can", "must", and "will" as helping verbs. Do
   not use "should", "would", "could", "may", or "might". You can use "do
   not" for a negative instruction.
6. **One instruction.** Write one instruction in each sentence.
7. **Punctuation.** Do not use semicolons. Do not use contractions such as
   "don't" or "it's".
8. **One name.** Use one name for one item. When the glossary has a name for
   the item, use that name.
9. **Noun clusters.** Write noun clusters of at most three words. This rule
   does not apply to identifiers.
10. **No "-ing" verbs.** Do not use the "-ing" form of a verb. A glossary
    name, such as the `running` task state, keeps its form.
11. **Simple words.** Use the simple word when one exists. The table below
    gives common pairs.

| Do not write | Write |
| --- | --- |
| utilize, leverage | use |
| commence | start |
| facilitate | help |
| prior to | before |
| subsequent to | after |
| in order to | to |
| terminate | stop |
| ascertain | find |
| sufficient | enough |
| approximately | about |
| additional | more |
| numerous | many |
| obtain | get |
| indicate | show |
| in the event that | if |
| due to the fact that | because |

## Exemptions

- **Fixed text.** The profile never changes code, identifiers, commands,
  paths, quoted text, or official names (DEC-126(3)). Keep them exactly as
  they are. A fixed statement that Conveyor ships byte for byte is quoted
  text. The citation-scope statement is an example.
- **Short labels.** A button, a column header, a state badge, or another
  label of one word or a few words follows only rule 8.
- **Advice only.** A profile finding is advice (DEC-126(4)). A reviewer can
  note a finding. No gate, CI job, or validator fails on a profile finding
  alone.

## The timeline rules stay

The first-sentence rules of req-readable-agent-text AC-1.1 to AC-1.3 apply
in full. The profile adds to these rules and does not replace them.

- The first sentence of a triage summary tells what the task asks for. It
  also tells if the task proceeds, or why triage parked it.
- The first sentence of other timeline text tells what happened and what
  happens next.
- The first sentence contains no work order or session identifiers. It also
  contains no file paths, migration numbers, commit SHAs, or REQ, AC, or DEC
  identifiers.
- Put the identifiers that other instructions require in later sentences.

## Glossary

The profile accepts these names as written. Each name has one entry
and one short description.

### Profile names

| Name | Description |
| --- | --- |
| finding | A place where text breaks a profile rule. A reviewer can note it as advice. |
| helping verb | A verb that goes before a main verb, such as "can" in "can start". |

### Factory names

| Name | Description |
| --- | --- |
| agent | The AI program that works in a session. |
| bounce | The return of a task to implement after a reviewer requests changes. |
| checkpoint | A stop where an agent releases its work order and asks an operator for a decision. |
| claim | The act by which a session takes a work order. A claim holds a lease. |
| corpus | The confirmed requirements, System Design documents, and decisions of a workspace. It is the design authority. |
| decision | A record with a DEC-n identifier. It states one confirmed choice and its reasons. |
| drift | A difference between the repository and the confirmed corpus, for example after a merge outside Conveyor. |
| evidence | A recorded result that shows what a check did and what it found. |
| factory | One Conveyor installation: its server, its workspaces, and the people and agents that do its work. |
| gate | A point where a task stops until an operator decides. |
| implement | The stage in which an agent changes the code and opens a pull request. |
| kit | A repository verification kit. It declares checks that the verify stage runs. |
| lease | The time limit of a claim. A session renews it, or the work order returns to the queue. |
| merge approval | The gate at which an operator decides if an approved task merges. Its field is `merge_approval`. |
| operator | A person who decides at gates and confirms documents. |
| plan | The stage that writes the execution plan of a task. The word also names that plan. |
| plan state | The field that tells if a task needs a plan and if its plan has approval. |
| reference document | An informative document, such as a product overview. It is never authority. |
| requirement | A document of REQ-n and AC-n.m items that states what a customer can see the system do. |
| review | The stage in which a reviewer agent reads the change and submits a verdict. |
| review seat | One reviewer place in a review round. Each seat has its own work order and session. |
| session | One agent conversation in a harness. It claims work orders under its own session ID. |
| stage | One step of a task: triage, plan, implement, verify, or review. |
| System Design document | A document that states the boundaries, surfaces, invariants, and failure behavior of one component or feature. |
| task | One change to one repository. It moves through stages until it merges or closes. |
| timeline text | The text that the task timeline shows, such as progress messages and verdict summaries. |
| triage | The stage that reads a new task. It lets the task proceed or parks it with a reason. |
| verdict | The result that a triage or review agent submits. |
| verify | The stage that runs kits and other checks and records their evidence. |
| work order | The unit of work that a session claims for one stage of one task. |
| workspace | One set of repositories, documents, tasks, and members in a factory. |

### State names

The dashboard shows these state names. The second column lists the labels
that the dashboard prints for each name.

| Name | Dashboard labels | Description |
| --- | --- | --- |
| `claiming` | Claiming | A new task before intake completes. |
| `queued` | Queued, Waiting for an agent, Ready | A task or work order that waits for a session to claim its next work order. |
| `running` | Running, Working | A task with a claimed work order, or a check that has started. |
| `awaiting_human` | Awaiting human, Needs you, Plan gate, Merge gate | A task that waits for an operator decision at a gate. |
| `approved` | Approved | A task that review approved. It waits for its merge. |
| `merged` | Merged, Finished | A task whose pull request merged. The task is done. |
| `closed` | Closed | A task that stopped without a merge. |
| `parked` | Parked | A task that stopped with a reason until an operator recovers it. |
| `claimed` | In progress | A work order that a session holds under a live lease. |
| `submitted` | Submitted | A work order whose session submitted its result. |
| `completed` | Completed, Planning completed | A work order that finished. As a plan state, the task has an approved plan. |
| `cancelled` | Cancelled | A work order that stopped and will not run. |
| `timed_out` | Timed out | A work order that passed its execution deadline. |
| `stale` | Stale, Went stale in the queue | A legacy state of a work order. The factory returns the work order to `queued`. |
| `not_required` | No plan required | A plan state. The task goes to implement without a plan. |
| `needed` | Planning needed | A plan state. The task needs a plan before implement. |
| `awaiting_approval` | Plan awaiting approval | A plan state. A plan waits for operator approval. |

### Status labels

The dashboard also shows these labels for checks, entries, and board groups.

| Label | Description |
| --- | --- |
| Blocked | A task that waits for a dependency to merge. |
| Failed | A job or check that ended with an error or a failed result. |
| Passed | A check that ended with a pass. |
| Pending | A check that has not started. |

## Examples

Each pair shows the usual shape of timeline text and the same text under the
profile. The "Before" text breaks the profile on purpose. The identifiers in
these pairs are samples.

**Progress message.**

> Before: Claimed 260101-abc123-implement-1, worktree is set up at base
> 1a2b3c4d; now implementing plan step 3 (AC-2.1), should be quick.
>
> After: The task worktree is ready, and next I will add the install tests.
> Work order 260101-abc123-implement-1 starts from base 1a2b3c4d. This step
> serves AC-2.1.

**Release reason.**

> Before: Releasing since done criterion 4 can't be met without amending
> component-example v12 clause 7, which would conflict with DEC-40; operator
> input needed.
>
> After: I stopped the work because the plan needs a change to a confirmed
> design, and an operator must decide. Done criterion 4 conflicts with
> component-example v12 clause 7. The operator can revise that design or the
> plan.

**Review verdict summary.**

> Before: LGTM overall, tests look solid, though the rollback path might be
> worth a second look; requesting changes on AC-1.3 only.
>
> After: The change installs the skill correctly, but one refusal case has no
> test, so the implementer must add it. AC-1.3 needs a test for an unmarked
> file at the destination. CI holds evidence for the other criteria.

**Operator gate question.**

> Before: Plan for 260101-abc123 is ready. I'd recommend approving since it's
> low risk. Reply approve or changes (default: approve).
>
> After: The plan for the install test task is ready, and it waits for your
> decision. Reply with one of these words: approve, changes, or wait. The plan
> belongs to task 260101-abc123.

The gate question gives no default and no recommendation.

## Check list

Read the text once for each item before you send it.

1. In timeline text, check that the first sentence tells what happened and
   what happens next.
2. In timeline text, move each identifier out of the first sentence.
3. Count the words in each instruction. Keep the count at 20 or fewer.
4. Count the words in each description. Keep the count at 25 or fewer.
5. Keep each paragraph to one topic and six sentences or fewer.
6. Name the actor of each action.
7. Replace "should", "would", "could", "may", and "might".
8. Remove semicolons and contractions.
9. Replace each "-ing" verb that is not a glossary name.
10. Use the glossary name for each Conveyor item.
11. Break each noun cluster of four or more words.
12. Replace each hard word with its simple word from the table.
13. Keep code, identifiers, commands, paths, quoted text, and official names
    exactly as they are.

## Source

This profile takes its rule types from ASD-STE100 Simplified Technical
English, Issue 7 (https://www.asd-ste100.org). This profile is not an
official ASD document. ASD owns ASD-STE100 and its dictionary. Conveyor ships
only its own profile text and glossary. Conveyor does not ship the ASD-STE100
dictionary word list or a copy of a third-party skill (DEC-126(5)).
