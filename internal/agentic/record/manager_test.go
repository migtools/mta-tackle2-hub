package record

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const testNS = "konveyor-tackle"

type runOpt func(u *unstructured.Unstructured)

func withMode(mode string) runOpt {
	return func(u *unstructured.Unstructured) {
		_ = unstructured.SetNestedField(u.Object, mode, "spec", "execution", "mode")
	}
}

func agentRun(name, uid, phase string, acpReady bool, opts ...runOpt) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetGroupVersionKind(agentRunGVK)
	u.SetName(name)
	u.SetNamespace(testNS)
	u.SetUID(types.UID(uid))
	ready := "False"
	if acpReady {
		ready = "True"
	}
	_ = unstructured.SetNestedField(u.Object, map[string]any{
		"phase":        phase,
		"sandboxName":  name + "-sb",
		"secretKeyRef": map[string]any{"name": name + "-key"},
		"conditions": []any{
			map[string]any{"type": "ACPReady", "status": ready},
		},
	}, "status")
	for _, opt := range opts {
		opt(u)
	}
	return u
}

func cstate(s string) core.ContainerState {
	switch s {
	case "running":
		return core.ContainerState{Running: &core.ContainerStateRunning{}}
	case "terminated":
		return core.ContainerState{Terminated: &core.ContainerStateTerminated{}}
	default:
		return core.ContainerState{Waiting: &core.ContainerStateWaiting{}}
	}
}

func pod(name, agent, skillLoader string) *core.Pod {
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: name, Namespace: testNS},
		Status: core.PodStatus{
			InitContainerStatuses: []core.ContainerStatus{{Name: "skill-loader", State: cstate(skillLoader)}},
			ContainerStatuses:     []core.ContainerStatus{{Name: "agent", State: cstate(agent)}},
		},
	}
}

type call struct {
	uid, container, url, key string
	live                     func() bool
	ctx                      context.Context
}

// recorder captures stub invocations.
type stubs struct {
	mu          sync.Mutex
	transcripts []call
	logs        []call
	// block, when true, keeps captures running until ctx is done.
	blockTranscript bool
	blockLog        bool
	panicLog        bool
}

func (s *stubs) transcriptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.transcripts)
}

func (s *stubs) logCount(container string) (n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.logs {
		if c.container == container {
			n++
		}
	}
	return
}

func newManager(t *testing.T, objs ...client.Object) (*Manager, *stubs, context.Context) {
	t.Helper()
	Settings.Hub.Namespace = testNS
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
	return newManagerWith(t, c)
}

func newManagerWith(t *testing.T, c client.Client) (*Manager, *stubs, context.Context) {
	t.Helper()
	s := &stubs{}
	m := &Manager{
		Client: c,
		Store:  &Store{Root: t.TempDir()},
		Logs: func(context.Context, string, string, bool) (r io.ReadCloser, err error) {
			err = errors.New("not used")
			return
		},
		KeyFor: func(_ context.Context, name string) (string, error) { return "key-of-" + name, nil },
		URLFor: func(sandbox string) string { return "ws://" + sandbox + "/acp" },
	}
	m.runTranscript = func(ctx context.Context, tc *TranscriptCapture) {
		s.mu.Lock()
		s.transcripts = append(s.transcripts, call{uid: tc.UID, url: tc.URL, key: tc.Key, live: tc.Live, ctx: ctx})
		block := s.blockTranscript
		s.mu.Unlock()
		if block {
			<-ctx.Done()
		}
	}
	m.runLog = func(ctx context.Context, lc *LogCapture) error {
		s.mu.Lock()
		s.logs = append(s.logs, call{uid: lc.UID, container: lc.Container, ctx: ctx})
		block, panics := s.blockLog, s.panicLog
		s.mu.Unlock()
		if panics {
			panic("boom")
		}
		if block {
			<-ctx.Done()
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		m.wg.Wait()
	})
	return m, s, ctx
}

// waitIdle waits until no capture goroutine is running.
func waitIdle(t *testing.T, m *Manager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m.active() == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("captures still running")
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for " + what)
}

func TestStartsTranscriptWhenACPReady(t *testing.T) {
	m, s, ctx := newManager(t, agentRun("run-a", uid1, "Running", true))
	m.tick(ctx)
	waitFor(t, "transcript", func() bool { return s.transcriptCount() == 1 })
	s.mu.Lock()
	c := s.transcripts[0]
	s.mu.Unlock()
	if c.uid != uid1 || c.url != "ws://run-a-sb/acp" || c.key != "key-of-run-a-key" {
		t.Fatalf("unexpected capture: %+v", c)
	}
	if !c.live() {
		t.Fatal("expected live")
	}
}

func TestNoTranscriptWhenACPNotReady(t *testing.T) {
	m, s, ctx := newManager(t, agentRun("run-a", uid1, "Running", false))
	m.tick(ctx)
	waitIdle(t, m)
	if n := s.transcriptCount(); n != 0 {
		t.Fatalf("got %d transcripts", n)
	}
}

func TestSkipsApproveMode(t *testing.T) {
	m, s, ctx := newManager(t,
		agentRun("run-a", uid1, "Running", true, withMode("approve")),
		pod("run-a", "running", "terminated"))
	s.blockLog = true
	m.tick(ctx)
	m.tick(ctx)
	meta, found, err := m.Store.ReadMeta("run-a", uid1)
	if err != nil || !found {
		t.Fatalf("meta not written: %v", err)
	}
	if meta.Transcript != "skipped" || meta.SkipReason != "approve mode" {
		t.Fatalf("got %+v", meta)
	}
	waitFor(t, "logs", func() bool { return s.logCount("agent") == 1 && s.logCount("skill-loader") == 1 })
	if n := s.transcriptCount(); n != 0 {
		t.Fatalf("got %d transcripts", n)
	}
	if n := m.skipWrites(); n != 1 {
		t.Fatalf("skip meta written %d times", n)
	}
}

func TestOneCapturePerUID(t *testing.T) {
	m, s, ctx := newManager(t,
		agentRun("run-a", uid1, "Running", true),
		pod("run-a", "running", "waiting"))
	s.blockTranscript = true
	s.blockLog = true
	m.tick(ctx)
	m.tick(ctx)
	waitFor(t, "captures", func() bool { return s.transcriptCount() == 1 && s.logCount("agent") == 1 })
	m.tick(ctx)
	if s.transcriptCount() != 1 || s.logCount("agent") != 1 || s.logCount("skill-loader") != 0 {
		t.Fatalf("transcripts %d agent logs %d skill logs %d",
			s.transcriptCount(), s.logCount("agent"), s.logCount("skill-loader"))
	}
}

func TestNoTranscriptForTerminalRun(t *testing.T) {
	for _, phase := range []string{"Succeeded", "Failed"} {
		t.Run(phase, func(t *testing.T) {
			m, s, ctx := newManager(t, agentRun("run-a", uid1, phase, true))
			m.tick(ctx)
			waitIdle(t, m)
			if n := s.transcriptCount(); n != 0 {
				t.Fatalf("got %d transcripts", n)
			}
		})
	}
}

func TestLiveReflectsLatestPhase(t *testing.T) {
	run := agentRun("run-a", uid1, "Running", true)
	m, s, ctx := newManager(t, run)
	s.blockTranscript = true
	m.tick(ctx)
	waitFor(t, "transcript", func() bool { return s.transcriptCount() == 1 })
	s.mu.Lock()
	live := s.transcripts[0].live
	s.mu.Unlock()
	if !live() {
		t.Fatal("expected live")
	}
	_ = unstructured.SetNestedField(run.Object, "Succeeded", "status", "phase")
	if err := m.Client.Update(ctx, run); err != nil {
		t.Fatal(err)
	}
	m.tick(ctx)
	if live() {
		t.Fatal("expected not live after terminal phase")
	}
}

func TestStaleRecordingMarkedIncomplete(t *testing.T) {
	const uid3 = "33333333-3333-3333-3333-333333333333"
	m, _, ctx := newManager(t,
		agentRun("run-done", uid1, "Succeeded", false),
		agentRun("run-live", uid3, "Running", false))
	for _, r := range []struct{ name, uid string }{{"run-done", uid1}, {"run-gone", uid2}, {"run-live", uid3}} {
		if err := m.Store.WriteMeta(r.name, r.uid, Meta{Run: r.name, UID: r.uid, Transcript: "recording"}); err != nil {
			t.Fatal(err)
		}
	}
	m.tick(ctx)
	for _, r := range []struct{ name, uid string }{{"run-done", uid1}, {"run-gone", uid2}} {
		meta, _, _ := m.Store.ReadMeta(r.name, r.uid)
		if meta.Transcript != "incomplete" || meta.Ended == nil {
			t.Fatalf("%s: got %+v", r.name, meta)
		}
	}
	meta, _, _ := m.Store.ReadMeta("run-live", uid3)
	if meta.Transcript != "recording" {
		t.Fatalf("live run: got %+v", meta)
	}
}

func TestRestartsLogCaptureAfterStreamDrop(t *testing.T) {
	p := pod("run-a", "running", "terminated")
	m, s, ctx := newManager(t, agentRun("run-a", uid1, "Running", false), p)
	m.tick(ctx)
	waitIdle(t, m)
	m.tick(ctx)
	waitIdle(t, m)
	if n := s.logCount("agent"); n != 2 {
		t.Fatalf("agent captures: %d", n)
	}
	// container finished: one final capture to EOF, then no more.
	p.Status.ContainerStatuses[0].State = cstate("terminated")
	if err := m.Client.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		m.tick(ctx)
		waitIdle(t, m)
	}
	if n := s.logCount("agent"); n != 3 {
		t.Fatalf("agent captures: %d", n)
	}
	if n := s.logCount("skill-loader"); n != 1 {
		t.Fatalf("skill-loader captures: %d", n)
	}
}

func TestTranscriptRestartedOnlyWhileReady(t *testing.T) {
	run := agentRun("run-a", uid1, "Running", true)
	m, s, ctx := newManager(t, run)
	m.tick(ctx)
	waitIdle(t, m)
	m.tick(ctx)
	waitIdle(t, m)
	if n := s.transcriptCount(); n != 2 {
		t.Fatalf("transcripts: %d", n)
	}
	_ = unstructured.SetNestedField(run.Object, "Failed", "status", "phase")
	if err := m.Client.Update(ctx, run); err != nil {
		t.Fatal(err)
	}
	m.tick(ctx)
	waitIdle(t, m)
	if n := s.transcriptCount(); n != 2 {
		t.Fatalf("transcripts: %d", n)
	}
}

func TestCancelsCapturesWhenRunDeleted(t *testing.T) {
	run := agentRun("run-a", uid1, "Running", true)
	m, s, ctx := newManager(t, run, pod("run-a", "running", "waiting"))
	s.blockTranscript = true
	s.blockLog = true
	m.tick(ctx)
	waitFor(t, "captures", func() bool { return s.transcriptCount() == 1 && s.logCount("agent") == 1 })
	if err := m.Client.Delete(ctx, run); err != nil {
		t.Fatal(err)
	}
	m.tick(ctx)
	waitIdle(t, m)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.transcripts[0].ctx.Err() == nil || s.logs[0].ctx.Err() == nil {
		t.Fatal("captures not cancelled")
	}
}

func TestRecoversCapturePanic(t *testing.T) {
	m, s, ctx := newManager(t, agentRun("run-a", uid1, "Running", false), pod("run-a", "running", "waiting"))
	s.panicLog = true
	m.tick(ctx)
	waitIdle(t, m)
	m.tick(ctx)
	waitIdle(t, m)
	if n := s.logCount("agent"); n != 2 {
		t.Fatalf("agent captures: %d", n)
	}
}

func TestListErrorDoesNotStopRun(t *testing.T) {
	Settings.Hub.Namespace = testNS
	var mu sync.Mutex
	lists := 0
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, o ...client.ListOption) error {
			mu.Lock()
			lists++
			mu.Unlock()
			return errors.New("no AgentRun CRD")
		},
	}).Build()
	m, _, ctx := newManagerWith(t, c)
	m.Interval = time.Millisecond
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		m.Run(runCtx)
		close(done)
	}()
	waitFor(t, "repeated ticks", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return lists >= 3
	})
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}
