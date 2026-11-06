package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agent "github.com/konveyor/agentic-controller/api/v1alpha1"
	"github.com/konveyor/tackle2-hub/internal/agentic/record"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gin-gonic/gin"
)

const recUID = "44444444-4444-4444-4444-444444444444"

// recorderFixture wires an AgenticHandler to a fake k8s client and a
// temporary recorder store.
type recorderFixture struct {
	t      *testing.T
	store  *record.Store
	logs   map[string]string // container -> log text served live
	client client.Client
	h      AgenticHandler
}

func newRecorderFixture(t *testing.T, objs ...client.Object) *recorderFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	oldNS := Settings.Hub.Namespace
	oldStore, oldOpener := AgentRunStore, AgentRunLogOpener
	Settings.Hub.Namespace = "konveyor-tackle"
	sch := scheme.Scheme
	if err := agent.SchemeBuilder.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	f := &recorderFixture{
		t:     t,
		store: &record.Store{Root: t.TempDir()},
		logs:  map[string]string{},
	}
	AgentRunLogOpener = func(ctx context.Context, pod, container string, follow bool) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(f.logs[container])), nil
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
	f.h = AgenticHandler{}
	f.client = c
	AgentRunStore = f.store
	t.Cleanup(func() {
		Settings.Hub.Namespace = oldNS
		AgentRunStore, AgentRunLogOpener = oldStore, oldOpener
	})
	return f
}

func (f *recorderFixture) get(path string) *httptest.ResponseRecorder {
	f.t.Helper()
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	rtx := RichContext(ctx)
	rtx.Client = f.client
	ctx.Params = gin.Params{{Key: "name", Value: "run-a"}}
	ctx.Request = httptest.NewRequest(http.MethodGet, path, nil)
	if strings.HasPrefix(path, "/logs") {
		f.h.AgentRunLogs(ctx)
	} else {
		f.h.AgentRunTranscript(ctx)
	}
	// The real engine flushes the pending status after the handler.
	ctx.Writer.WriteHeaderNow()
	return w
}

// runCR builds an AgentRun CR object.
func runCR(name, uid, phase string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(recRunGVK)
	u.SetName(name)
	u.SetNamespace("konveyor-tackle")
	u.SetUID(types.UID(uid))
	_ = unstructured.SetNestedField(u.Object, phase, "status", "phase")
	return u
}

func podCR(name string) *core.Pod {
	return &core.Pod{ObjectMeta: meta.ObjectMeta{Name: name, Namespace: "konveyor-tackle"}}
}

func (f *recorderFixture) writeFile(file, content string) {
	f.t.Helper()
	w, _, err := f.store.OpenAppend("run-a", recUID, file)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := w.WriteString(content); err != nil {
		f.t.Fatal(err)
	}
	_ = w.Close()
}

func TestLogsSavedFromOffset(t *testing.T) {
	f := newRecorderFixture(t)
	f.writeFile("agent.log", "abcdef")
	w := f.get("/logs?container=agent&offset=2")
	if w.Code != http.StatusOK {
		t.Fatalf("code %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "cdef" {
		t.Fatalf("body %q", w.Body.String())
	}
	if w.Header().Get("X-Log-Source") != "saved" {
		t.Fatalf("source %q", w.Header().Get("X-Log-Source"))
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("content-type %q", ct)
	}
}

func TestLogsLivePreferred(t *testing.T) {
	f := newRecorderFixture(t, runCR("run-a", recUID, "Running"), podCR("run-a"))
	f.logs["agent"] = "live-log\n"
	w := f.get("/logs?container=agent&follow=false")
	if w.Code != http.StatusOK {
		t.Fatalf("code %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "live-log\n" {
		t.Fatalf("body %q", w.Body.String())
	}
	if w.Header().Get("X-Log-Source") != "live" {
		t.Fatalf("source %q", w.Header().Get("X-Log-Source"))
	}
}

func TestLogsNotFound(t *testing.T) {
	f := newRecorderFixture(t)
	w := f.get("/logs?container=agent")
	if w.Code != http.StatusNotFound {
		t.Fatalf("code %d", w.Code)
	}
}

func TestTranscriptOffsetLines(t *testing.T) {
	f := newRecorderFixture(t)
	f.writeFile("transcript.jsonl", "{\"t\":\"1\"}\n{\"t\":\"2\"}\n{\"t\":\"3\"}\n")
	_ = f.store.WriteMeta("run-a", recUID, record.Meta{
		Run: "run-a", UID: recUID, Transcript: "complete",
	})
	w := f.get("/transcript?offset=1")
	if w.Code != http.StatusOK {
		t.Fatalf("code %d: %s", w.Code, w.Body.String())
	}
	lines := strings.Split(strings.TrimSuffix(w.Body.String(), "\n"), "\n")
	if len(lines) != 2 || lines[0] != "{\"t\":\"2\"}" {
		t.Fatalf("body %q", w.Body.String())
	}
	if w.Header().Get("X-Transcript-Status") != "complete" {
		t.Fatalf("status %q", w.Header().Get("X-Transcript-Status"))
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/x-ndjson") {
		t.Fatalf("content-type %q", ct)
	}
}

func TestTranscriptSkippedHeader(t *testing.T) {
	f := newRecorderFixture(t)
	_ = f.store.WriteMeta("run-a", recUID, record.Meta{
		Run: "run-a", UID: recUID, Transcript: "skipped", SkipReason: "approve mode",
	})
	w := f.get("/transcript")
	if w.Code != http.StatusOK {
		t.Fatalf("code %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "" {
		t.Fatalf("body %q", w.Body.String())
	}
	if w.Header().Get("X-Transcript-Status") != "skipped" ||
		w.Header().Get("X-Transcript-Skip-Reason") != "approve mode" {
		t.Fatalf("headers %q %q",
			w.Header().Get("X-Transcript-Status"),
			w.Header().Get("X-Transcript-Skip-Reason"))
	}
}

func TestTerminalRunNoRecording(t *testing.T) {
	f := newRecorderFixture(t, runCR("run-a", recUID, "Failed"), podCR("run-a"))
	f.logs["agent"] = "tail of pod\n"
	w := f.get("/logs?container=agent&follow=false")
	if w.Code != http.StatusOK || w.Header().Get("X-Log-Source") != "live" {
		t.Fatalf("logs: code %d source %q", w.Code, w.Header().Get("X-Log-Source"))
	}
	w = f.get("/transcript")
	if w.Code != http.StatusNotFound {
		t.Fatalf("transcript code %d", w.Code)
	}
	if w.Header().Get("X-Transcript-Status") != "none" {
		t.Fatalf("status %q", w.Header().Get("X-Transcript-Status"))
	}
}

func TestTranscriptNoneWhenNoDir(t *testing.T) {
	f := newRecorderFixture(t)
	w := f.get("/transcript")
	if w.Code != http.StatusNotFound {
		t.Fatalf("code %d", w.Code)
	}
	if w.Header().Get("X-Transcript-Status") != "none" {
		t.Fatalf("status %q", w.Header().Get("X-Transcript-Status"))
	}
}

func TestLogsRejectsBadContainerName(t *testing.T) {
	f := newRecorderFixture(t)
	w := f.get("/logs?container=../../etc")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code %d", w.Code)
	}
}
