# Agent events

What an agent on a machine is doing, in words that don't belong to one vendor. A status
bar, a notifier or a dashboard reads this instead of what each agent happens to say, and
keeps working when those words change.

`meterm agents --format json` is where it comes from today:

```json
{"schema":1,"hosts":[{"host":"devbox","reachable":true,"error":"","agents":[
  {"kind":"interactive","pid":812,"status":"busy","sessionId":"…",
   "event":"working","agent_kind":"claude-code","source":"claude agents --json","confidence":"high"}]}]}
```

The fields below are added to each agent. Schema 1's fields stay as they were, and `schema`
stays 1: it goes up when a field changes meaning, and new fields don't need it to.

| Field | Values | Meaning |
|---|---|---|
| `event` | `working`, `needs_input`, `done`, `failed`, `idle`, `unknown` | What the agent is doing. |
| `agent_kind` | `claude-code` | Which agent. More can be added, so show one you don't know as text. |
| `source` | `claude agents --json` | What this reading came from. It says how far to trust it, and another source may come later. |
| `confidence` | `high`, `low` | `high`: a known value said so and nothing contradicted it. `low`: the values disagree, `event` is `unknown`, or it's `working` or `needs_input` with no `pid`. |

## What each event means

| `event` | Means | What to do with it |
|---|---|---|
| `working` | It's busy. | Leave it. |
| `needs_input` | It's waiting for a person. | Tell them. |
| `done` | It finished. | Report it. |
| `failed` | It ended badly. | Report it. |
| `idle` | It has nothing to do, and said so. | Nothing. |
| `unknown` | Nothing here can say. | Show it as unknown. |

`unknown` is never `idle`. In `meterm agents --watch`, a wait that was raised stays raised
while its session reads as `unknown`. That stream's last line, `stopped`, says how many lines
it couldn't write on the way out (`dropped`, a field added within schema 1): see
[agents-watch.md](agents-watch.md). An agent saying a word nobody has seen before could be stuck on
something. Treat it as "can't say", as you would a host that can't be reached.

## How Claude Code's words map

One function does this, `Classify` in `internal/agents/event.go`, with a table test beside it.

| Claude Code says | `event` |
|---|---|
| `waitingFor` set to anything | `needs_input` |
| `state` `working`, `running` | `working` |
| `state` `blocked` | `needs_input` |
| `state` `done` | `done` |
| `state` `failed`, `error` | `failed` |
| `state` `stopped` | `idle` |
| `status` `busy` | `working` |
| `status` `waiting` | `needs_input` |
| `status` `idle` | `idle` |
| any other value of `state` or `status`, or neither | `unknown` |

A row with no `pid` whose `event` is `working` or `needs_input` has `confidence: low`. That's
what a background session's row reads like for 6 to 10 seconds after its worker died, until the
supervisor starts another (seen on Claude Code 2.1.323): `state: blocked` and no process.
`meterm agents --watch` raises such a row only when two answers from the host in a row (a look that fails in between doesn't count) say so ([agents-watch.md](agents-watch.md)).

`state` goes before `status`, as a finished background agent says `state: done, status: idle`.
Matching is exact, so `IDLE` is `unknown`. `done`, `failed` or `stopped` with `status: busy`,
or a finished or stopped agent with `waitingFor` still set, keeps the `state` and has
`confidence: low`.

`stopped` is `idle`. In Claude Code (read from 2.1.323 to 2.1.325's own code; not yet seen in
real output) it's one of the three ends a background job can have, beside `done` and
`failed`: a session someone stopped, or a cloud one paused ("reply to resume") or out of
contact. `claude agents --json` says `stopped` only for a job that isn't running, so nothing
there is waiting on a person, and a wait that was raised for it ends. It isn't `done`, as the
work didn't finish; it can be started again by hand. Before this, meterm called it `unknown`.
The set of `event` values hasn't changed, so `schema` stays 1.

A consumer that names hosts on the command line should put `--` before them
(`meterm agents --watch --format json -- HOST…`): a host could be named like one of
`meterm agents`' own words, such as `key`, and without `--` it would be taken for that.

`claude agents --json` isn't a promised interface. `busy`, `idle`, `waiting`, `working`,
`blocked`, `done` and `failed`, and `waitingFor: "permission prompt"`, have been seen in real
output (2.1.323); the other values come from reading Claude Code 2.1.316 itself, and may change.
That's what `unknown` is for: a change shows up as `unknown`, not as something false.

## Reading it safely

The four fields come from meterm's own tables, never from a host, so they can be matched
exactly. Everything else in the report comes from a machine: strings have control and
invisible characters replaced and are cut at 1024 characters, but a `name` is still text a
stranger may have written. Show it as text, and don't run or follow it.
