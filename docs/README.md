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
  That seam is not in the core yet: see [core-seam.md](core-seam.md).
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
  and removed when the turn ends. module.json declares this.

### What crosses, and what doesn't

- **Credentials stay here.** GitHub stays with the effects process on the main
  machine: pushes, pull requests and merges never involve the runner. The
  runner gets what a local sandboxed turn gets: the spec's environment,
  which carries the short-lived model token for that turn, and the turn's own
  folders. The spec file is deleted once read on both sides, and the turn's
  folder on the runner (home, prompt, spec) is wiped when the turn ends, or
  at the next session if the runner was cut off.
- **Not sent:** other projects (`/hive/projects`) and the Hive's records
  (`/hive/state`) are empty folders on the runner, and anything a Hide mount
  covers (logins, module homes, private) is never read for sending. A turn
  that needs to read other projects or the journal sees nothing there; that
  is the price of not copying the Hive's records to another machine.
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
machine a Linux user to run turns as (your own is fine), Go 1.23+ to build
(or copy a built binary), and the hive binary.

**1. On the other machine (iiks1)**, install the hive binary the same way as
on the main one (the installer, or copy `~/.local/bin/hive`), with the same
release, and check its sandbox works:

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
machine can reach it (same network, or Tailscale).

**2. On the main machine**, install the module and print the Hive's key:

```sh
hive module add dispatch     # or ask the lead
hive dispatch key            # until the core's command seam lands:
                             #   DISPATCH_HOME=~/hive/.hive/modules/dispatch ~/hive/.modules/dispatch/run key
```

**3. On the other machine**, register the Hive's key, held to the runner:

```sh
hive-dispatch runner setup --name iiks1 --hive "$(command -v hive)" --hive-key "ssh-ed25519 AAAA... hive-dispatch" --install
```

It saves `~/.config/hive-dispatch/runner.json` (name, how many turns at once,
default half the cores, where turns live, default
`~/.local/share/hive-dispatch`), adds one line to `~/.ssh/authorized_keys`,
and prints the command for the last step with this machine's host key.

**4. On the main machine**, run what setup printed:

```sh
hive dispatch add iiks1 ssh://ihsan@iiks1 --host-key "ssh-ed25519 AAAA..."
hive dispatch list           # iiks1: 0/3 turns, load 0.0 on 6 cores, ...
```

`add` reads the runner's room at once. A runner has room while it has a free
turn slot, its load per core is under 0.75 (`--max-load`) and it has 4 GB free
(`--min-free-mb`). `hive dispatch off iiks1` stops offering it turns without
removing it; `hive dispatch remove iiks1` forgets it and its host key.

To take the Hive's key off iiks1, delete its line from
`~/.ssh/authorized_keys` there.

## Commands

On the main machine (the module, run as `hive dispatch ...` once the core
passes module commands through):

| command | does |
| --- | --- |
| `dispatch` | the module's program: reads each runner's room every 20s, reports the board and every placed turn |
| `dispatch room` | prints `{"runner": "<name>"}` for the runner with most room, or exits 1 saying why none has |
| `dispatch turn --spec F [--runner N]` | runs one turn's spec on a runner (the core runs this) |
| `dispatch key` | the Hive's public key, made the first time |
| `dispatch add NAME ADDRESS --host-key K` | registers a runner (`ssh://user@host[:port][/path/to/hive-dispatch]`)|
| `dispatch list`, `dispatch status` | runners, their room, the latest turns (`status` reads every runner first) |
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
