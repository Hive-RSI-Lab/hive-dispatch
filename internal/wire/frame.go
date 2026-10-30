// Package wire is the link between the Hive and a remote runner: framed
// messages over one byte stream (an SSH session's stdin and stdout, or a local
// command's), and the file trees a turn carries each way.
//
// A frame is one type byte, a big-endian uint32 length and the payload.
package wire

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// Version is the protocol both sides must speak. 2: a turn carries the
// Hive's own hive binary (Turn.Hive), so a runner from before it, whose
// turns would run an older hive CLI than the core's, takes none.
var Version = 2

// protocol, set at link time (-ldflags "-X .../wire.protocol=1"), builds a
// dispatch that speaks another protocol: a test's runner one protocol behind
// the Hive's, which must still take the Hive's update.
var protocol string

func init() {
	if v, err := strconv.Atoi(protocol); err == nil && v > 0 {
		Version = v
	}
}

// Frame types.
const (
	JSON   = 'J' // a JSON message, or the last piece of one that came in Big frames
	Big    = 'B' // a piece of a JSON message too big for one frame; a J frame ends it
	Tar    = 'T' // a chunk of a tar stream
	End    = 'E' // the end of a tar stream
	Out    = 'O' // turn stdout
	Err    = 'R' // turn stderr
	Outbox = 'M' // a mailbox request, runner to Hive
	Inbox  = 'I' // a mailbox response, Hive to runner
	Stop   = 'S' // stop the turn
	Exit   = 'X' // the turn's exit code (JSON)
	Done   = 'Z' // nothing more follows
)

// MaxFrame bounds one frame's payload.
const MaxFrame = 4 << 20

// MaxJSON bounds one JSON message sent in pieces (a runner's manifests of its
// cached mounts run to several MB once it holds the Go SDK).
const MaxJSON = 1 << 30

// Conn reads and writes frames; writes are safe from several goroutines.
type Conn struct {
	r   *bufio.Reader
	w   io.Writer
	wmu sync.Mutex
}

// New wraps a stream.
func New(r io.Reader, w io.Writer) *Conn { return &Conn{r: bufio.NewReaderSize(r, 64<<10), w: w} }

// Send writes one frame.
func (c *Conn) Send(t byte, payload []byte) error {
	if len(payload) > MaxFrame {
		return fmt.Errorf("frame of %d bytes is over the limit", len(payload))
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.write(t, payload)
}

// write sends one frame; the caller holds wmu.
func (c *Conn) write(t byte, payload []byte) error {
	var h [5]byte
	h[0] = t
	binary.BigEndian.PutUint32(h[1:], uint32(len(payload)))
	if _, err := c.w.Write(h[:]); err != nil {
		return err
	}
	_, err := c.w.Write(payload)
	return err
}

// SendJSON writes v as a frame of type t. A JSON message over MaxFrame goes
// as Big frames and a last J frame, which Recv puts back together; a peer that
// predates Big frames couldn't have taken such a message anyway.
func (c *Conn) SendJSON(t byte, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if t != JSON || len(raw) <= MaxFrame {
		return c.Send(t, raw)
	}
	if len(raw) > MaxJSON {
		return fmt.Errorf("JSON message of %d bytes is over the limit", len(raw))
	}
	// Hold the lock throughout so no other frame lands between the pieces.
	c.wmu.Lock()
	defer c.wmu.Unlock()
	for len(raw) > MaxFrame {
		if err := c.write(Big, raw[:MaxFrame]); err != nil {
			return err
		}
		raw = raw[MaxFrame:]
	}
	return c.write(JSON, raw)
}

// Recv reads one frame; Big frames come back joined with the J frame that
// ends them.
func (c *Conn) Recv() (byte, []byte, error) {
	var big []byte
	for {
		t, p, err := c.recv()
		if err != nil || (t != Big && big == nil) {
			return t, p, err
		}
		if len(big)+len(p) > MaxJSON {
			return 0, nil, fmt.Errorf("JSON message of over %d bytes is over the limit", MaxJSON)
		}
		big = append(big, p...)
		switch t {
		case Big:
		case JSON:
			return JSON, big, nil
		default:
			return 0, nil, fmt.Errorf("frame %q inside a JSON message", t)
		}
	}
}

func (c *Conn) recv() (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(c.r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > MaxFrame {
		return 0, nil, fmt.Errorf("frame of %d bytes is over the limit", n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(c.r, p); err != nil {
		return 0, nil, err
	}
	return h[0], p, nil
}

// Expect reads one frame that must be of type t and decodes JSON into v.
func (c *Conn) Expect(t byte, v any) error {
	got, p, err := c.Recv()
	if err != nil {
		return err
	}
	if got != t {
		return fmt.Errorf("expected frame %q, got %q", t, got)
	}
	if v == nil {
		return nil
	}
	return json.Unmarshal(p, v)
}

// TarWriter returns a writer whose bytes go out as Tar frames; Close sends End.
func (c *Conn) TarWriter() io.WriteCloser { return &chunkWriter{c: c} }

type chunkWriter struct {
	c   *Conn
	buf []byte
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for len(w.buf) >= 1<<20 {
		if err := w.c.Send(Tar, w.buf[:1<<20]); err != nil {
			return 0, err
		}
		w.buf = w.buf[1<<20:]
	}
	return len(p), nil
}

func (w *chunkWriter) Close() error {
	if len(w.buf) > 0 {
		if err := w.c.Send(Tar, w.buf); err != nil {
			return err
		}
		w.buf = nil
	}
	return w.c.Send(End, nil)
}

// TarReader reads Tar frames until End. The caller must read it to its end
// before reading another frame.
func (c *Conn) TarReader() io.Reader { return &chunkReader{c: c} }

type chunkReader struct {
	c    *Conn
	buf  []byte
	done bool
}

func (r *chunkReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		if r.done {
			return 0, io.EOF
		}
		t, payload, err := r.c.Recv()
		if err != nil {
			return 0, err
		}
		switch t {
		case Tar:
			r.buf = payload
		case End:
			r.done = true
		default:
			return 0, fmt.Errorf("frame %q inside a tar stream", t)
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// Drain reads what is left of a tar stream.
func Drain(r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}
