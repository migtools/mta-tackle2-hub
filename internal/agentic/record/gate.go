package record

import (
	"bytes"
	"encoding/json"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TranscriptAllowed reports whether the run transcript may be recorded.
// Runs in approve mode or with askUser enabled are not recorded.
func TranscriptAllowed(u *unstructured.Unstructured) (ok bool, reason string) {
	mode, _, _ := unstructured.NestedString(u.Object, "spec", "execution", "mode")
	if mode == "approve" {
		return false, "approve mode"
	}
	askUser, _, _ := unstructured.NestedBool(u.Object, "spec", "execution", "askUser")
	if askUser {
		return false, "askUser enabled"
	}
	return true, ""
}

// RunFrame filters a raw websocket frame. Only JSON-RPC notifications
// (no id) of session/update kinds for a session other than ownSession are kept.
// The returned line is {"t":"<RFC3339Nano>","frame":<raw>}.
func RunFrame(raw []byte, ownSession string) (line []byte, keep bool) {
	return runFrame(raw, ownSession, time.Now)
}

func runFrame(raw []byte, ownSession string, now func() time.Time) (line []byte, keep bool) {
	var f struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			SessionID string `json:"sessionId"`
		} `json:"params"`
	}
	if json.Unmarshal(raw, &f) != nil {
		return
	}
	if len(f.ID) > 0 {
		return
	}
	if f.Method != "session/update" && f.Method != "_goose/unstable/session/update" {
		return
	}
	if f.Params.SessionID == ownSession {
		return
	}
	t, err := json.Marshal(now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return
	}
	// compact so the line never contains a newline (JSONL).
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return
	}
	line = make([]byte, 0, buf.Len()+len(t)+20)
	line = append(line, `{"t":`...)
	line = append(line, t...)
	line = append(line, `,"frame":`...)
	line = append(line, buf.Bytes()...)
	line = append(line, '}')
	keep = true
	return
}

// GapLine returns a JSON line marking a recording gap.
func GapLine(from, to time.Time) []byte {
	b, _ := json.Marshal(Gap{From: from.UTC(), To: to.UTC()})
	return append(append([]byte(`{"gap":`), b...), '}')
}
