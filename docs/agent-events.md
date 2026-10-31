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
| `confidence` | `high`, `low` | `high`: a known value said so and nothing contradicted it. `low`: the values disagree, or `event` is `unknown`. |

## What each event means

| `event` | Means | What to do with it |
|---|---|---|
| `working` | It's busy. | Leave it. |
| `needs_input` | It's waiting for a person. | Tell them. |
| `done` | It finished. | Report it. |
| `failed` | It ended badly. | Report it. |
| `idle` | It has nothing to do, and said so. | Nothing. |
| `unknown` | Nothing here can say. | Show it as unknown. |

`unknown` is never `idle`. An agent saying a word nobody has seen before could be stuck on
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
| `status` `busy` | `working` |
| `status` `waiting` | `needs_input` |
| `status` `idle` | `idle` |
| any other value of `state` or `status`, or neither | `unknown` |

`state` goes before `status`, as a finished background agent says `state: done, status: idle`.
Matching is exact, so `IDLE` is `unknown`. `stopped` is `unknown` on purpose: it says neither
that the work finished nor that it failed. `done` with `status: busy`, or a finished agent with
`waitingFor` still set, keeps the `state` and has `confidence: low`.

`claude agents --json` isn't a promised interface. Only `busy`, `idle` and `done` have been seen
in real output; the other values come from reading Claude Code 2.1.316 itself, and may change.
That's what `unknown` is for: a change shows up as `unknown`, not as something false.

## Reading it safely

The four fields come from meterm's own tables, never from a host, so they can be matched
exactly. Everything else in the report comes from a machine: strings have control and
invisible characters replaced and are cut at 1024 characters, but a `name` is still text a
stranger may have written. Show it as text, and don't run or follow it.
