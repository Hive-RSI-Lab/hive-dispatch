// Command dispatch runs some of a Hive's agent turns on another machine the
// person owns. The same binary is both sides:
//
// On the Hive's machine, as the dispatch module (HOME is the module's own):
//
//	dispatch                         the module's program: reads each runner's room, reports the board
//	dispatch room                    prints the runner with the most room, or exits 1 saying why none has
//	dispatch turn --spec F [--runner N]   runs a turn's sandbox spec on a runner (or here, when it can't go)
//	dispatch key                     prints the Hive's public key, for a runner's setup
//	dispatch add NAME ADDRESS [--host-key K] [--max-load L] [--min-free-mb M]
//	dispatch remove NAME | off NAME | on NAME
//	dispatch list                    the runners, their room and the latest turns
//	dispatch status                  reads every runner now
//	dispatch score                   turns a runner ran and ended ok in the last 24 hours
//
// On the other machine, installed as hive-dispatch:
//
//	hive-dispatch runner setup --hive-key K [--install] [--name N] [--max N] [--hive PATH] [--dir D]
//	hive-dispatch runner serve       one session on stdin and stdout (an SSH forced command)
//	hive-dispatch runner status      this machine's room
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Hive-RSI-Lab/hive-dispatch/internal/hive"
	"github.com/Hive-RSI-Lab/hive-dispatch/internal/runner"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 || args[0] == "module" {
		home, err := home()
		if err != nil {
			return fail(err)
		}
		return fail(home.Serve(os.Getenv("HIVE_API"), os.Getenv("HIVE_MODULE_TOKEN")))
	}
	if args[0] == "runner" {
		return runnerCmd(args[1:])
	}
	if args[0] == "score" {
		// The project's score: no module folder or no records reads 0.
		n := 0
		if h, err := home(); err == nil {
			n = h.Score(time.Now())
		}
		fmt.Println(n)
		return 0
	}
	h, err := home()
	if err != nil {
		return fail(err)
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "room":
		r, why, err := h.Pick()
		if err != nil {
			return fail(err)
		}
		if r.Name == "" {
			fmt.Fprintln(os.Stderr, "no runner has room:", why)
			return 1
		}
		json.NewEncoder(os.Stdout).Encode(map[string]string{"runner": r.Name})
		return 0
	case "turn":
		fs := flag.NewFlagSet("turn", flag.ContinueOnError)
		specPath := fs.String("spec", "", "the turn's sandbox spec (deleted once read)")
		name := fs.String("runner", "", "the runner to use (default: the one with most room)")
		if fs.Parse(rest) != nil || *specPath == "" {
			return 2
		}
		return h.Delegate(*specPath, *name)
	case "key":
		pub, err := h.PublicKey()
		if err != nil {
			return fail(err)
		}
		fmt.Println(pub)
		return 0
	case "add":
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "usage: dispatch add NAME ssh://user@host[:port] --host-key \"ssh-ed25519 AAAA...\"")
			return 2
		}
		fs := flag.NewFlagSet("add", flag.ContinueOnError)
		key := fs.String("host-key", "", "the runner's SSH host key, as `hive-dispatch runner setup` printed it")
		load := fs.Float64("max-load", 0, "offer it nothing while its load per core is over this")
		free := fs.Int("min-free-mb", 0, "offer it nothing while it has less memory free")
		if fs.Parse(rest[2:]) != nil {
			return 2
		}
		r := hive.Runner{Name: rest[0], Address: rest[1], MaxLoad: *load, MinFreeMB: *free}
		if err := h.Add(r, *key); err != nil {
			return fail(err)
		}
		s := h.Probe(r)
		fmt.Printf("added %s\n", r.Name)
		if w := r.Room(s); w != "" {
			fmt.Printf("it has no room yet: %s\n", w)
		} else {
			fmt.Printf("it has room: %d turns running, load %.1f on %d cores, %d MB free\n", s.Running, s.Load1, s.Cores, s.MemFreeMB)
		}
		return 0
	case "remove":
		if len(rest) != 1 {
			return 2
		}
		if err := h.Remove(rest[0]); err != nil {
			return fail(err)
		}
		return 0
	case "on", "off":
		if len(rest) != 1 {
			return 2
		}
		rs, err := h.Runners()
		if err != nil {
			return fail(err)
		}
		found := false
		for i := range rs {
			if rs[i].Name == rest[0] {
				rs[i].Off, found = cmd == "off", true
			}
		}
		if !found {
			return fail(fmt.Errorf("no runner %q", rest[0]))
		}
		if err := h.Save(rs); err != nil {
			return fail(err)
		}
		return 0
	case "status":
		rs, err := h.Runners()
		if err != nil {
			return fail(err)
		}
		for _, r := range rs {
			h.Probe(r)
		}
		fallthrough
	case "list":
		lines := h.Board(20)
		if len(lines) == 0 {
			fmt.Println("no runners: dispatch add NAME ssh://user@host --host-key ...")
		}
		for _, l := range lines {
			fmt.Println(l)
		}
		return 0
	}
	fmt.Fprintf(os.Stderr, "dispatch: unknown command %q\n", cmd)
	return 2
}

func runnerCmd(args []string) int {
	if len(args) == 0 {
		return 2
	}
	fs := flag.NewFlagSet("runner", flag.ContinueOnError)
	cfgPath := fs.String("config", runner.ConfigPath(), "the runner's settings")
	name := fs.String("name", "", "this machine's name in the Hive")
	dir := fs.String("dir", "", "where turns and the cache live")
	hiveBin := fs.String("hive", "", "the hive binary that runs the sandbox")
	maxTurns := fs.Int("max", 0, "a fixed cap on turns at once besides load and memory (default none)")
	maxLoad := fs.Float64("max-load", 0, "take no turn while load per core is over this (default 0.75)")
	minFree := fs.Int("min-free-mb", 0, "take no turn while less memory is free (default 4096)")
	hiveKey := fs.String("hive-key", "", "setup: the Hive's public key (`dispatch key` on the Hive's machine)")
	install := fs.Bool("install", false, "setup: add the Hive's key to ~/.ssh/authorized_keys")
	if fs.Parse(args[1:]) != nil {
		return 2
	}
	cfg, err := runner.Load(*cfgPath)
	if err != nil {
		return fail(err)
	}
	for _, f := range []struct {
		to   *string
		from string
	}{{&cfg.Name, *name}, {&cfg.Dir, *dir}, {&cfg.Hive, *hiveBin}} {
		if f.from != "" {
			*f.to = f.from
		}
	}
	if *maxTurns > 0 {
		cfg.Max = *maxTurns
	}
	if *maxLoad > 0 {
		cfg.MaxLoad = *maxLoad
	}
	if *minFree > 0 {
		cfg.MinFreeMB = *minFree
	}
	switch args[0] {
	case "serve":
		if err := runner.Serve(cfg, os.Stdin, os.Stdout); err != nil {
			return fail(err)
		}
		return 0
	case "status":
		json.NewEncoder(os.Stdout).Encode(runner.Room(cfg))
		return 0
	case "setup":
		out, err := runner.Setup(cfg, *hiveKey, *install)
		if err != nil {
			return fail(err)
		}
		fmt.Print(out)
		return 0
	}
	return 2
}

// home is the module's folder: the HOME the Hive's module runner gives it,
// or DISPATCH_HOME for the person running it by hand.
func home() (hive.Home, error) { return homeFrom(os.Getenv) }

func homeFrom(getenv func(string) string) (hive.Home, error) {
	if d := getenv("DISPATCH_HOME"); d != "" {
		return hive.Home(d), nil
	}
	h := getenv("HOME")
	if getenv("HIVE_MODULE") != "" || getenv("HIVE_DISPATCH") != "" {
		return hive.Home(h), nil
	}
	// The core runs `room`, `turn` and `hive dispatch ...` with the module's
	// folder as HOME and no HIVE_ variables (runner.Env scrubs them): until
	// this, every `room` failed here and no turn ever went to a runner.
	if h != "" && strings.HasSuffix(filepath.Clean(h), filepath.Join(".hive", "modules", "dispatch")) {
		return hive.Home(filepath.Clean(h)), nil
	}
	if d := getenv("HIVE_HOME"); d != "" {
		return hive.Home(filepath.Join(d, ".hive", "modules", "dispatch")), nil
	}
	d := filepath.Join(h, "hive", ".hive", "modules", "dispatch")
	if _, err := os.Stat(filepath.Dir(d)); err != nil {
		return "", errors.New("can't find the dispatch module's folder: set DISPATCH_HOME to <your hive>/.hive/modules/dispatch")
	}
	return hive.Home(d), nil
}

func fail(err error) int {
	if err == nil {
		return 0
	}
	fmt.Fprintln(os.Stderr, "dispatch:", err)
	return 1
}
