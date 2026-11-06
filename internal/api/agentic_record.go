package api

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/konveyor/tackle2-hub/internal/agentic/record"
	core "k8s.io/api/core/v1"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8s "sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	// AgentRunStore serves recorded agent run transcripts and logs.
	// Defaults to <bucket>/agentruns when nil.
	AgentRunStore *record.Store
	// AgentRunLogOpener opens live pod logs. Nil disables live logs.
	AgentRunLogOpener record.LogOpener
)

// containerRe validates a container name before it is used in a path.
var containerRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// recRunGVK is the AgentRun CR GVK.
var recRunGVK = schema.GroupVersionKind{
	Group: "konveyor.io", Version: "v1alpha1", Kind: "AgentRun",
}

// followPoll is how often a saved-file follow stream checks for growth.
const followPoll = 500 * time.Millisecond

// followIdle ends a saved-file follow stream once the run is terminal or
// gone and the file has not grown for this long.
const followIdle = 5 * time.Second

// recorderStore returns the store serving recordings.
func recorderStore() (s *record.Store) {
	if AgentRunStore != nil {
		return AgentRunStore
	}
	return &record.Store{
		Root: filepath.Join(Settings.Hub.Bucket.Path, "agentruns"),
	}
}

// AgentRunLogs godoc
// @summary Stream the pod log of an agent run.
// @description Streams the agent run's container log from the live pod
// @description when one exists, else the recording saved by the hub's
// @description run recorder. Use offset (bytes) to resume a cut stream.
// @tags runs
// @produce plain
// @success 200 {string} string
// @router /agentic/agentruns/{name}/logs [get]
// @param name path string true "AgentRun name"
// @param container query string false "Container name (agent)"
// @param follow query bool false "Keep streaming new output"
// @param offset query int false "Byte offset to resume from"
func (h AgenticHandler) AgentRunLogs(ctx *gin.Context) {
	name := ctx.Param(Name)
	container := ctx.DefaultQuery("container", "agent")
	follow := ctx.Query("follow") == "true"
	offset, err := strconv.ParseInt(ctx.DefaultQuery("offset", "0"), 10, 64)
	if err != nil || offset < 0 {
		ctx.Status(http.StatusBadRequest)
		return
	}
	if !containerRe.MatchString(container) {
		ctx.Status(http.StatusBadRequest)
		return
	}
	run := h.recRunGet(ctx, name)
	terminal := run == nil || recTerminal(run)
	_, podFound := h.recPodGet(ctx, name)
	if podFound && AgentRunLogOpener != nil {
		reader, err := AgentRunLogOpener(ctx.Request.Context(), name, container, follow)
		if err == nil {
			defer reader.Close()
			// Live and saved logs agree: skip the requested bytes so a
			// resumed stream continues where the cut one ended.
			if offset > 0 {
				_, err = io.CopyN(io.Discard, reader, offset)
			}
			if err == nil {
				h.streamText(ctx, reader, "live")
				return
			}
		}
	}
	// Saved file.
	dir, _, found := h.recDir(ctx, run, name)
	if !found {
		ctx.Status(http.StatusNotFound)
		return
	}
	file := filepath.Join(dir, container+".log")
	st, err := os.Stat(file)
	if err != nil {
		ctx.Status(http.StatusNotFound)
		return
	}
	f, err := os.Open(file)
	if err != nil {
		ctx.Status(http.StatusNotFound)
		return
	}
	defer f.Close()
	if st.Size() > offset {
		if _, err = f.Seek(offset, io.SeekStart); err != nil {
			ctx.Status(http.StatusInternalServerError)
			return
		}
	} else {
		if _, err = f.Seek(0, io.SeekEnd); err != nil {
			ctx.Status(http.StatusInternalServerError)
			return
		}
	}
	if !follow {
		h.streamText(ctx, f, "saved")
		return
	}
	// Follow the saved file while the recorder may still append to it.
	live := run != nil && !terminal
	h.streamSavedFollow(ctx, f, file, live, "saved")
}

// AgentRunTranscript godoc
// @summary Stream the recorded transcript of an agent run.
// @description Returns the run's recorded ACP transcript as NDJSON,
// @description one frame per line. Use offset (lines) to resume a cut
// @description stream. Follow streams new lines while the recorder runs.
// @tags runs
// @produce json
// @success 200 {string} string
// @router /agentic/agentruns/{name}/transcript [get]
// @param name path string true "AgentRun name"
// @param follow query bool false "Keep streaming new lines"
// @param offset query int false "Line offset to resume from"
func (h AgenticHandler) AgentRunTranscript(ctx *gin.Context) {
	name := ctx.Param(Name)
	follow := ctx.Query("follow") == "true"
	offset, err := strconv.Atoi(ctx.DefaultQuery("offset", "0"))
	if err != nil || offset < 0 {
		ctx.Status(http.StatusBadRequest)
		return
	}
	run := h.recRunGet(ctx, name)
	dir, uid, found := h.recDir(ctx, run, name)
	if !found {
		ctx.Header("X-Transcript-Status", "none")
		ctx.Status(http.StatusNotFound)
		return
	}
	meta, _, err := recorderStore().ReadMeta(name, uid)
	if err != nil {
		ctx.Status(http.StatusInternalServerError)
		return
	}
	ctx.Header("X-Transcript-Status", meta.Transcript)
	if meta.SkipReason != "" {
		ctx.Header("X-Transcript-Skip-Reason", meta.SkipReason)
	}
	if meta.Transcript == "skipped" {
		ctx.Status(http.StatusOK)
		return
	}
	file := filepath.Join(dir, "transcript.jsonl")
	f, err := os.Open(file)
	if err != nil {
		// A dir with meta but no transcript file yet.
		if meta.Transcript == "recording" && follow {
			h.streamTranscriptFollow(ctx, f, name, uid, run, offset)
			return
		}
		ctx.Header("X-Transcript-Status", "none")
		ctx.Status(http.StatusNotFound)
		return
	}
	defer f.Close()
	if !follow {
		h.streamTranscriptFrom(ctx, f, offset)
		return
	}
	h.streamTranscriptFollow(ctx, f, name, uid, run, offset)
}

// streamText writes reader to the client as text/plain, flushing each
// chunk, and reports the source in X-Log-Source.
func (h AgenticHandler) streamText(ctx *gin.Context, reader io.Reader, source string) {
	ctx.Header("X-Log-Source", source)
	ctx.Header("Content-Type", "text/plain; charset=utf-8")
	ctx.Status(http.StatusOK)
	buf := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			if _, werr := ctx.Writer.Write(buf[:n]); werr != nil {
				return
			}
			ctx.Writer.Flush()
		}
		if err != nil {
			return
		}
		if ctx.Request.Context().Err() != nil {
			return
		}
	}
}

// streamSavedFollow tails a saved log file, ending once the run is
// terminal or gone and the file has stopped growing.
func (h AgenticHandler) streamSavedFollow(ctx *gin.Context, f *os.File, file string, live bool, source string) {
	ctx.Header("X-Log-Source", source)
	ctx.Header("Content-Type", "text/plain; charset=utf-8")
	ctx.Status(http.StatusOK)
	buf := make([]byte, 32*1024)
	var idle time.Duration
	for {
		if ctx.Request.Context().Err() != nil {
			return
		}
		n, err := f.Read(buf)
		if n > 0 {
			idle = 0
			if _, werr := ctx.Writer.Write(buf[:n]); werr != nil {
				return
			}
			ctx.Writer.Flush()
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return
		}
		if !live {
			// Terminal run: the file is final.
			return
		}
		idle += followPoll
		if idle >= followIdle {
			return
		}
		st, serr := os.Stat(file)
		if serr == nil && st.Size() > sizeOf(f) {
			// The recorder appended; re-read from the current position.
			idle = 0
			continue
		}
		time.Sleep(followPoll)
	}
}

// streamTranscriptFrom writes complete transcript lines from the given
// line offset.
func (h AgenticHandler) streamTranscriptFrom(ctx *gin.Context, f *os.File, offset int) {
	ctx.Header("Content-Type", "application/x-ndjson")
	ctx.Status(http.StatusOK)
	h.copyLinesFrom(ctx, f, offset)
}

// streamTranscriptFollow streams transcript lines from offset, then keeps
// streaming new lines while the recording is in progress.
func (h AgenticHandler) streamTranscriptFollow(ctx *gin.Context, f *os.File, name, uid string, run *unstructured.Unstructured, offset int) {
	ctx.Header("Content-Type", "application/x-ndjson")
	ctx.Status(http.StatusOK)
	h.copyLinesFrom(ctx, f, offset)
	var idle time.Duration
	for {
		if ctx.Request.Context().Err() != nil {
			return
		}
		meta, found, err := recorderStore().ReadMeta(name, uid)
		if err != nil || (found && meta.Transcript != "recording") {
			return
		}
		terminal := run == nil || recTerminal(run)
		if terminal {
			idle += followPoll
			if idle >= followIdle {
				return
			}
		} else {
			idle = 0
		}
		h.copyLinesFrom(ctx, f, 0)
		time.Sleep(followPoll)
	}
}

// copyLinesFrom writes whole lines starting at the given line offset
// (0-based), leaving a partial trailing line for the next call.
func (h AgenticHandler) copyLinesFrom(ctx *gin.Context, f *os.File, offset int) {
	if f == nil {
		return
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return
	}
	// Only complete lines: a partial trailing line is left unread.
	complete := data
	if i := bytesLastIndexByte(data, '\n'); i >= 0 {
		complete = data[:i+1]
		// Rewind the file to the start of the partial line so a later
		// follow pass re-reads it.
		_, _ = f.Seek(int64(i+1), io.SeekStart)
	}
	lines := strings.Split(string(complete), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if offset > len(lines) {
		return
	}
	for _, line := range lines[offset:] {
		if _, err := ctx.Writer.WriteString(line + "\n"); err != nil {
			return
		}
	}
	ctx.Writer.Flush()
}

// recRunGet fetches the AgentRun CR; nil when gone.
func (h AgenticHandler) recRunGet(ctx *gin.Context, name string) (run *unstructured.Unstructured) {
	r := &unstructured.Unstructured{}
	r.SetGroupVersionKind(recRunGVK)
	err := h.Client(ctx).Get(
		ctx.Request.Context(),
		k8s.ObjectKey{Namespace: Settings.Hub.Namespace, Name: name},
		r)
	if err != nil {
		return nil
	}
	return r
}

// recPodGet reports whether the run's pod exists.
func (h AgenticHandler) recPodGet(ctx *gin.Context, name string) (pod *core.Pod, found bool) {
	p := &core.Pod{}
	err := h.Client(ctx).Get(
		ctx.Request.Context(),
		k8s.ObjectKey{Namespace: Settings.Hub.Namespace, Name: name},
		p)
	if err != nil {
		if !k8serr.IsNotFound(err) {
			_ = ctx.Error(err)
		}
		return nil, false
	}
	return p, true
}

// recDir resolves the recording directory for a run: the CR's uid when
// the CR exists, else the newest matching directory in the store.
func (h AgenticHandler) recDir(ctx *gin.Context, run *unstructured.Unstructured, name string) (dir, uid string, found bool) {
	store := recorderStore()
	if run != nil {
		uid = string(run.GetUID())
		dir = store.Dir(name, uid)
		if _, err := os.Stat(dir); err == nil {
			return dir, uid, true
		}
	}
	dir, uid, found = store.Find(name)
	return
}

// recTerminal reports whether a run has finished.
func recTerminal(u *unstructured.Unstructured) bool {
	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	if phase == "Succeeded" || phase == "Failed" {
		return true
	}
	conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conditions {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := cm["type"].(string); t == "Succeeded" {
			s, _ := cm["status"].(string)
			return strings.TrimSpace(s) == "True" || strings.TrimSpace(s) == "False"
		}
	}
	return false
}

// sizeOf returns the current file position.
func sizeOf(f *os.File) int64 {
	pos, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0
	}
	return pos
}

// bytesLastIndexByte is bytes.LastIndexByte without importing bytes for
// one call.
func bytesLastIndexByte(b []byte, c byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == c {
			return i
		}
	}
	return -1
}
