package agent

import (
	"encoding/json"
	"fmt"
)

// guard stops runaway loops that a single iteration cap misses: the model
// repeating the exact same call, or round after round where every tool fails.
type guard struct {
	repeatLimit   int
	failRoundCap  int
	counts        map[string]int
	failingRounds int
}

func newGuard(repeatLimit, failRoundCap int) *guard {
	return &guard{repeatLimit: repeatLimit, failRoundCap: failRoundCap, counts: map[string]int{}}
}

// seen records a call and returns an error once the identical call (same
// tool, same arguments after normalising JSON) has reached the limit.
func (g *guard) seen(name string, input json.RawMessage) error {
	key := name + " " + canonicalJSON(input)
	g.counts[key]++
	if n := g.counts[key]; g.repeatLimit > 0 && n >= g.repeatLimit {
		return fmt.Errorf("the identical call to %s was made %d times", name, n)
	}
	return nil
}

// round records the outcome of one batch of tool calls.
func (g *guard) round(calls, failures int) error {
	if calls > 0 && failures == calls {
		g.failingRounds++
	} else {
		g.failingRounds = 0
	}
	if g.failRoundCap > 0 && g.failingRounds >= g.failRoundCap {
		return fmt.Errorf("every tool call failed for %d rounds in a row", g.failingRounds)
	}
	return nil
}

func canonicalJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, _ := json.Marshal(v) // map keys come out sorted
	return string(b)
}
