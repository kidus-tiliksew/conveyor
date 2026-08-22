# Concepts

Three ideas explain most of Conveyor's design: the software factory, the
knowledge graph, and the light-to-dark spectrum of factory operation. If you
understand these, the rest of the docs read as implementation detail.

## The software factory

Generating code is easier than it has ever been. Checking that the code
matches product intent is now the bottleneck, and it is a bottleneck that
gets worse as agents get faster: a queue of unsupervised agents can ship
more unread code per day than a team can read.

A factory is the answer to this shape of problem. You do
not inspect every screw; you fix the process so that inspection happens at
the points where mistakes can happen, and you make every unit traceable
so that when something is wrong you know what else is affected.

Conveyor applies that answer to agent-written software:

- Intent is written down before work starts. Requirements, System Design
  documents, and decisions are drafted by anyone, agents included, but only
  an operator confirms them, and only confirmed documents are authority.
  This is [the document corpus](document-corpus.md).
- Work flows through stations. A [task](tasks.md) passes triage, an optional
  plan gate, implementation, and independent review, each an explicit stage
  with its own contract. The server rejects self-review and unstructured
  verdicts the way a factory rejects an uncalibrated gauge.
- Inspection is placed where mistakes change the product or its
  architecture: document confirmation, plan approval, merge approval, and
  the review contract that forces every delivery to be judged against
  pinned authority.
- Deviation is detected, not prevented. Merges that touch governed code
  without engaging the design, deliveries that outlive the intent they
  served, changes made outside the pipeline entirely: each raises a
  [signal](misalignment.md) for human judgment rather than a hard stop.

Conveyor coordinates work, it does not execute it. Agents run on your machines with
your credentials, edit in ordinary Git worktrees, and deliver through
ordinary pull requests. There is no hosted sandbox and the server never
holds your model keys. 

If you deleted Conveyor tomorrow, the repository,
the PRs, and the review trail would all still be sitting in Git and GitHub,
readable without it.

## The knowledge graph

A factory that cannot trace its parts is just a fast way to make defects.
Conveyor's answer is that every meaningful act appends an event, and lineage
edges are projected from those events. 

The graph is a projection: you can delete it and rebuild it from the ledger.

The result is one connected graph from intent to delivery. A requirement
serves tasks; design versions govern tasks and repository paths; tasks
depend on tasks, dispatch work orders, and are submitted as pull requests;
work orders produce verdicts supported by evidence; proposals point back at
the task that authored them. Ask it in either direction:
"what delivered this requirement, who reviewed it, against which design
version?" or "if I change this document, which merged work was built on the
old version?"

The graph is a working memory. 

When an agent claims a work order, a traversal of the graph supplies its context:
the served requirements, the governing designs, the neighboring tasks and
artifacts, each item tagged with the edge path that justified serving it.

Conveyor has no separate memory store. Durable knowledge belongs in
the corpus, where it is versioned, confirmed, and citable, not in a pile of
recollections nobody governs.

## Light and dark factories

Manufacturing distinguishes a lit factory floor full of people from a
"lights-out" factory that runs with nobody inside. Agent-driven software
has the same spectrum, and most teams genuinely need both ends of it in the
same week: hands-on for the subtle refactor of the payments code, hands-off
for the long tail of chores.

Conveyor treats the light level as configuration.

Running light, a person is in the loop at every step. Plan approval and
merge approval are on. Tasks are created with `hold`, so no worker touches
them; the operator runs `conveyor run <task-id>` and confirms each stage
before it claims, watches the output live, and answers gates inline. Every
stage of every task passes under human eyes.

Running dark, the queue drives itself. Gates are off (per task or workspace
default), durable [workers](worker-operations.md) poll and execute around
the clock, review rounds are staffed by configured agent seats, and approved
work merges without a click. The [monitor](misalignment.md#the-monitor)
watches the repository and post-merge CI, and files reconciliation tasks for
what it finds. The operator's job collapses to the attention queues:
pending proposals, drift, staleness.

Because the switches are per task, the practical answer is rarely one end.
The workspace default can be dark while the tasks that touch governed
architecture carry gates, or hold, or an assignee. Bounce limits are the
backstop for the dark end: review rounds that keep failing park the task at
a human gate after `max_bounces` rounds rather than burning tokens forever.

Document confirmation is operator-only at every light level; agents propose and keep working, and nothing they propose becomes authority on its own. Misalignment signals are
likewise never auto-resolved. 

The darkest configuration Conveyor supports is a factory that writes, reviews, and merges its own code all night, and still cannot change what it is supposed to be building without a human saying so in the morning.
