# The seam this module needs in the core (ihsan-sa/hive)

The module seam (#27) lets a module's program read the journal and call a
slice of the control API. It has no way for a module to run a turn's sandbox
spec somewhere else, nor to add a `hive <module> ...` command. These are the
smallest additions that give it both; they go to ihsan-sa/hive as a pull
request of their own, filed from this work.

## 1. An executor hook: a turn's spec handed to a module

A module whose manifest lists the hook `executor` can be handed turns. Its
program answers two commands, run from the module's folder with HOME set to
the module's home and the same scrubbed environment `runner.start` gives a
module (no forge or model credentials, no `HIVE_*`):

- `<exec> room`: exit 0 and print `{"runner": "<name>"}` when it can take a
  turn now; anything else means no. The core waits at most 5 seconds.
- `<exec> turn --spec <file>`: run the turn in the spec and exit with its exit
  code, writing its output to the spec's `stdout` (and `.err`, `.egress`) and
  stopping when the spec's `stop_file` appears, as `hive sandbox-exec` does.
  It deletes the file once read: it carries the turn's token.

In `core.Runs.launch`, where it now holds a turn because `Strained(...)` says
so (and, with a setting, when more runs are queued than it starts at once):

```go
if why := Strained(cfg.MemFloorMB, cfg.MemPressureMax, cfg.CPUPressureMax); why != "" {
	if ex, where := rs.executor(); ex != nil { // a running executor module that answered `room`
		sb, elsewhere = ex, where
	} else {
		... hold, as now ...
		return
	}
}
```

`ex` is a `sandbox.Sandbox` like WSL's: `Isolated()` is true (inside paths, so
the spec is built exactly as for the local sandbox), and `Start(spec)` writes
the spec with `writeTempSpec` and runs

```go
proc.StartDetached([]string{modDir + "/run", "turn", "--spec", specPath}, moduleEnv(home), modDir, "", turnOut+".dispatch")
```

`run.turn_started` gains `where: "<runner>"` (empty is this machine), so the
record and the dashboard say where each turn ran. Nothing else changes: the
core watches the delegate's process with `proc.Alive`, drains the mailbox,
ends the turn when it exits and reads its output as before; the module brings
the turn's files back into the checkout, box and home before it exits.

`app` wires `rs.executor` from the module registry and the module runner
(`Runner.Status(name) == "running"` for a module with the hook). Scopes gains
`"executor": {}`: the hook needs no API paths.

## 2. `hive <module> ...` for a module that asks for it

A manifest with `"command": true` gets `hive <name> <args...>`: the CLI runs
`<module dir>/<exec[0]> <args...>` attached to the terminal, with the
module's HOME (`<hive>/.hive/modules/<name>`) and the scrubbed environment.
That is `hive dispatch add iiks1 ssh://...`, `hive dispatch list`. Until it
lands, the person runs the module's `run` with `DISPATCH_HOME` set (see
README).

## 3. The dashboard

The Modules screen shows each module's latest `module.reported` of kind
`board` (this module's runners and their room), and a run's row shows
`where` from its latest `run.turn_started` when it isn't this machine.

## Order

Install waits on task 261024-174301-2f7050eb10 (module programs run in the
sandbox with only their declared reach), per the lead's decision. Note for
that work: this module's `turn` and its program need to reach the runner over
SSH (outbound TCP to the runner's address, which is on the local network:
the egress proxy refuses private addresses unless granted), write the turn's
checkout, box and home, and read the turn's mounts (kb, modules, tools). The
module declares that reach; it holds no credential but its own SSH key.

## Once module programs are sandboxed

Today `run turn --spec` falls back to the local sandbox itself (it starts the
spec's `hive sandbox-exec`) when no runner has room or the turn can't go. When
module programs run in a sandbox (task 261024-174301-2f7050eb10), that nested
start may not work. Then the fallback should move to the core: `run room`
already answers before the core hands a turn over, so the core can keep a
"no room" turn and start it locally as it does now, and `turn` would exit
without starting anything.
