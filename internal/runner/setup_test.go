package runner

import (
	"os/exec"
	"strings"
	"testing"
)

// The forced command keeps an awkward path one word: sshd undoes \" inside
// command="...", then hands the rest to the shell.
func TestAuthorizedKeyQuotesPath(t *testing.T) {
	for _, self := range []string{"/usr/local/bin/hive-dispatch", "/home/i ks/my bin/hive-dispatch", `/opt/it's "here"/hive-dispatch`} {
		line := AuthorizedKey(self, "ssh-ed25519", "AAAAkey")
		cmd, rest, ok := strings.Cut(strings.TrimPrefix(line, `command="`), `",restrict `)
		if !ok || rest != "ssh-ed25519 AAAAkey hive-dispatch" {
			t.Fatalf("line %q", line)
		}
		cmd = strings.ReplaceAll(cmd, `\"`, `"`)
		word, ok := strings.CutSuffix(cmd, " runner serve")
		if !ok {
			t.Fatalf("command %q", cmd)
		}
		out, err := exec.Command("sh", "-c", "printf %s "+word).Output()
		if err != nil || string(out) != self {
			t.Errorf("%q: the shell read %q (%v)", self, out, err)
		}
	}
}
