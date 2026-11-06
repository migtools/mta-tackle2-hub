package record

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestTranscriptAllowed(t *testing.T) {
	mk := func(exec map[string]any) *unstructured.Unstructured {
		spec := map[string]any{}
		if exec != nil {
			spec["execution"] = exec
		}
		return &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	}
	cases := []struct {
		name   string
		u      *unstructured.Unstructured
		ok     bool
		reason string
	}{
		{"none", mk(nil), true, ""},
		{"auto", mk(map[string]any{"mode": "auto"}), true, ""},
		{"approve", mk(map[string]any{"mode": "approve"}), false, "approve mode"},
		{"askUser", mk(map[string]any{"askUser": true}), false, "askUser enabled"},
		{"askUser string", mk(map[string]any{"askUser": "true"}), false, "invalid execution spec"},
		{"mode int", mk(map[string]any{"mode": int64(1)}), false, "invalid execution spec"},
	}
	for _, c := range cases {
		ok, reason := TranscriptAllowed(c.u)
		if ok != c.ok || reason != c.reason {
			t.Errorf("%s: got %v %q", c.name, ok, reason)
		}
	}
}

func TestRunFrame(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		keep bool
	}{
		{"run session", `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"run"}}`, true},
		{"own session", `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"own"}}`, false},
		{"response", `{"jsonrpc":"2.0","id":1,"method":"session/update","params":{"sessionId":"run"}}`, false},
		{"goose", `{"jsonrpc":"2.0","method":"_goose/unstable/session/update","params":{"sessionId":"run"}}`, true},
		{"other method", `{"jsonrpc":"2.0","method":"foo","params":{"sessionId":"run"}}`, false},
		{"malformed", `{not json`, false},
	}
	for _, c := range cases {
		line, keep := RunFrame([]byte(c.raw), "own")
		if keep != c.keep {
			t.Errorf("%s: keep=%v", c.name, keep)
			continue
		}
		if keep {
			var out struct {
				T     string          `json:"t"`
				Frame json.RawMessage `json:"frame"`
			}
			if err := json.Unmarshal(line, &out); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if _, err := time.Parse(time.RFC3339Nano, out.T); err != nil {
				t.Errorf("%s: bad t: %v", c.name, err)
			}
			if string(out.Frame) != c.raw {
				t.Errorf("%s: frame %s", c.name, out.Frame)
			}
		}
	}
}

func TestGapLine(t *testing.T) {
	a := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got := string(GapLine(a, a.Add(time.Second)))
	want := `{"gap":{"from":"2026-01-01T00:00:00Z","to":"2026-01-01T00:00:01Z"}}`
	if strings.TrimSpace(got) != want {
		t.Fatalf("got %s", got)
	}
}
