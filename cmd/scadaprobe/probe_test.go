package main

// The pure parts of the diagnosis: the latency arithmetic behind the
// recommended polling budget, and the rendering helpers.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dernate/energontrol/v2"
)

func TestSummariseAndPercentiles(t *testing.T) {
	var shots []time.Duration
	for i := 1; i <= 100; i++ {
		shots = append(shots, time.Duration(i)*time.Millisecond)
	}
	s := summarise(shots)
	if s.min != time.Millisecond || s.max != 100*time.Millisecond {
		t.Errorf("min/max = %s/%s, want 1ms/100ms", s.min, s.max)
	}
	if s.p50 != 50*time.Millisecond || s.p95 != 95*time.Millisecond {
		t.Errorf("p50/p95 = %s/%s, want 50ms/95ms", s.p50, s.p95)
	}
	// An empty sample set must not index off the end.
	if got := summarise(nil); got.max != 0 {
		t.Errorf("summarise(nil) = %+v", got)
	}
	if got := percentileIndex(0, 95); got != 0 {
		t.Errorf("percentileIndex(0, 95) = %d", got)
	}
	if got := percentileIndex(1, 95); got != 0 {
		t.Errorf("percentileIndex(1, 95) = %d, want 0", got)
	}
}

// The recommendation exists because the polling budget is a wall-clock deadline
// that has to hold several round trips.
func TestRecommendedPollTimeout(t *testing.T) {
	if got := recommendedPollTimeout(5 * time.Millisecond); got != minRecommendedPollTimeout {
		t.Errorf("fast server: %s, want the floor %s", got, minRecommendedPollTimeout)
	}
	if got := recommendedPollTimeout(400 * time.Millisecond); got < 3*time.Second {
		t.Errorf("slow server: %s, want room for %d round trips", got, pollAttemptsWanted)
	}
	// The figure the report compares against has to be the library's real
	// default, or the advice would be wrong.
	if defaultPollTimeoutForReport != 5*time.Second {
		t.Errorf("defaultPollTimeoutForReport = %s; keep it in step with the library",
			defaultPollTimeoutForReport)
	}
}

func TestUnlistedPlantAndNotListed(t *testing.T) {
	if p, ok := unlistedPlant([]uint8{2, 5}); !ok || p == 2 || p == 5 {
		t.Errorf("unlistedPlant = %d, %t; want a number the park does not use", p, ok)
	}
	if got := notListed([]uint8{2, 7}, []uint8{2, 5}); len(got) != 1 || got[0] != 7 {
		t.Errorf("notListed = %v, want [7]", got)
	}
	if got := notListed([]uint8{2}, []uint8{2, 5}); len(got) != 0 {
		t.Errorf("notListed = %v, want none", got)
	}
}

// A state that could not be read must never render as a plausible state: Ctrl 0
// means "running", which is the last thing an unknown state should look like.
func TestStateCellsSayWhenTheyDoNotKnow(t *testing.T) {
	boom := errors.New("bad quality")
	ctrl := energontrol.PlantStates{
		{PlantNo: 2, Ctrl: energontrol.CtrlStop90},
		{PlantNo: 5, Err: boom},
	}
	if got := stateCell(ctrl, 2); got != "Stop90" {
		t.Errorf("plant 2 = %q, want Stop90", got)
	}
	if got := stateCell(ctrl, 5); !strings.HasPrefix(got, "unknown") {
		t.Errorf("plant 5 = %q, want it to say the state is unknown", got)
	}
	if got := stateCell(ctrl, 9); got != "(not read)" {
		t.Errorf("plant 9 = %q, want (not read)", got)
	}

	rbh := energontrol.RbhStates{{PlantNo: 2, Status: energontrol.RbhInstalled}, {PlantNo: 5, Err: boom}}
	if got := rbhCell(rbh, 5); !strings.HasPrefix(got, "unknown") {
		t.Errorf("heating of plant 5 = %q", got)
	}
	if got := rbhCell(rbh, 2); !strings.Contains(got, "installed") {
		t.Errorf("heating of plant 2 = %q", got)
	}
	ice := energontrol.IceDetStates{{PlantNo: 2}, {PlantNo: 5, Err: boom}}
	if got := iceCell(ice, 5); !strings.HasPrefix(got, "unknown") {
		t.Errorf("ice detection of plant 5 = %q", got)
	}
	if got := iceCell(ice, 2); !strings.Contains(got, "No ice") {
		t.Errorf("ice detection of plant 2 = %q", got)
	}
}

// A joined error has to stay on one line of a table.
func TestFirstLine(t *testing.T) {
	if got := firstLine(nil); got != "" {
		t.Errorf("firstLine(nil) = %q", got)
	}
	joined := errors.Join(errors.New("first"), errors.New("second"))
	got := firstLine(joined)
	if strings.Contains(got, "\n") {
		t.Errorf("firstLine kept a newline: %q", got)
	}
	if !strings.HasPrefix(got, "first") || !strings.HasSuffix(got, "…") {
		t.Errorf("firstLine = %q, want the first line and an ellipsis", got)
	}
}

// The trace in the log has to distinguish a value from a fault from something
// that could not be decoded.
func TestItemTraceDistinguishesTheThreeOutcomes(t *testing.T) {
	got := itemTrace([]energontrol.ItemResult{
		{Name: "a", Values: []uint64{2}, Quality: "good"},
		{Name: "b", ResultID: "E_UNKNOWNITEMNAME"},
		{Name: "c", Err: errors.New("not a number")},
	})
	if len(got) != 3 {
		t.Fatalf("trace = %v", got)
	}
	if !strings.Contains(got[0], "[2]") || !strings.Contains(got[0], "good") {
		t.Errorf("value trace = %q", got[0])
	}
	if !strings.Contains(got[1], "fault") {
		t.Errorf("fault trace = %q", got[1])
	}
	if !strings.Contains(got[2], "undecodable") {
		t.Errorf("undecodable trace = %q", got[2])
	}
}

func TestWrapIndentKeepsWordsWhole(t *testing.T) {
	got := wrapIndent("the quick brown fox jumps over the lazy dog", 4, 24)
	for _, line := range strings.Split(got, "\n") {
		if len(line) > 24 {
			t.Errorf("line longer than the limit: %q", line)
		}
	}
	if strings.Join(strings.Fields(got), " ") != "the quick brown fox jumps over the lazy dog" {
		t.Errorf("wrapping changed the words: %q", got)
	}
}
