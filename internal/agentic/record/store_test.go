package record

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	uid1 = "11111111-1111-1111-1111-111111111111"
	uid2 = "22222222-2222-2222-2222-222222222222"
	uid3 = "33333333-3333-3333-3333-333333333333"
)

func TestMetaRoundTrip(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	if _, ok, err := s.ReadMeta("run-a", uid1); ok || err != nil {
		t.Fatalf("expected missing, got ok=%v err=%v", ok, err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	in := Meta{
		Run: "run-a", UID: uid1, Transcript: "incomplete", SkipReason: "x",
		Gaps:    []Gap{{From: now, To: now.Add(time.Second)}},
		Started: &now,
	}
	if err := s.WriteMeta("run-a", uid1, in); err != nil {
		t.Fatal(err)
	}
	out, ok, err := s.ReadMeta("run-a", uid1)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if out.Run != "run-a" || out.UID != uid1 || out.Transcript != "incomplete" ||
		out.SkipReason != "x" || len(out.Gaps) != 1 || !out.Gaps[0].To.Equal(now.Add(time.Second)) ||
		out.Started == nil || !out.Started.Equal(now) || out.Ended != nil {
		t.Fatalf("mismatch: %+v", out)
	}
	ents, _ := os.ReadDir(s.Dir("run-a", uid1))
	if len(ents) != 1 {
		t.Fatalf("temp file left behind: %v", ents)
	}
}

func TestOpenAppendReturnsSize(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	f, size, err := s.OpenAppend("run-a", uid1, "transcript.jsonl")
	if err != nil || size != 0 {
		t.Fatalf("size=%d err=%v", size, err)
	}
	if _, err = f.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	f, size, err = s.OpenAppend("run-a", uid1, "transcript.jsonl")
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
	d1 := filepath.Join(s.Root, "run-a-"+uid1)
	d2 := filepath.Join(s.Root, "run-a-"+uid2)
	d3 := filepath.Join(s.Root, "run-ab-"+uid3)
	for _, d := range []string{d1, d2, d3} {
		_ = os.MkdirAll(d, 0o755)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(d1, old, old)
	_ = os.Chtimes(d2, time.Now().Add(-time.Minute), time.Now().Add(-time.Minute))
	_ = os.Chtimes(d3, time.Now(), time.Now())
	d4 := filepath.Join(s.Root, "run-a-b-"+uid3) // decoy: newer, other run name
	_ = os.MkdirAll(d4, 0o755)
	d5 := filepath.Join(s.Root, "run-a-notauuid") // decoy: bad uid shape
	_ = os.MkdirAll(d5, 0o755)
	dir, uid, found := s.Find("run-a")
	if !found || uid != uid2 || dir != d2 {
		t.Fatalf("dir=%s uid=%s found=%v", dir, uid, found)
	}
}

func TestRejectUnsafeNames(t *testing.T) {
	parent := t.TempDir()
	s := &Store{Root: filepath.Join(parent, "agentruns")}
	for _, name := range []string{"../x", "", "a/b", `a\b`, "a..b"} {
		if _, _, err := s.OpenAppend(name, uid1, "a.log"); err == nil {
			t.Errorf("OpenAppend(%q) no error", name)
		}
		if err := s.WriteMeta(name, uid1, Meta{}); err == nil {
			t.Errorf("WriteMeta(%q) no error", name)
		}
		if _, _, err := s.ReadMeta(name, uid1); err == nil {
			t.Errorf("ReadMeta(%q) no error", name)
		}
		if _, _, found := s.Find(name); found {
			t.Errorf("Find(%q) found", name)
		}
	}
	if _, _, err := s.OpenAppend("run-a", "../x", "a.log"); err == nil {
		t.Error("bad uid accepted")
	}
	if _, _, err := s.OpenAppend("run-a", uid1, "../a.log"); err == nil {
		t.Error("bad file accepted")
	}
	ents, _ := os.ReadDir(parent)
	if len(ents) != 0 {
		t.Errorf("created outside/inside root: %v", ents)
	}
}
