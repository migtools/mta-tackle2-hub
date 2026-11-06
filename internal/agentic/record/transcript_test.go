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
}

func update(session, text string) string {
	return `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"` + session +
		`","update":{"text":"` + text + `"}}}`
}

func (f *fakeAgent) server(t *testing.T) *httptest.Server {
	up := websocket.Upgrader{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.key.Store(r.Header.Get("X-Secret-Key"))
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
			}
			if c.ReadJSON(&req) != nil {
				return
			}
			f.mu.Lock()
			f.methods[idx] = append(f.methods[idx], req.Method)
			f.mu.Unlock()
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
