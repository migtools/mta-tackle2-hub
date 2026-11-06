package record

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jortel/go-utils/logr"
	"github.com/konveyor/tackle2-hub/shared/settings"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	Settings = &settings.Settings
	Log      = logr.WithName("agentic.recorder")
)

const (
	agentContainer       = "agent"
	skillLoaderContainer = "skill-loader"
	// finalAttempts bounds retries of a log capture started after the
	// container terminated (expected to read to EOF in one run).
	finalAttempts = 3
)

var (
	agentRunGVK     = schema.GroupVersionKind{Group: "konveyor.io", Version: "v1alpha1", Kind: "AgentRun"}
	agentRunListGVK = schema.GroupVersionKind{Group: "konveyor.io", Version: "v1alpha1", Kind: "AgentRunList"}
)

// Manager drives transcript and log capture for every AgentRun in the
// hub namespace.
type Manager struct {
	Client   client.Client
	Store    *Store
	Logs     LogOpener
	KeyFor   func(ctx context.Context, secretName string) (string, error)
	URLFor   func(sandbox string) string
	Interval time.Duration // default 2s

	// injectable for tests
	runTranscript func(ctx context.Context, c *TranscriptCapture)
	runLog        func(ctx context.Context, c *LogCapture) error

	mutex   sync.Mutex
	runs    map[string]*runState // keyed by uid
	swept   bool
	lastErr string
	skips   int // skipped-meta writes
	running int // capture goroutines
	wg      sync.WaitGroup
}

// runState is the per-uid capture state. Fields other than live are
// guarded by Manager.mutex.
type runState struct {
	name       string
	ctx        context.Context
	cancel     context.CancelFunc
	live       atomic.Bool
	transcript bool // capture running
	skipped    bool
	keyErr     string
	logs       map[string]*logState
}

type logState struct {
	running  bool
	final    bool // started after the container terminated
	attempts int  // final attempts
	done     bool
}

// Run reconciles captures every Interval until ctx is done, then waits
// for the capture goroutines to return.
func (m *Manager) Run(ctx context.Context) {
	interval := m.Interval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	defer m.wg.Wait()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		m.safeTick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) safeTick(ctx context.Context) {
	defer func() {
		if p := recover(); p != nil {
			Log.Error(fmt.Errorf("panic: %v", p), "recorder tick failed")
		}
	}()
	m.tick(ctx)
}

// tick lists AgentRuns and starts/cancels captures.
func (m *Manager) tick(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(agentRunListGVK)
	err := m.Client.List(ctx, list, client.InNamespace(Settings.Hub.Namespace))
	if err != nil {
		m.mutex.Lock()
		changed := err.Error() != m.lastErr
		m.lastErr = err.Error()
		m.mutex.Unlock()
		if changed {
			Log.Error(err, "recorder: list AgentRuns failed")
		}
		return
	}
	m.mutex.Lock()
	m.lastErr = ""
	if m.runs == nil {
		m.runs = make(map[string]*runState)
	}
	sweep := !m.swept
	m.swept = true
	m.mutex.Unlock()
	if sweep {
		m.sweep(list.Items)
	}
	seen := make(map[string]bool)
	for i := range list.Items {
		u := &list.Items[i]
		seen[string(u.GetUID())] = true
		m.reconcile(ctx, u)
	}
	m.mutex.Lock()
	for uid, st := range m.runs {
		if !seen[uid] {
			st.cancel()
			delete(m.runs, uid)
		}
	}
	m.mutex.Unlock()
}

// sweep marks recordings left "recording" by a previous hub as
// incomplete when their run is terminal or gone.
func (m *Manager) sweep(items []unstructured.Unstructured) {
	terminal := make(map[string]bool)
	for i := range items {
		terminal[string(items[i].GetUID())] = isTerminal(&items[i])
	}
	ents, err := os.ReadDir(m.Store.Root)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			Log.Error(err, "recorder: read store failed")
		}
		return
	}
	for _, e := range ents {
		n := e.Name()
		if !e.IsDir() || len(n) < 38 || n[len(n)-37] != '-' {
			continue
		}
		name, uid := n[:len(n)-37], n[len(n)-36:]
		if !uuidRe.MatchString(uid) {
			continue
		}
		meta, found, err := m.Store.ReadMeta(name, uid)
		if err != nil || !found || meta.Transcript != "recording" {
			continue
		}
		term, exists := terminal[uid]
		if exists && !term {
			continue
		}
		meta.Transcript = "incomplete"
		if meta.Ended == nil {
			now := time.Now().UTC()
			meta.Ended = &now
		}
		err = m.Store.WriteMeta(name, uid, meta)
		if err != nil {
			Log.Error(err, "recorder: mark incomplete failed", "run", name)
		}
	}
}

// reconcile starts the captures due for one run.
func (m *Manager) reconcile(ctx context.Context, u *unstructured.Unstructured) {
	name, uid := u.GetName(), string(u.GetUID())
	if validate(name, uid) != nil || !uuidRe.MatchString(uid) {
		return
	}
	terminal := isTerminal(u)
	m.mutex.Lock()
	st, found := m.runs[uid]
	if !found {
		st = &runState{name: name, logs: make(map[string]*logState)}
		st.ctx, st.cancel = context.WithCancel(ctx)
		m.runs[uid] = st
	}
	st.live.Store(!terminal)
	m.mutex.Unlock()
	allowed, reason := TranscriptAllowed(u)
	if !allowed {
		m.skip(st, uid, reason)
	} else if !terminal && acpReady(u) {
		m.startTranscript(ctx, st, uid, u)
	}
	if m.Logs == nil && m.runLog == nil {
		return
	}
	pod := &core.Pod{}
	err := m.Client.Get(ctx, client.ObjectKey{Namespace: Settings.Hub.Namespace, Name: name}, pod)
	if err != nil {
		return
	}
	agent := containerState(pod.Status.ContainerStatuses, agentContainer)
	agentStarted := agent.Running != nil || agent.Terminated != nil
	m.startLog(st, uid, agentContainer, agentStarted, agent.Terminated != nil)
	loader := containerState(pod.Status.InitContainerStatuses, skillLoaderContainer)
	m.startLog(st, uid, skillLoaderContainer, loader.Terminated != nil, loader.Terminated != nil)
}

// skip records (once) that the transcript is not captured.
func (m *Manager) skip(st *runState, uid, reason string) {
	m.mutex.Lock()
	done := st.skipped
	m.mutex.Unlock()
	if done {
		return
	}
	meta, _, err := m.Store.ReadMeta(st.name, uid)
	if err == nil && !(meta.Transcript == "skipped" && meta.SkipReason == reason) {
		meta.Run, meta.UID = st.name, uid
		meta.Transcript = "skipped"
		meta.SkipReason = reason
		err = m.Store.WriteMeta(st.name, uid, meta)
		if err == nil {
			m.mutex.Lock()
			m.skips++
			m.mutex.Unlock()
			Log.Info("recorder skipped transcript", "run", st.name, "reason", reason)
		}
	}
	if err != nil {
		Log.Error(err, "recorder: write skipped meta failed", "run", st.name)
		return
	}
	m.mutex.Lock()
	st.skipped = true
	m.mutex.Unlock()
}

// startTranscript starts the transcript capture unless one is running.
func (m *Manager) startTranscript(ctx context.Context, st *runState, uid string, u *unstructured.Unstructured) {
	m.mutex.Lock()
	running := st.transcript
	m.mutex.Unlock()
	if running {
		return
	}
	sandbox, _, _ := unstructured.NestedString(u.Object, "status", "sandboxName")
	secret, _, _ := unstructured.NestedString(u.Object, "status", "secretKeyRef", "name")
	if sandbox == "" || secret == "" {
		return
	}
	key, err := m.KeyFor(ctx, secret)
	if err != nil {
		m.mutex.Lock()
		changed := err.Error() != st.keyErr
		st.keyErr = err.Error()
		m.mutex.Unlock()
		if changed {
			Log.Error(err, "recorder: ACP key lookup failed", "run", st.name)
		}
		return
	}
	c := &TranscriptCapture{
		Store: m.Store,
		Name:  st.name,
		UID:   uid,
		URL:   m.URLFor(sandbox),
		Key:   key,
		Live:  st.live.Load,
	}
	run := m.runTranscript
	if run == nil {
		run = func(ctx context.Context, c *TranscriptCapture) { c.Run(ctx) }
	}
	m.mutex.Lock()
	st.keyErr = ""
	st.transcript = true
	m.mutex.Unlock()
	m.spawn(st.name, "transcript", func() {
		run(st.ctx, c)
	}, func(bool) {
		st.transcript = false
	})
}

// startLog starts a container log capture when due.
// A capture that ends while the container still runs (stream drop) is
// restarted on a later tick; once the container has terminated, one more
// capture reads the remainder to EOF.
func (m *Manager) startLog(st *runState, uid, container string, started, terminated bool) {
	if !started {
		return
	}
	m.mutex.Lock()
	ls, found := st.logs[container]
	if !found {
		ls = &logState{}
		st.logs[container] = ls
	}
	if ls.running || ls.done {
		m.mutex.Unlock()
		return
	}
	ls.running = true
	ls.final = terminated
	m.mutex.Unlock()
	c := &LogCapture{
		Store:     m.Store,
		Name:      st.name,
		UID:       uid,
		Container: container,
		Open:      m.Logs,
	}
	run := m.runLog
	if run == nil {
		run = func(ctx context.Context, c *LogCapture) error { return c.Run(ctx) }
	}
	var err error
	m.spawn(st.name, container+" log", func() {
		err = run(st.ctx, c)
		if err != nil {
			Log.Error(err, "recorder: log capture failed", "run", st.name, "container", container)
		}
	}, func(ok bool) {
		ls.running = false
		if ls.final {
			ls.attempts++
			if (ok && err == nil) || ls.attempts >= finalAttempts {
				ls.done = true
			}
		}
	})
}

// spawn runs f in a goroutine, recovering panics. exited is called with
// the manager mutex held; ok is false when f panicked.
func (m *Manager) spawn(run, what string, f func(), exited func(ok bool)) {
	m.mutex.Lock()
	m.running++
	m.mutex.Unlock()
	m.wg.Add(1)
	go func() {
		ok := false
		defer func() {
			if p := recover(); p != nil {
				Log.Error(fmt.Errorf("panic: %v", p), "recorder: capture panicked", "run", run, "capture", what)
			}
			m.mutex.Lock()
			exited(ok)
			m.running--
			m.mutex.Unlock()
			m.wg.Done()
		}()
		f()
		ok = true
	}()
}

// active returns the number of running capture goroutines.
func (m *Manager) active() int {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.running
}

// skipWrites returns the number of skipped-meta writes.
func (m *Manager) skipWrites() int {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.skips
}

// isTerminal reports whether the run has finished: a terminal phase or
// a decided Succeeded condition.
func isTerminal(u *unstructured.Unstructured) bool {
	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	if phase == "Succeeded" || phase == "Failed" {
		return true
	}
	status := condition(u, "Succeeded")
	return status == "True" || status == "False"
}

func acpReady(u *unstructured.Unstructured) bool {
	return condition(u, "ACPReady") == "True"
}

// condition returns the status of the named status condition.
func condition(u *unstructured.Unstructured, kind string) string {
	conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conditions {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := cm["type"].(string); t == kind {
			s, _ := cm["status"].(string)
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func containerState(statuses []core.ContainerStatus, name string) core.ContainerState {
	for _, s := range statuses {
		if s.Name == name {
			return s.State
		}
	}
	return core.ContainerState{}
}
