package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// ConfigPath is where the runner's settings live on its machine.
func ConfigPath() string {
	if p := os.Getenv("HIVE_DISPATCH_RUNNER_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "hive-dispatch", "runner.json")
}

// Load reads the runner's settings; none is all defaults.
func Load(path string) (Config, error) {
	var c Config
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	if err == nil {
		if err := json.Unmarshal(raw, &c); err != nil {
			return c, fmt.Errorf("%s: %w", path, err)
		}
	}
	c.Defaults()
	return c, nil
}

// Setup readies this machine as a runner for the Hive whose public key is
// hiveKey: it saves the settings, writes (with install) the authorized_keys
// line that lets that key run the runner and nothing else, and says what to
// run on the Hive's machine.
func Setup(cfg Config, hiveKey string, install bool) (string, error) {
	cfg.Defaults()
	var b strings.Builder
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	self, _ = filepath.EvalSymlinks(self)
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.MkdirAll(filepath.Dir(ConfigPath()), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(ConfigPath(), raw, 0o600); err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "settings: %s\n", ConfigPath())
	sb := probe(Config{Dir: cfg.Dir, Hive: cfg.Hive})
	os.Remove(filepath.Join(cfg.Dir, "probe"))
	fmt.Fprintf(&b, "sandbox (%s sandbox-probe): %s\n", cfg.Hive, sb)
	if !strings.HasPrefix(sb, "ok") {
		fmt.Fprintf(&b, "  the Hive offers this machine nothing until its sandbox works: install the hive binary (and bubblewrap on Ubuntu 23.10 or later)\n")
	}

	key := strings.Fields(hiveKey)
	if len(key) < 2 || !strings.HasPrefix(key[0], "ssh-") {
		return "", errors.New(`--hive-key "ssh-ed25519 AAAA..." is the Hive's key: on the Hive's machine, run: hive dispatch key`)
	}
	line := AuthorizedKey(self, key[0], key[1])
	home, _ := os.UserHomeDir()
	ak := filepath.Join(home, ".ssh", "authorized_keys")
	if install {
		have, _ := os.ReadFile(ak)
		if strings.Contains(string(have), key[1]) {
			fmt.Fprintf(&b, "%s already holds the Hive's key; left as it is\n", ak)
		} else {
			os.MkdirAll(filepath.Dir(ak), 0o700)
			f, err := os.OpenFile(ak, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return "", err
			}
			if len(have) > 0 && !strings.HasSuffix(string(have), "\n") {
				f.WriteString("\n")
			}
			_, err = f.WriteString(line + "\n")
			f.Close()
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "added the Hive's key to %s, held to running the runner:\n  %s\n", ak, line)
		}
	} else {
		fmt.Fprintf(&b, "add this line to %s (or run setup again with --install):\n  %s\n", ak, line)
	}

	hostKey := ""
	for _, k := range []string{"ssh_host_ed25519_key.pub", "ssh_host_ecdsa_key.pub", "ssh_host_rsa_key.pub"} {
		if raw, err := os.ReadFile(filepath.Join("/etc/ssh", k)); err == nil {
			if f := strings.Fields(string(raw)); len(f) >= 2 {
				hostKey = f[0] + " " + f[1]
				break
			}
		}
	}
	who := "you"
	if u, err := user.Current(); err == nil {
		who = u.Username
	}
	host, _ := os.Hostname()
	if hostKey == "" {
		fmt.Fprintf(&b, "no SSH host key in /etc/ssh: is the SSH server installed? (sudo apt install openssh-server)\n")
		hostKey = "<this machine's host key>"
	}
	fmt.Fprintf(&b, "\nthen, on the Hive's machine:\n  hive dispatch add %s ssh://%s@%s --host-key %q\n", cfg.Name, who, host, hostKey)
	return b.String(), nil
}

// AuthorizedKey is the authorized_keys line that lets the Hive's key start
// self's `runner serve` and nothing else. The path is quoted for the shell sshd
// runs it with (a path with a space or quote in it stays one word), and that
// is escaped for authorized_keys' own double quotes (sshd undoes only \").
func AuthorizedKey(self, typ, key string) string {
	cmd := "'" + strings.ReplaceAll(self, "'", `'\''`) + "' runner serve"
	cmd = strings.ReplaceAll(cmd, `"`, `\"`)
	return fmt.Sprintf(`command="%s",restrict %s %s hive-dispatch`, cmd, typ, key)
}
