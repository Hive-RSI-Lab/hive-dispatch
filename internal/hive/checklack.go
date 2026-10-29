package hive

import (
	"bytes"
	"io"
	"path/filepath"
	"regexp"

	"github.com/Hive-RSI-Lab/hive-dispatch/internal/spec"
)

// CheckProject is the project a check the core handed over belongs to: the
// core writes a check's output to <hive>/checks/<project>/<id>.log. "" when
// s isn't a check or its output isn't there.
func CheckProject(s spec.Spec) string {
	if !IsCheck(s) || s.Stdout == "" {
		return ""
	}
	dir := filepath.Dir(s.Stdout)
	if filepath.Base(filepath.Dir(dir)) != "checks" {
		return ""
	}
	return filepath.Base(dir)
}

// missingTool are the ways a check says it couldn't find a program: the
// shell's own (only with exit 127, as the core reads it), Go's exec, and a
// project's check script saying a tool "is not on PATH".
var (
	shellNotFound = regexp.MustCompile(`(?m)^(?:\S*sh: (?:line )?\d*:? ?([^\s/:']+): (?:command )?not found|env: ['‘]?([^\s/:'‘’]+)['’]?: No such file or directory|zsh:(?:\d+: )? ?command not found: ([^\s/:']+))\s*$`)
	execNotFound  = regexp.MustCompile(`exec: "([^"/\s]+)": executable file not found in \$PATH`)
	notOnPath     = regexp.MustCompile(`\b([A-Za-z0-9_][A-Za-z0-9_.+-]*) is not on (?:the |your )?\$?PATH\b`)
)

// MissingTool names the program a check that failed with code couldn't
// find, reading the end of its output, or "" when it failed on its own. On a
// runner that means the runner lacks it, not that the commit is red; read
// wrongly, it costs only a run here.
func MissingTool(code int, out string) string {
	if code == 0 {
		return ""
	}
	if code == 127 {
		if m := shellNotFound.FindStringSubmatch(out); m != nil {
			return m[1] + m[2] + m[3]
		}
	}
	for _, re := range []*regexp.Regexp{execNotFound, notOnPath} {
		if m := re.FindStringSubmatch(out); m != nil {
			return m[1]
		}
	}
	return ""
}

// markerHold writes a check's stdout through, but holds back the core's exit
// marker (a line "hive-check-exit <nonce> <code>", which the core's wrapper
// prints last) until the check ends, so a check that couldn't run there can
// end without it and the core reads it as can't-run, not as its exit.
type markerHold struct {
	w    io.Writer
	part []byte // a line not yet ended
	held []byte // the last marker line, and nothing after it yet
	tail []byte // the end of the output, to read a missing tool in
	done bool
}

const tailMax = 8 << 10

var markerTag = []byte("hive-check-exit ")

func (m *markerHold) Write(p []byte) (int, error) {
	m.tail = keepTail(m.tail, p)
	m.part = append(m.part, p...)
	for {
		i := bytes.IndexByte(m.part, '\n')
		if i < 0 {
			break
		}
		line := m.part[:i+1]
		if bytes.HasPrefix(line, markerTag) {
			m.w.Write(m.held)
			m.held = append([]byte(nil), line...)
		} else {
			m.w.Write(m.held)
			m.held = nil
			m.w.Write(line)
		}
		m.part = m.part[i+1:]
	}
	if len(m.part) > 0 && !bytes.HasPrefix(m.part, markerTag) && !bytes.HasPrefix(markerTag, m.part) {
		// Not the start of a marker: no need to hold it.
		m.w.Write(m.held)
		m.held = nil
		m.w.Write(m.part)
		m.part = nil
	}
	return len(p), nil
}

// end writes what is held, or with drop, all of it but the marker.
func (m *markerHold) end(drop bool) {
	if m.done {
		return
	}
	m.done = true
	if !drop {
		m.w.Write(m.held)
	}
	m.w.Write(m.part)
}

func keepTail(tail, p []byte) []byte {
	tail = append(tail, p...)
	if len(tail) > tailMax {
		tail = append(tail[:0], tail[len(tail)-tailMax:]...)
	}
	return tail
}
