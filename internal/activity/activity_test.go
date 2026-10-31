package activity

import (
	"os"
	"strings"
	"sync"
	"testing"
)

func newLog(t *testing.T) *Log {
	t.Helper()
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func appendN(t *testing.T, l *Log, n int) []Event {
	t.Helper()
	var out []Event
	for i := 0; i < n; i++ {
		e, err := l.Append(Event{Action: "exec", Host: "web1", Target: "dev@10.0.0.1", Detail: "uptime"})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func TestAppendBuildsAChain(t *testing.T) {
	l := newLog(t)
	got := appendN(t, l, 3)

	if got[0].Seq != 1 || got[0].PrevHash != "" {
		t.Errorf("first event: seq %d prev %q, want seq 1 and no prev", got[0].Seq, got[0].PrevHash)
	}
	for i := 1; i < len(got); i++ {
		if got[i].PrevHash != got[i-1].Hash {
			t.Errorf("event %d does not point at event %d", i+1, i)
		}
		if got[i].Device != got[0].Device {
			t.Errorf("device ID changed within one log")
		}
	}
	all, err := l.All()
	if err != nil {
		t.Fatal(err)
	}
	if broken := Verify(all); broken != nil {
		t.Fatalf("fresh chain should verify: %v", broken)
	}
}

func TestVerifyCatchesAnEditedEvent(t *testing.T) {
	l := newLog(t)
	appendN(t, l, 3)

	data, err := os.ReadFile(l.eventsPath())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	lines[1] = strings.Replace(lines[1], `"web1"`, `"web2"`, 1) // rewrite which host event 2 touched
	if err := os.WriteFile(l.eventsPath(), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	all, err := l.All()
	if err != nil {
		t.Fatal(err)
	}
	broken := Verify(all)
	if broken == nil || broken.Line != 2 {
		t.Fatalf("Verify = %v, want a break at line 2", broken)
	}
}

func TestVerifyCatchesADeletedEvent(t *testing.T) {
	l := newLog(t)
	appendN(t, l, 3)

	all, err := l.All()
	if err != nil {
		t.Fatal(err)
	}
	withoutMiddle := []Event{all[0], all[2]}
	broken := Verify(withoutMiddle)
	if broken == nil || broken.Line != 2 {
		t.Fatalf("Verify = %v, want a break at line 2 after removing an event", broken)
	}
}

func TestPendingAndMarkUploaded(t *testing.T) {
	l := newLog(t)
	appendN(t, l, 3)

	if p, _ := l.Pending("t1"); len(p) != 3 {
		t.Fatalf("pending = %d, want 3 before any upload", len(p))
	}
	if err := l.MarkUploaded("t1", 2); err != nil {
		t.Fatal(err)
	}
	p, err := l.Pending("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 1 || p[0].Seq != 3 {
		t.Fatalf("pending after uploading seq 2 = %+v, want only seq 3", p)
	}
	if err := l.MarkUploaded("t1", 1); err != nil { // must not move backwards
		t.Fatal(err)
	}
	if p, _ := l.Pending("t1"); len(p) != 1 {
		t.Errorf("MarkUploaded moved the cursor backwards: pending = %d", len(p))
	}
}

func TestCursorsArePerTeam(t *testing.T) {
	l := newLog(t)
	appendN(t, l, 2)
	if err := l.MarkUploaded("team-a", 2); err != nil {
		t.Fatal(err)
	}
	if p, _ := l.Pending("team-b"); len(p) != 2 {
		t.Errorf("team-b pending = %d, want 2 (team-a's upload must not count for it)", len(p))
	}
}

func TestVerifySharedAllowsGapsButNotEdits(t *testing.T) {
	l := newLog(t)
	all := appendN(t, l, 4)
	shared := []Event{all[3], all[0], all[1]} // seq 3 belongs to another team; any order
	if broken := VerifyShared(shared); broken != nil {
		t.Fatalf("a gap should be fine: %v", broken)
	}
	shared[2].Detail = "something else"
	if broken := VerifyShared(shared); broken == nil {
		t.Error("an edited event must be caught")
	}
}

func TestConcurrentAppendsKeepTheChain(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := Open(dir) // separate handles, like separate meterm processes
			if err != nil {
				t.Error(err)
				return
			}
			for j := 0; j < 5; j++ {
				if _, err := l.Append(Event{Action: "connect", Host: "web1"}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()

	l, _ := Open(dir)
	all, err := l.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 40 {
		t.Fatalf("got %d events, want 40", len(all))
	}
	if broken := Verify(all); broken != nil {
		t.Fatalf("concurrent appends broke the chain: %v", broken)
	}
}

func TestHashIsFieldBoundarySafe(t *testing.T) {
	a := Event{Host: "ab", Detail: "c"}
	b := Event{Host: "a", Detail: "bc"}
	if a.computeHash() == b.computeHash() {
		t.Error("shifting text between fields must change the hash")
	}
}

// A Git team's ID is a URL; its upload cursor must still be a single file.
func TestMarkUploadedWithAURLKey(t *testing.T) {
	l, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "git:https://github.com/acme/team"
	if err := l.MarkUploaded(key, 5); err != nil {
		t.Fatalf("MarkUploaded: %v", err)
	}
	if got, err := l.uploadedSeq(key); err != nil || got != 5 {
		t.Errorf("uploadedSeq = %d, %v", got, err)
	}
	if got, _ := l.uploadedSeq("277db75afca5c39505b9"); got != 0 {
		t.Errorf("another key's cursor = %d", got)
	}
	if fileKey("277db75afca5c39505b9") != "277db75afca5c39505b9" {
		t.Error("cloud IDs must keep their existing file names")
	}
}
