package record

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMetaRoundTrip(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	if _, ok, err := s.ReadMeta("run-a", "u1"); ok || err != nil {
		t.Fatalf("expected missing, got ok=%v err=%v", ok, err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	in := Meta{
		Run: "run-a", UID: "u1", Transcript: "incomplete", SkipReason: "x",
		Gaps:    []Gap{{From: now, To: now.Add(time.Second)}},
		Started: &now,
	}
	if err := s.WriteMeta("run-a", "u1", in); err != nil {
		t.Fatal(err)
	}
	out, ok, err := s.ReadMeta("run-a", "u1")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if out.Run != "run-a" || out.UID != "u1" || out.Transcript != "incomplete" ||
		out.SkipReason != "x" || len(out.Gaps) != 1 || !out.Gaps[0].To.Equal(now.Add(time.Second)) ||
		out.Started == nil || !out.Started.Equal(now) || out.Ended != nil {
		t.Fatalf("mismatch: %+v", out)
	}
	ents, _ := os.ReadDir(s.Dir("run-a", "u1"))
	if len(ents) != 1 {
		t.Fatalf("temp file left behind: %v", ents)
	}
}

func TestOpenAppendReturnsSize(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	f, size, err := s.OpenAppend("run-a", "u1", "transcript.jsonl")
	if err != nil || size != 0 {
		t.Fatalf("size=%d err=%v", size, err)
	}
	if _, err = f.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	f, size, err = s.OpenAppend("run-a", "u1", "transcript.jsonl")
	if err != nil || size != 10 {
		t.Fatalf("size=%d err=%v", size, err)
	}
	_ = f.Close()
}

func TestFindNewest(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	if _, _, found := s.Find("run-a"); found {
		t.Fatal("unexpected found")
	}
	d1 := filepath.Join(s.Root, "run-a-uid1")
	d2 := filepath.Join(s.Root, "run-a-uid2")
	d3 := filepath.Join(s.Root, "run-ab-uid3")
	for _, d := range []string{d1, d2, d3} {
		_ = os.MkdirAll(d, 0o755)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(d1, old, old)
	_ = os.Chtimes(d2, time.Now().Add(-time.Minute), time.Now().Add(-time.Minute))
	_ = os.Chtimes(d3, time.Now(), time.Now())
	dir, uid, found := s.Find("run-a")
	if !found || uid != "uid2" || dir != d2 {
		t.Fatalf("dir=%s uid=%s found=%v", dir, uid, found)
	}
}
