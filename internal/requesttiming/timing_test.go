package requesttiming

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestSnapshotSealsPendingOnce(t *testing.T) {
	ctx := Start(context.Background(), uuid.New(), Core, Issue)
	finish := Measure(ctx, EmployeeVerify)
	before := Snapshot(ctx)
	finish(nil)
	after := Snapshot(ctx)
	if before != after || !before.Incomplete || before.Count != 1 || before.Cells[0].Outcome != Pending || before.Cells[0].Count != 1 {
		t.Fatalf("pending snapshot changed or was counted twice: %+v %+v", before, after)
	}
	if RequestID(ctx) == uuid.Nil {
		t.Fatal("missing request ID")
	}
}
func TestConcurrentCountsAndIdempotentFinish(t *testing.T) {
	ctx := Start(context.Background(), uuid.New(), Gateway, OwnTasks)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); end := Measure(ctx, SQL); end(nil); end(context.Canceled) }()
	}
	wg.Wait()
	s := Snapshot(ctx)
	if s.Incomplete || s.Count != 1 || s.Cells[0].Count != 100 || s.Cells[0].Outcome != OK || s.Cells[0].Method != OwnTasks {
		t.Fatalf("wrong aggregate: %+v", s)
	}
}
func TestWireRejectsInvalidAndTruncatedSummaries(t *testing.T) {
	ctx := Start(context.Background(), uuid.New(), Gateway, OwnTask)
	end := Measure(ctx, SQL)
	end(nil)
	raw := Encode(Snapshot(ctx), Gateway)
	if _, err := Decode(raw, Gateway); err != nil {
		t.Fatal(err)
	}
	var original wire
	if err := json.Unmarshal([]byte(raw), &original); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{"empty": "", "oversize": strings.Repeat("x", MaxWireBytes+1), "truncated": raw[:len(raw)-1], "duplicate_field": strings.Replace(raw, `"v":1`, `"v":1,"v":1`, 1)}
	mutate := func(name string, f func(*wire)) {
		w := original
		w.Cells = append([]Cell(nil), original.Cells...)
		f(&w)
		b, _ := json.Marshal(w)
		cases[name] = string(b)
	}
	mutate("version", func(w *wire) { w.Version = 2 })
	mutate("count", func(w *wire) { w.Count++ })
	mutate("terminal", func(w *wire) { w.Terminal = false })
	mutate("emitter", func(w *wire) { w.Service = Core })
	mutate("negative", func(w *wire) { w.Cells[0].Micros = -1 })
	mutate("enum", func(w *wire) { w.Cells[0].Stage = 255 })
	mutate("duplicates", func(w *wire) { w.Cells = append(w.Cells, w.Cells[0]); w.Count++ })
	mutate("revision", func(w *wire) { w.Revisions[Core] = "https://secret.example/token" })
	mutate("pending_complete", func(w *wire) { w.Cells[0].Outcome = Pending })
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(raw, Gateway); err == nil {
				t.Fatal("accepted malformed summary")
			}
		})
	}
}
func TestHeaderBoundsIncludeControlsAndTerminal(t *testing.T) {
	var s Summary
	for service := Core; service < serviceCount; service++ {
		for stage := Handler; stage < stageCount; stage++ {
			add(&s, Cell{service, stage, OwnTasks, OK, 1, 1000})
		}
	}
	s.Revisions[Core] = "unknown"
	h := s.ServerTiming()
	if len(h) > MaxHeaderBytes || !strings.HasPrefix(h, `diag;desc="v=1;`) || !strings.Contains(h, "truncated=true") || !strings.Contains(h, "complete=false") || !strings.Contains(h, ", diag_end;") {
		t.Fatalf("unbounded or ambiguous header (%d): %s", len(h), h)
	}
	parts := strings.Split(h, ", ")
	n := len(parts) - 2
	if !strings.Contains(parts[0], "n="+fmtInt(n)+";") || parts[len(parts)-1] != `diag_end;desc="n=`+fmtInt(n)+`"` {
		t.Fatal("entry count omitted controls or terminal")
	}
}
func fmtInt(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestWireEncodingCapPreservesTerminalAndExplicitIncomplete(t *testing.T) {
	var s Summary
	for service := Core; service < serviceCount; service++ {
		for stage := Handler; stage < stageCount; stage++ {
			add(&s, Cell{service, stage, OwnTasks, OK, 100000, 80000000000})
		}
	}
	for i := range s.Revisions {
		s.Revisions[i] = strings.Repeat("a", 64) + "-dirty-12345678"
		s.Configs[i] = Config{Present: true, Limits: Limits{PairRPS: 7, AccountRPS: 50, PairBurst: 1, AccountBurst: 1, MaxWaitMS: 5000, PairWaiters: 64, AccountWaiters: 512, ProcessWaiters: 4096, Replicas: 1}}
	}
	for _, method := range []Method{Issue, Validate, OwnTask, OwnLead} {
		add(&s, Cell{Core, SQL, method, OK, 1_000_000, 86_400_000_000})
	}
	uncapped, err := json.Marshal(wire{1, Gateway, s.Count, s.Incomplete, s.Cells[:s.Count], s.Revisions, s.Configs, true})
	if err != nil || len(uncapped) <= MaxWireBytes {
		t.Fatalf("fixture did not exceed wire cap: bytes=%d err=%v", len(uncapped), err)
	}
	raw := Encode(s, Gateway)
	got, err := Decode(raw, Gateway)
	if len(raw) > MaxWireBytes || err != nil || !got.Incomplete || got.Count >= s.Count {
		t.Fatalf("wire truncation ambiguous: bytes=%d count=%d err=%v", len(raw), got.Count, err)
	}
}
