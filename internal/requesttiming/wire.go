package requesttiming

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
)

const MaxWireBytes = 4096

type wire struct {
	Version    int                  `json:"v"`
	Service    Service              `json:"s"`
	Count      int                  `json:"n"`
	Incomplete bool                 `json:"i"`
	Cells      []Cell               `json:"c"`
	Revisions  [serviceCount]string `json:"r"`
	Configs    [serviceCount]Config `json:"l"`
	Terminal   bool                 `json:"end"`
}

func Encode(s Summary, service Service) string {
	w := wire{1, service, s.Count, s.Incomplete, s.Cells[:s.Count], s.Revisions, s.Configs, true}
	for {
		data, _ := json.Marshal(w)
		if len(data) <= MaxWireBytes {
			return string(data)
		}
		w.Incomplete = true
		w.Cells = w.Cells[:len(w.Cells)-1]
		w.Count = len(w.Cells)
	}
}
func Decode(raw string, emitter Service) (Summary, error) {
	bad := errors.New("invalid request timing summary")
	if len(raw) == 0 || len(raw) > MaxWireBytes {
		return Summary{}, bad
	}
	var w wire
	d := json.NewDecoder(bytes.NewBufferString(raw))
	d.DisallowUnknownFields()
	if d.Decode(&w) != nil {
		return Summary{}, bad
	}
	if d.Decode(new(any)) != io.EOF || w.Version != 1 || w.Service != emitter || !w.Terminal || w.Count != len(w.Cells) || w.Count > MaxCells {
		return Summary{}, bad
	}
	canonical, _ := json.Marshal(w)
	if string(canonical) != raw {
		return Summary{}, bad
	}
	s := Summary{Incomplete: w.Incomplete, Revisions: w.Revisions, Configs: w.Configs}
	seen := map[key]bool{}
	for _, c := range w.Cells {
		k := key{c.Service, c.Stage, c.Method, c.Outcome}
		if c.Service >= serviceCount || c.Stage >= stageCount || c.Method >= methodCount || c.Outcome >= outcomeCount || c.Count == 0 || c.Count > 1_000_000 || c.Micros < 0 || c.Micros > 86_400_000_000 || seen[k] {
			return Summary{}, bad
		}
		seen[k] = true
		if c.Outcome == Pending && !w.Incomplete {
			return Summary{}, bad
		}
		add(&s, c)
	}
	for _, rev := range w.Revisions {
		if rev != "" && rev != "unknown" && rev != "multiple" && safeRevision(rev) != rev {
			return Summary{}, bad
		}
	}
	for _, c := range w.Configs {
		l := c.Limits
		if c.Present && (math.IsNaN(l.PairRPS) || math.IsInf(l.PairRPS, 0) || l.PairRPS < .1 || l.PairRPS > 50 || math.IsNaN(l.AccountRPS) || math.IsInf(l.AccountRPS, 0) || l.AccountRPS < l.PairRPS || l.AccountRPS > 50 || l.PairBurst < 1 || l.PairBurst > 100 || l.AccountBurst < 1 || l.AccountBurst > 100 || l.MaxWaitMS < 100 || l.MaxWaitMS > 60000 || l.PairWaiters < 1 || l.AccountWaiters < l.PairWaiters || l.ProcessWaiters < l.AccountWaiters || l.ProcessWaiters > 1_000_000 || l.Replicas != 1) {
			return Summary{}, bad
		}
	}
	return s, nil
}
