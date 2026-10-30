# dispatch: run some of the Hive's turns on another machine

The Hive starts every agent turn as a sandbox spec on its own machine. When
that machine is strained (low memory, CPU or memory pressure) the core holds
turns until it isn't. This module lets it hand those turns to another machine
you own instead, such as iiks1 (6 cores, 30 GB, load near 0), where they run in
the same sandbox the Hive uses, and their work comes back as if they had run
here.

It is an optional module, not one of the defaults.

## How it works

```
 main machine (the Hive)                              iiks1 (the runner)
 ───────────────────────                              ──────────────────
 core: turn is held for strain
   └─ asks `dispatch room`  ──── ssh: status ───────▶ hive-dispatch runner serve
   └─ starts `dispatch turn --spec F`                   (forced command, one session)
        sends: prompt, /work, /hive/run, /hive/box,
        /hive/home (whole), kb, modules, tools  ─────▶  unpacks into a folder of its own,
        (cached there, only changes sent)               runs `hive sandbox-exec` on it
        ◀── output, stderr, mailbox requests ──────     (bubblewrap / hive's namespaces,
        ── mailbox replies, stop ────────────────▶      egress through its own proxy)
        ◀── exit code, changed files of the writable    wipes the folder
            mounts (checkout, box, home)
   core sees the turn end as any other
```

- **The core's seam.** The core hands a turn to this module exactly as it
  hands one to its own sandbox: a spec in a file, run as
  `<module>/run turn --spec <file>` (as it runs `hive sandbox-exec --spec`).
  The turn's process lives on this machine for as long as the remote turn
  runs, so the core watches it, stops it and reads its output unchanged.
  The core does this for a turn it would otherwise hold for strain, after
  asking `run room` (it waits at most 5 seconds). The hook has been in the
  core since ihsan-sa/hive#601: see [core-seam.md](core-seam.md).
- **Where each turn ran** is in the module's `turns.jsonl` and reported to the
  Hive's journal (`module.reported`, kind `turn`), with the runners' room
  (kind `board`) every time it changes. `dispatch list` shows both.
- **Falls back to here.** A turn runs here, in the Hive's own sandbox, when no
  runner has room, when it was granted a service on this machine (a local
  Ollama), or when the runner refuses or can't be reached before the turn
  starts. A link that drops mid-turn ends the turn there and here (the core
  treats it like a crashed turn). The core has handed the turn over by then
  and can't take it back, so the module runs it here itself, as the core
  would have: the hive binary the core named among the turn's tools (an
  absolute path to `hive`), `hive sandbox-exec --spec F`, with F (it carries
  the turn's token) written under the module's HOME in `tmp/` (0700) as 0600
  and removed when the turn ends. module.json declares this. That binary is
  trusted because the core wrote the spec naming it (its own hive binary);
  nothing the module or a runner sends chooses it.
- **A turn goes only where its harness is.** The runner looks for each of a
  turn's tools (the hive binary, the path `"tools"` in runner.json names, then
  PATH, `~/.local/bin` and `/usr/local/bin`) before it takes the turn, and
  refuses one whose harness (`claude`, `codex`) isn't there, so the turn runs
  here. A runner from before that check starts it and its sandbox ends it
  `tool "claude" not found` (exit 125) before anything ran; the module reads
  that as the same refusal. Either way it remembers for an hour what the runner
  lacks and sends no turn needing it there (the board says so). To give
  iiks1 turns, install the harness there (and log it in) or name it under
  `"tools"`. With claude installed for `hive-runner`, real worker turns run
  there (the first: task 261028-141143, 2026-10-29, which ran `go test ./...`
  and the score on iiks1). Its `hive agent` ops go through the relayed mailbox
  and wait on the core here like a local turn's: when this machine is loaded
  an op can time out on the runner and its reply still arrive minutes later.
- **Worker turns only, no checks.** A worker turn needs only its harness on
  the runner; a project's test check needs that project's whole toolchain
  there, which is why checks sent to iiks1 kept failing or being refused.
  So module.json doesn't serve the core's `checks` hook and checks stay in
  the core's check line here. A check handed over anyway (a spec whose ID
  starts with `gate-`) is not run, here or there: `turn` writes why to its
  `.err`, records it as on `nowhere`, and exits 125 with no
  `hive-check-exit` marker, so the core reads "can't run" and runs it here.

### What crosses, and what doesn't

- **Credentials stay here.** GitHub stays with the effects process on the main
  machine: pushes, pull requests and merges never involve the runner. The
  runner gets what a local sandboxed turn gets: the spec's environment,
  which carries the short-lived model token for that turn, and the turn's own
  folders. The spec file is deleted once read on both sides, and the turn's
  folder on the runner (home, prompt, spec) is wiped when the turn ends, or
  at the next session if the runner was cut off.
- **Other projects go as their tracked files.** `/hive/projects` is cached on
  the runner like the knowledge base, modules and tools, and sent as changes,
  but only each project's git-tracked files (`git ls-files` per project
  folder), not its `.git` or what it built. On 10-29 the projects held about
  280k files in several GB and their tracked files 6.6k entries in 199 MB:
  the first turn sends that, later ones what changed, and the scan takes
  under a second. So a hive-fw, hive-mech or hive-hwde turn proving itself on
  hifi-receiver-2's files finds them there (task 261029-215215: two turns
  ended blocked on an empty folder). `git -C /hive/projects/x log` doesn't
  work there; a folder git can't list (not a repo, a symlink, dubious
  ownership) sends nothing and says so on the turn's dispatch output.
- **The Hive's records stay here.** `/hive/state` is an empty folder on the
  runner: its cache alone was 6.8 GB and its journal near a GB, live. A run
  that mounts `/hive/state` and isn't a worker's (`w-`) runs here instead:
  sweeps, step-backs, judges, filers, and reviews (`r-`, `s-`), which check
  goals.md quotes against `$HIVE_STATE/chats`. A worker whose brief sends it
  to the journal finds it empty there. A worker's brief and `$HIVE_RUN` carry
  what it needs, and the knowledge base, which is sent, holds the record's
  story. Anything a Hide mount covers (logins, module homes, private) is
  never read for sending.
- **The link is authenticated both ways and opens from the Hive only.** The
  Hive dials the runner over SSH with a key of its own (kept in the module's
  HOME, which no agent can read), and only to the host key you pinned.
  On the runner, that key is held by `authorized_keys` to one command
  (`hive-dispatch runner serve`) with `restrict`: no shell, no forwarding, no
  terminal. The runner listens on nothing of its own; it needs only the SSH
  server it already has. Nothing on the main machine is opened.
- **The sandbox there is the Hive's own.** The runner runs `hive sandbox-exec`
  from a hive binary installed on it, so a turn there can write only its own
  folders, reaches the network only through the egress proxy (with the
  turn's allow list), and can't reach iiks1's local services or network.

## Setting up a second machine

You need: SSH from the main machine to the other one, and on the other
machine a separate non-admin Linux user to run turns as (below:
`hive-runner`), Go 1.23+ to build (or copy a built binary), and the hive
binary.

**1. On the other machine (iiks1)**, as `hive-runner`, install the hive binary
with the same release as the main machine (v0.1.136 or later: the installer,
or copy `~/.local/bin/hive` from the main machine), and check its sandbox
works:

```sh
hive sandbox-probe           # should print: ok <sandbox>
# on Ubuntu 23.10 or later, if it fails: sudo apt install bubblewrap
```

Build the runner there and put it on PATH:

```sh
git clone https://github.com/Hive-RSI-Lab/hive-dispatch && cd hive-dispatch
go build -o ~/.local/bin/hive-dispatch ./cmd/dispatch
```

Make sure the SSH server runs (`sudo apt install openssh-server`) and the main
machine can reach iiks1 (over Tailscale, or the same network).

**2. On the main machine**, print the Hive's public key (the module makes it
the first time):

```sh
hive dispatch key
```

If the module isn't installed yet, `hive module add dispatch` first (or ask
the lead). If `hive dispatch` isn't available yet, use
`DISPATCH_HOME=~/hive/.hive/modules/dispatch ~/hive/.modules/dispatch/run key`.
If it says the module can't run, its review hasn't approved its current
commit yet.

**3. On the other machine**, as `hive-runner`, register the Hive's key, held to the runner:

```sh
hive-dispatch runner setup --name iiks1 --hive "$(command -v hive)" --hive-key "ssh-ed25519 AAAA... hive-dispatch" --install
```

It saves `~/.config/hive-dispatch/runner.json` (name, where turns live,
default `~/.local/share/hive-dispatch`, and the room it keeps: `max_load`
per core, default 0.75, and `min_free_mb`, default 4096), adds one line to `~/.ssh/authorized_keys`,
and prints the command for the last step with this machine's host key.

**4. On the main machine**, run what setup printed:

```sh
hive dispatch add iiks1 ssh://hive-runner@iiks1 --host-key "ssh-ed25519 AAAA..."
hive dispatch list           # iiks1: 0 turns, load 0.0 on 6 cores, ...
```

What this grants: the Hive's key can run only the runner command on iiks1
(`command="... runner serve",restrict` in `authorized_keys`). It gets no
shell, no terminal and no port forwarding. Turns there run in the Hive's own
sandbox and can't reach iiks1's local services.

`add` reads the runner's room at once. A runner has room while its load per
core is under 0.75 (`--max-load`) and it has 4 GB free (`--min-free-mb`); there
is no fixed number of turns. The runner checks again as each turn arrives, by
its own `max_load` and `min_free_mb`, and counts a turn that started in the
last two minutes as one core and 1 GB until the load average catches up. The
Hive counts the same way before it hands a turn over: each turn it has handed
a runner that the runner's last reading doesn't show yet is one more turn
running and one more core and 1 GB taken, so a burst stops at its room. A
person who wants a fixed cap besides sets `max_turns` in runner.json (or
`runner serve --max N`); the `max` key older setups wrote is ignored. `hive dispatch off iiks1` stops offering it turns without
removing it; `hive dispatch remove iiks1` forgets it and its host key.

To take the Hive's key off iiks1, delete its line from
`~/.ssh/authorized_keys` there.

To update the runner after a change to the module (a fix on the runner's
side of the link only takes effect once iiks1 runs it), as `hive-runner` there:

```sh
cd hive-dispatch && git pull && go build -o ~/.local/bin/hive-dispatch ./cmd/dispatch
```

## Commands

On the main machine (the module, run as `hive dispatch ...`):

| command | does |
| --- | --- |
| `dispatch` | the module's program: reads each runner's room every 20s, reports the board and every placed turn |
| `dispatch room` | prints `{"runner": "<name>"}` for the runner with most room, or exits 1 saying why none has |
| `dispatch turn --spec F [--runner N]` | runs one turn's spec on a runner (the core runs this) |
| `dispatch key` | the Hive's public key, made the first time |
| `dispatch add NAME ADDRESS --host-key K` | registers a runner (`ssh://user@host[:port][/path/to/hive-dispatch]`)|
| `dispatch list`, `dispatch status` | runners, their room, the latest turns (`status` reads every runner first) |
| `dispatch score` | the project's score (`.hive/project.json`): turns a runner ran that ended with exit 0 in the last 24 hours, by when they ended; 0 with no records |
| `dispatch on|off|remove NAME` | |

On the other machine: `hive-dispatch runner setup`, `hive-dispatch runner
serve` (what the forced command runs) and `hive-dispatch runner status`.

## Tests

`go test ./...` runs a turn end to end through a fake runner on the same
machine: the real runner, reached through a local command instead of SSH (a link
only the tests can set up: a runner in `runners.json` is always `ssh://`),
with a stand-in for `hive sandbox-exec` that plays an agent's turn (edits
and deletes files in the checkout, writes notes and a session, asks the core
through its mailbox and waits for the answer, writes stderr and an egress
refusal, exits 3). It checks all of that comes back, that the stop file stops
it there, that nothing of the Hive's records, its hidden folders or the
turn's token stays on the runner, and that each fallback runs the turn here.

`DISPATCH_LIVE_HIVE=$(command -v hive) go test ./internal/hive -run Live`
runs one turn through the real hive sandbox, on a machine where it works.
