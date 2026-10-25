package hive

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAddPinsHostKey(t *testing.T) {
	h := Home(t.TempDir())
	r := Runner{Name: "iiks1", Address: "ssh://ihsan@iiks1.lan:2222"}
	if err := h.Add(r, ""); err == nil {
		t.Fatal("an ssh runner was added with no host key to pin")
	}
	if err := h.Add(Runner{Name: "Bad Name", Address: "exec:x"}, ""); err == nil {
		t.Fatal("a bad name was taken")
	}
	if err := h.Add(r, "ssh-ed25519 AAAAC3Nza hostcomment"); err != nil {
		t.Fatal(err)
	}
	kh, _ := os.ReadFile(h.KnownHosts())
	if strings.TrimSpace(string(kh)) != "iiks1 ssh-ed25519 AAAAC3Nza" {
		t.Errorf("known_hosts = %q", kh)
	}
	argv, err := h.Argv(r)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{"-F /dev/null", "IdentityAgent=none", "IdentitiesOnly=yes", "StrictHostKeyChecking=yes", "HostKeyAlias=iiks1",
		"UserKnownHostsFile=" + h.KnownHosts(), "-i " + h.Key(), "-p 2222", "-l ihsan iiks1.lan " + RemoteCommand} {
		if !strings.Contains(joined, want) {
			t.Errorf("ssh argv lacks %q: %s", want, joined)
		}
	}
	if err := h.Remove("iiks1"); err != nil {
		t.Fatal(err)
	}
	if kh, _ := os.ReadFile(h.KnownHosts()); strings.Contains(string(kh), "iiks1") {
		t.Errorf("the host key stayed pinned: %q", kh)
	}
	if rs, _ := h.Runners(); len(rs) != 0 {
		t.Errorf("runners = %+v", rs)
	}
}

func TestPublicKey(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen")
	}
	h := Home(t.TempDir())
	a, err := h.PublicKey()
	if err != nil || !strings.HasPrefix(a, "ssh-ed25519 ") {
		t.Fatalf("%q %v", a, err)
	}
	if b, _ := h.PublicKey(); a != b {
		t.Error("the key changed on a second read")
	}
	if fi, _ := os.Stat(h.Key()); fi.Mode().Perm() != 0o600 {
		t.Errorf("private key mode %v", fi.Mode().Perm())
	}
}

// The module's program reports the board and each placed turn to the Hive.
func TestServeReports(t *testing.T) {
	w := newWorld(t)
	if code := w.delegate(); code != 3 {
		t.Fatalf("exit %d", code)
	}
	var mu sync.Mutex
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/module/report" || r.Header.Get("Authorization") != "Bearer tok" {
			rw.WriteHeader(403)
			return
		}
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		got = append(got, b)
		mu.Unlock()
	}))
	defer srv.Close()
	go w.home.Serve(srv.URL, "tok")
	deadline := time.Now().Add(15 * time.Second)
	for {
		mu.Lock()
		kinds := map[string]string{}
		for _, b := range got {
			kinds[b["kind"].(string)] += b["text"].(string)
		}
		mu.Unlock()
		if strings.Contains(kinds["board"], "fake (exec:") && strings.Contains(kinds["turn"], "w-test turn-001.jsonl on fake: exit 3") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("reports = %+v", got)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
