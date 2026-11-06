package record

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	transcriptFile   = "transcript.jsonl"
	handshakeTimeout = 30 * time.Second
	syncInterval     = time.Second
)

// TranscriptCapture silently records the ACP session/update stream of an
// agent run. It only ever sends initialize and session/new.
type TranscriptCapture struct {
	Store *Store
	Name  string
	UID   string
	URL   string
	Key   string
	Live  func() bool

	// injectable for tests
	backoffMin time.Duration
	backoffMax time.Duration
}

// Run captures until the agent is no longer live or ctx is done.
func (c *TranscriptCapture) Run(ctx context.Context) {
	bmin, bmax := c.backoffMin, c.backoffMax
	if bmin <= 0 {
		bmin = time.Second
	}
	if bmax <= 0 {
		bmax = 30 * time.Second
	}
	m, _, _ := c.Store.ReadMeta(c.Name, c.UID)
	m.Run, m.UID = c.Name, c.UID
	now := time.Now().UTC()
	if m.Started == nil {
		m.Started = &now
	}
	m.Ended = nil
	m.Transcript = "recording"
	f, size, err := c.Store.OpenAppend(c.Name, c.UID, transcriptFile)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	w := &syncWriter{f: f}
	if size > 0 {
		// hub restarted mid-run: repair a torn last line and mark the gap.
		if c.endsWithNewline(size) == false {
			w.raw([]byte("\n"))
		}
		from := c.lastTime()
		if from.IsZero() {
			from = *m.Started
		}
		c.addGap(w, &m, from, now)
	}
	_ = c.Store.WriteMeta(c.Name, c.UID, m)
	stop := make(chan struct{})
	done := make(chan struct{})
	go w.syncLoop(stop, done)

	connected := false    // a connection succeeded in this Run
	var gapFrom time.Time // set while disconnected after a connection
	var disconnected time.Time
	backoff := bmin
	for ctx.Err() == nil {
		conn, session, pending, err := c.connect(ctx)
		if err == nil {
			connected = true
			if !gapFrom.IsZero() {
				c.addGap(w, &m, gapFrom, time.Now().UTC())
				_ = c.Store.WriteMeta(c.Name, c.UID, m)
				gapFrom = time.Time{}
			}
			backoff = bmin
			for _, raw := range pending {
				w.write(RunFrame(raw, session))
			}
			c.read(ctx, conn, session, w)
			_ = conn.Close()
			disconnected = time.Now().UTC()
		}
		if ctx.Err() != nil || !c.Live() {
			break
		}
		if connected && gapFrom.IsZero() {
			gapFrom = disconnected
		}
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > bmax {
			backoff = bmax
		}
	}
	// ctx done while the run is still live: hub shutdown, the run goes on.
	interrupted := ctx.Err() != nil && c.Live()
	if !interrupted && !gapFrom.IsZero() {
		c.addGap(w, &m, gapFrom, time.Now().UTC())
	}
	close(stop)
	<-done
	if !interrupted {
		end := time.Now().UTC()
		m.Ended = &end
	}
	m.Transcript = "complete"
	if interrupted || len(m.Gaps) > 0 || w.failed.Load() {
		m.Transcript = "incomplete"
	}
	_ = c.Store.WriteMeta(c.Name, c.UID, m)
}

func (c *TranscriptCapture) addGap(w *syncWriter, m *Meta, from, to time.Time) {
	w.writeLine(GapLine(from, to))
	m.Gaps = append(m.Gaps, Gap{From: from.UTC(), To: to.UTC()})
}

func (c *TranscriptCapture) endsWithNewline(size int64) bool {
	f, err := os.Open(filepath.Join(c.Store.Dir(c.Name, c.UID), transcriptFile))
	if err != nil {
		return true
	}
	defer func() { _ = f.Close() }()
	b := make([]byte, 1)
	_, err = f.ReadAt(b, size-1)
	return err != nil || b[0] == '\n'
}

// lastTime returns the timestamp of the last parseable line in the transcript.
func (c *TranscriptCapture) lastTime() (t time.Time) {
	b, err := os.ReadFile(filepath.Join(c.Store.Dir(c.Name, c.UID), transcriptFile))
	if err != nil {
		return
	}
	ls := bytes.Split(b, []byte("\n"))
	for i := len(ls) - 1; i >= 0; i-- {
		var l struct {
			T   time.Time `json:"t"`
			Gap *Gap      `json:"gap"`
		}
		if json.Unmarshal(ls[i], &l) != nil {
			continue
		}
		if l.Gap != nil && !l.Gap.To.IsZero() {
			return l.Gap.To
		}
		if !l.T.IsZero() {
			return l.T
		}
	}
	return
}

type rpcMsg struct {
	ID     *json.RawMessage `json:"id"`
	Result struct {
		SessionID string `json:"sessionId"`
	} `json:"result"`
	Error *json.RawMessage `json:"error"`
}

// connect dials and performs initialize + session/new. Frames received
// before the handshake completes are returned as pending.
func (c *TranscriptCapture) connect(ctx context.Context) (conn *websocket.Conn, session string, pending [][]byte, err error) {
	conn, _, err = websocket.DefaultDialer.DialContext(ctx, c.URL, http.Header{"X-Secret-Key": {c.Key}})
	if err != nil {
		return
	}
	hs := make(chan struct{})
	defer close(hs)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-hs:
		}
	}()
	defer func() {
		if err != nil {
			_ = conn.Close()
			conn = nil
		}
	}()
	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	reqs := []struct {
		method string
		params string
	}{
		{"initialize", `{"protocolVersion":1,"clientCapabilities":{}}`},
		{"session/new", `{"cwd":"/workspace","mcpServers":[]}`},
	}
	for i, r := range reqs {
		id := i + 1
		msg := []byte(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"` + r.method + `","params":` + r.params + `}`)
		err = conn.WriteMessage(websocket.TextMessage, msg)
		if err != nil {
			return
		}
		for {
			var raw []byte
			_, raw, err = conn.ReadMessage()
			if err != nil {
				return
			}
			var res rpcMsg
			if json.Unmarshal(raw, &res) != nil || res.ID == nil {
				pending = append(pending, raw)
				continue
			}
			if string(*res.ID) != strconv.Itoa(id) {
				continue
			}
			if res.Error != nil {
				err = errors.New(r.method + " failed")
				return
			}
			session = res.Result.SessionID
			break
		}
	}
	_ = conn.SetReadDeadline(time.Time{})
	return
}

func (c *TranscriptCapture) read(ctx context.Context, conn *websocket.Conn, session string, w *syncWriter) {
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		w.write(RunFrame(raw, session))
	}
}

// syncWriter appends lines and fsyncs about once a second.
type syncWriter struct {
	f      *os.File
	mu     sync.Mutex
	dirty  atomic.Bool
	failed atomic.Bool
}

func (w *syncWriter) write(line []byte, keep bool) {
	if keep {
		w.writeLine(line)
	}
}

func (w *syncWriter) writeLine(line []byte) {
	b := make([]byte, 0, len(line)+1)
	b = append(append(b, line...), '\n')
	w.mu.Lock()
	_, err := w.f.Write(b)
	w.mu.Unlock()
	if err != nil {
		w.fail(err)
		return
	}
	w.dirty.Store(true)
}

// raw appends bytes verbatim.
func (w *syncWriter) raw(b []byte) {
	w.mu.Lock()
	_, err := w.f.Write(b)
	w.mu.Unlock()
	if err != nil {
		w.fail(err)
	}
}

func (w *syncWriter) fail(err error) {
	w.failed.Store(true)
	log.Printf("agentic transcript write failed: %v", err)
}

func (w *syncWriter) syncLoop(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(syncInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if w.dirty.Swap(false) {
				w.sync()
			}
		case <-stop:
			w.sync()
			return
		}
	}
}

func (w *syncWriter) sync() {
	if err := w.f.Sync(); err != nil {
		w.fail(err)
	}
}
