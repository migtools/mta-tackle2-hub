package record

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const testUID = "11111111-2222-3333-4444-555555555555"

type fakeAgent struct {
	mu      sync.Mutex
	methods [][]string
	conns   int32
	// frames sent per connection index (after handshake); then close.
	frames [][]string
	key    atomic.Value
	// reject, if set, refuses the websocket upgrade for that 0-based attempt.
	reject   func(attempt int) bool
	attempts int32
	t        *testing.T
}

func update(session, text string) string {
	return `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"` + session +
		`","update":{"text":"` + text + `"}}}`
}

func (f *fakeAgent) server(t *testing.T) *httptest.Server {
	up := websocket.Upgrader{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.key.Store(r.Header.Get("X-Secret-Key"))
		attempt := int(atomic.AddInt32(&f.attempts, 1)) - 1
		if f.reject != nil && f.reject(attempt) {
			http.Error(w, "no", http.StatusServiceUnavailable)
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		idx := int(atomic.AddInt32(&f.conns, 1)) - 1
		f.mu.Lock()
		f.methods = append(f.methods, nil)
		f.mu.Unlock()
		for i := 0; i < 2; i++ {
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if c.ReadJSON(&req) != nil {
				return
			}
			f.mu.Lock()
			f.methods[idx] = append(f.methods[idx], req.Method)
			f.mu.Unlock()
			want := map[string]string{
				"initialize":  `{"protocolVersion":1,"clientCapabilities":{}}`,
				"session/new": `{"cwd":"/workspace","mcpServers":[]}`,
			}[req.Method]
			if string(req.Params) != want && f.t != nil {
				f.t.Errorf("%s params = %s, want %s", req.Method, req.Params, want)
			}
			res := `{}`
			if req.Method == "session/new" {
				res = `{"sessionId":"viewer-1"}`
			}
			_ = c.WriteMessage(websocket.TextMessage, []byte(`{"jsonrpc":"2.0","id":`+string(req.ID)+`,"result":`+res+`}`))
		}
		if idx < len(f.frames) {
			for _, fr := range f.frames[idx] {
				_ = c.WriteMessage(websocket.TextMessage, []byte(fr))
			}
		}
		// Observe anything else the client sends until it closes.
		_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		for {
			var req struct {
				Method string `json:"method"`
			}
			if c.ReadJSON(&req) != nil {
				return
			}
			f.mu.Lock()
			f.methods[idx] = append(f.methods[idx], req.Method)
			f.mu.Unlock()
		}
	}))
}

func newCapture(t *testing.T, srv *httptest.Server, live func() bool) (*TranscriptCapture, *Store) {
	s := &Store{Root: t.TempDir()}
	return &TranscriptCapture{
		Store: s, Name: "run-1", UID: testUID,
		URL: "ws" + strings.TrimPrefix(srv.URL, "http"), Key: "sekret", Live: live,
		backoffMin: time.Millisecond, backoffMax: 5 * time.Millisecond,
	}, s
}

func lines(t *testing.T, s *Store) []string {
	b, err := os.ReadFile(filepath.Join(s.Dir("run-1", testUID), "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		return nil
	}
	if b[len(b)-1] != '\n' {
		t.Fatal("transcript does not end with newline")
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func TestCaptureWritesRunFramesOnly(t *testing.T) {
	f := &fakeAgent{frames: [][]string{{update("run-1", "a"), update("run-1", "b"), update("viewer-1", "x"), update("run-1", "c")}}}
	f.t = t
	srv := f.server(t)
	defer srv.Close()
	c, s := newCapture(t, srv, func() bool { return false })
	c.Run(context.Background())
	if got := lines(t, s); len(got) != 3 {
		t.Fatalf("want 3 lines, got %d: %v", len(got), got)
	}
	m, found, err := s.ReadMeta("run-1", testUID)
	if err != nil || !found || m.Transcript != "complete" || m.Started == nil || m.Ended == nil {
		t.Fatalf("meta: %+v %v %v", m, found, err)
	}
	if f.key.Load() != "sekret" {
		t.Fatal("secret key header missing")
	}
}

func TestGapMarkerOnReconnect(t *testing.T) {
	f := &fakeAgent{frames: [][]string{{update("run-1", "a")}, {update("run-1", "b"), update("run-1", "c")}}}
	f.t = t
	srv := f.server(t)
	defer srv.Close()
	var n int32
	c, s := newCapture(t, srv, func() bool { return atomic.AddInt32(&n, 1) == 1 })
	c.Run(context.Background())
	got := lines(t, s)
	if len(got) != 4 || !strings.HasPrefix(got[1], `{"gap":`) {
		t.Fatalf("unexpected lines: %v", got)
	}
	m, _, _ := s.ReadMeta("run-1", testUID)
	if m.Transcript != "incomplete" || len(m.Gaps) != 1 {
		t.Fatalf("meta: %+v", m)
	}
}

func TestNeverPrompts(t *testing.T) {
	f := &fakeAgent{frames: [][]string{{update("run-1", "a")}, {update("run-1", "b")}}}
	f.t = t
	srv := f.server(t)
	defer srv.Close()
	var n int32
	c, _ := newCapture(t, srv, func() bool { return atomic.AddInt32(&n, 1) == 1 })
	c.Run(context.Background())
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.methods) != 2 {
		t.Fatalf("want 2 connections, got %d", len(f.methods))
	}
	for i, m := range f.methods {
		if strings.Join(m, ",") != "initialize,session/new" {
			t.Fatalf("conn %d methods: %v", i, m)
		}
	}
}

func TestGapOnRestartWithExistingFile(t *testing.T) {
	f := &fakeAgent{frames: [][]string{{update("run-1", "b")}}}
	f.t = t
	srv := f.server(t)
	defer srv.Close()
	c, s := newCapture(t, srv, func() bool { return false })
	past := time.Now().Add(-time.Minute).UTC()
	file, _, err := s.OpenAppend("run-1", testUID, "transcript.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	old, _ := json.Marshal(past.Format(time.RFC3339Nano))
	_, _ = file.WriteString(`{"t":` + string(old) + `,"frame":{}}` + "\n")
	_ = file.Close()
	c.Run(context.Background())
	got := lines(t, s)
	if len(got) != 3 || !strings.HasPrefix(got[1], `{"gap":`) {
		t.Fatalf("unexpected lines: %v", got)
	}
	var g struct{ Gap Gap }
	if err := json.Unmarshal([]byte(got[1]), &g); err != nil || !g.Gap.From.Equal(past.Truncate(0)) {
		t.Fatalf("gap from: %+v %v", g, err)
	}
	m, _, _ := s.ReadMeta("run-1", testUID)
	if m.Transcript != "incomplete" || len(m.Gaps) != 1 {
		t.Fatalf("meta: %+v", m)
	}
}

func TestGapRecordedWhenRunEndsBeforeReconnect(t *testing.T) {
	f := &fakeAgent{frames: [][]string{{update("run-1", "a")}}, reject: func(a int) bool { return a >= 1 }}
	f.t = t
	srv := f.server(t)
	defer srv.Close()
	var n int32
	c, s := newCapture(t, srv, func() bool { return atomic.AddInt32(&n, 1) == 1 })
	c.Run(context.Background())
	got := lines(t, s)
	if len(got) != 2 || !strings.HasPrefix(got[1], `{"gap":`) {
		t.Fatalf("unexpected lines: %v", got)
	}
	m, _, _ := s.ReadMeta("run-1", testUID)
	if m.Transcript != "incomplete" || len(m.Gaps) != 1 || m.Ended == nil {
		t.Fatalf("meta: %+v", m)
	}
}

func TestCtxCancelWhileLiveIsNotComplete(t *testing.T) {
	f := &fakeAgent{frames: [][]string{{update("run-1", "a")}}}
	f.t = t
	srv := f.server(t)
	defer srv.Close()
	c, s := newCapture(t, srv, func() bool { return true })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(filepath.Join(s.Dir("run-1", testUID), "transcript.jsonl"))
		if len(b) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	m, _, _ := s.ReadMeta("run-1", testUID)
	if m.Transcript != "incomplete" || m.Ended != nil {
		t.Fatalf("meta: %+v", m)
	}
}

func TestNoGapBeforeFirstConnect(t *testing.T) {
	f := &fakeAgent{frames: [][]string{{update("run-1", "a")}}, reject: func(a int) bool { return a == 0 }}
	f.t = t
	srv := f.server(t)
	defer srv.Close()
	var n int32
	c, s := newCapture(t, srv, func() bool { return atomic.AddInt32(&n, 1) == 1 })
	c.Run(context.Background())
	got := lines(t, s)
	if len(got) != 1 || strings.HasPrefix(got[0], `{"gap":`) {
		t.Fatalf("unexpected lines: %v", got)
	}
	m, _, _ := s.ReadMeta("run-1", testUID)
	if m.Transcript != "complete" || len(m.Gaps) != 0 {
		t.Fatalf("meta: %+v", m)
	}
}

func TestRestartAfterTornLine(t *testing.T) {
	f := &fakeAgent{frames: [][]string{{update("run-1", "b")}}}
	f.t = t
	srv := f.server(t)
	defer srv.Close()
	c, s := newCapture(t, srv, func() bool { return false })
	file, _, err := s.OpenAppend("run-1", testUID, "transcript.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString(`{"t":"2026-11-06T00:00:00Z","frame":{"a":1}}` + "\n" + `{"t":"2026-11-06T00:00:0`)
	_ = file.Close()
	c.Run(context.Background())
	got := lines(t, s)
	if len(got) != 4 {
		t.Fatalf("unexpected lines: %v", got)
	}
	for i, l := range got {
		if i != 1 && !json.Valid([]byte(l)) {
			t.Fatalf("line %d invalid: %s", i, l)
		}
	}
	var g struct{ Gap Gap }
	want, _ := time.Parse(time.RFC3339, "2026-11-06T00:00:00Z")
	if err := json.Unmarshal([]byte(got[2]), &g); err != nil || !g.Gap.From.Equal(want) {
		t.Fatalf("gap: %+v %v", g, err)
	}
}
