package record

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeOpener(data string) LogOpener {
	return func(ctx context.Context, pod, container string, follow bool) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(data)), nil
	}
}

func TestLogCaptureCopies(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	c := &LogCapture{Store: s, Name: "run-a", UID: uid1, Container: "agent", Open: fakeOpener("a\nb\n")}
	if err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(s.Dir("run-a", uid1), "agent.log"))
	if err != nil || string(b) != "a\nb\n" {
		t.Fatalf("got %q err %v", b, err)
	}
}

func TestResumeSkipsSavedBytes(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	f, _, err := s.OpenAppend("run-a", uid1, "agent.log")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("a\n")
	_ = f.Close()
	c := &LogCapture{Store: s, Name: "run-a", UID: uid1, Container: "agent", Open: fakeOpener("a\nb\n")}
	if err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(s.Dir("run-a", uid1), "agent.log"))
	if string(b) != "a\nb\n" {
		t.Fatalf("got %q", b)
	}
}
