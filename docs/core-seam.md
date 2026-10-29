# The core's seam for this module (ihsan-sa/hive)

The core hands turns to this module and gives it a `hive dispatch ...`
command. Both landed in ihsan-sa/hive with PR #601. In the core's source the
hook is `internal/mod/runner/executor.go`, with `internal/sandbox/delegate.go`
and `Runs.launch`/`Runs.executor` in `internal/core/runs.go`. The design is in
the core's `docs/architecture.md` ("Two seams where the core calls the
module"). `docs/core.md` says why the hook is core: only launch knows a turn's
spec and when the machine holds it. This page says what the module relies on.

## 1. The executor hook

The core treats a module as an executor when its manifest lists the hook
`executor` (this one's does) and the module runner has it `running`. Its
program answers two commands. Each runs from the module's folder with HOME set
to the module's home (`<hive>/.hive/modules/dispatch`) and the scrubbed
environment `runner.Env` gives (no forge or model credentials, no `HIVE_*`):

- `<exec> room`: exit 0 and print `{"runner": "<name>"}` when it can take a
  turn now. Anything else means no. The core waits at most 5 seconds
  (`RoomWait`). A "no" stands for 15 seconds and a "yes" for 5, so the work
  queue's question and the launch after it ask only once. A turn handed over
  spends the "yes". Each "yes" counts as a turn handed to that runner (for
  30 seconds, or until its turn comes and takes its place), and each turn
  counts until a reading of the runner after it started shows it, so a burst
  of asks judged on one cached reading stops at the runner's room instead of
  all hearing "yes" (10-29: 61 turns in seven minutes, all refused there and
  run here on a machine at 92% memory pressure).
- `<exec> turn --spec <file> --runner <name>`: runs the turn in the spec and
  exits with its exit code. It writes the turn's output to the spec's `stdout`
  (and `.err`, `.egress`) and stops when the spec's `stop_file` appears, as
  `hive sandbox-exec` does. It deletes the file once read, because the file
  carries the turn's token. `--runner` is the runner `room` named. The module
  still decides at `turn` and runs the turn here when that runner has filled
  since.

In `Runs.launch`, a turn the machine is too strained for (`Strained(...)`)
goes to `rs.executor()` before it is held. That only happens when the local
sandbox is an isolated one, so the spec is built with inside paths, exactly as
for the local sandbox. Leads get no pressure hold (only the memory floor), so
in practice it is workers' turns that come here. With an executor that has
room, the turn's sandbox is a `sandbox.Delegate`. It writes the spec with
`writeTempSpec` and starts the module's program detached, its output going to
the turn's stdout plus `.dispatch`. With no executor, or no room, the turn is
held as before.

The core watches the delegate's process, drains the mailbox, ends the turn
when the process exits and reads its output, as for any turn. Before it exits,
the module brings the turn's files back into the checkout, box and home.
`run.turn_started` carries `where: "<runner>"` (empty means this machine), and
the run's row shows "on <runner>". The work queue lets one task per pass go
past a strained machine while an executor has room. A machine over `load_max`
alone still holds tasks, since launch would run them here.

A project's test check goes to an executor only when its manifest also lists
the hook `checks` (`Runs.CheckExecutor`, ihsan-sa/hive#1885). This module's
doesn't: it carries worker turns only, and checks stay in the core's line
here. A check spec (ID `gate-...`) handed to `turn` anyway ends at once,
unrun, with no `hive-check-exit` marker, so the core reads "can't run" and
puts it back in its line.

## 2. `hive dispatch ...`

The module's manifest sets `"command": true`. That gives `hive dispatch
<args...>`: the CLI runs `./run <args...>` attached to the terminal, from the
module's folder, with its own HOME and the scrubbed environment. It does so
only while the module may run (its review approved the commit it is at). A
built-in command of the same name wins. That is how `hive dispatch key`,
`hive dispatch add iiks1 ssh://...` and `hive dispatch list` work. If the
module can't run, `hive dispatch` says why. The module's `run` with
`DISPATCH_HOME` set does the same by hand (see [README.md](README.md)).

## 3. The dashboard

The run's row shows where each turn ran, from `run.turn_started`. The
runners' board is in the journal as `module.reported` of kind `board`, and
`hive dispatch list` shows it. The core has no Modules-screen view of it yet.

## The fallback here

The core can't take a turn back once it has handed it over, so when the turn
can't go, `run turn --spec` runs it here itself. That covers no runner with
room by then, a turn granted a service on this machine, or a runner that
refused it before starting. It runs the hive binary the core named among the
turn's tools, `hive sandbox-exec --spec F`, as module.json declares. If
module programs come to run in a sandbox where that nested start doesn't
work, the fallback moves to the core. `room` already answers before the core
hands a turn over, so the core can keep a "no room" turn and start it locally,
and `turn` would exit without starting anything.
