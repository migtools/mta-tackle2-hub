package record

import (
	"context"
	"errors"
	"io"

	core "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// LogOpener opens a pod container log stream.
type LogOpener func(ctx context.Context, pod, container string, follow bool) (io.ReadCloser, error)

// KubeLogOpener returns a LogOpener backed by the cluster API.
func KubeLogOpener(cs kubernetes.Interface, ns string) LogOpener {
	return func(ctx context.Context, pod, container string, follow bool) (io.ReadCloser, error) {
		options := &core.PodLogOptions{Container: container, Follow: follow}
		return cs.CoreV1().Pods(ns).GetLogs(pod, options).Stream(ctx)
	}
}

// LogCapture appends a container's log to <container>.log, resuming
// after bytes already saved. The pod is named Name.
type LogCapture struct {
	Store     *Store
	Name, UID string
	Container string
	Open      LogOpener
}

// Run copies the followed stream until EOF (container finished) or ctx is done.
// Returns nil on clean EOF or ctx cancellation.
func (c *LogCapture) Run(ctx context.Context) (err error) {
	f, size, err := c.Store.OpenAppend(c.Name, c.UID, c.Container+".log")
	if err != nil {
		return
	}
	defer func() {
		if cErr := f.Close(); err == nil {
			err = cErr
		}
	}()
	r, err := c.Open(ctx, c.Name, c.Container, true)
	if err != nil {
		if ctx.Err() != nil {
			err = nil
		}
		return
	}
	defer func() { _ = r.Close() }()
	// The stream restarts from the beginning: skip what is already saved.
	_, err = io.CopyN(io.Discard, r, size)
	if err == nil {
		_, err = io.Copy(f, r)
	} else if errors.Is(err, io.EOF) {
		err = nil
	}
	if err != nil && ctx.Err() != nil {
		err = nil
	}
	return
}
