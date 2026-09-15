package energontrol_test

// The package seen from outside, as a consumer sees it.
//
// Every other test file is white-box: the tracker, correlate and ctrlSatisfied
// have to be driven from within, and a Transport built on the in-memory fake is
// the shortest way to drive the session machine. But that leaves nobody
// checking that the exported surface is usable on its own — that a caller can
// implement Transport without reaching for an unexported type, classify every
// failure through the sentinels, and read a result without guessing.
//
// It is deliberately not a second copy of the protocol tests. It asks one
// question: is what this package exports enough to use it?

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dernate/energontrol/v2"
)

// scriptedTransport is a Transport written the way a caller would have to write
// one — from the exported types alone. It answers reads from a table of item
// names and records what it was asked.
type scriptedTransport struct {
	mu sync.Mutex

	state  string
	values map[string][]uint64
	faults map[string]string
	nodes  map[string][]energontrol.Node

	reads  []string
	writes []energontrol.ItemWrite
}

func newScriptedTransport() *scriptedTransport {
	return &scriptedTransport{
		state:  "running",
		values: map[string][]uint64{},
		faults: map[string]string{},
		nodes:  map[string][]energontrol.Node{},
	}
}

func (s *scriptedTransport) Status(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, nil
}

func (s *scriptedTransport) Read(_ context.Context, names []string,
	_ energontrol.ReadOptions) (energontrol.Response, error) {

	s.mu.Lock()
	defer s.mu.Unlock()
	resp := energontrol.Response{ServerState: s.state}
	for _, name := range names {
		s.reads = append(s.reads, name)
		r := energontrol.ItemResult{Name: name, Quality: "good", Timestamp: time.Now()}
		if rid, ok := s.faults[name]; ok {
			r.ResultID = rid
			resp.Items = append(resp.Items, r)
			continue
		}
		v, ok := s.values[name]
		if !ok {
			continue // the server has no such item
		}
		r.Values = v
		resp.Items = append(resp.Items, r)
	}
	return resp, nil
}

func (s *scriptedTransport) Write(_ context.Context, items []energontrol.ItemWrite) (energontrol.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := energontrol.Response{ServerState: s.state}
	for _, it := range items {
		s.writes = append(s.writes, it)
		resp.Items = append(resp.Items, energontrol.ItemResult{Name: it.Name})
	}
	return resp, nil
}

func (s *scriptedTransport) Browse(_ context.Context, path string,
	_ energontrol.BrowseFilter) ([]energontrol.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nodes[path], nil
}

// Compile-time proof that a Transport can be written from the exported API
// alone. If this stops compiling, the port has grown a dependency on something
// a caller cannot reach.
var _ energontrol.Transport = (*scriptedTransport)(nil)

func (s *scriptedTransport) set(name string, values ...uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[name] = values
}

func (s *scriptedTransport) wroteTo(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.writes {
		if w.Name == name {
			return true
		}
	}
	return false
}

// A stopped plant read from outside the package, with the state decoded through
// the exported constants.
func TestExternalReadingAPlantState(t *testing.T) {
	tr := newScriptedTransport()
	tr.set("Loc/Wec/Plant2/Ctrl/Ctrl", uint64(energontrol.CtrlStop90))
	tr.set("Loc/Wec/Plant4/Ctrl/Ctrl", uint64(energontrol.CtrlCommError))

	states, err := energontrol.New(tr).PlantCtrlState(context.Background(), 2, 4)
	if err != nil {
		t.Fatalf("PlantCtrlState: %v", err)
	}
	got, ok := states.Get(2)
	if !ok {
		t.Fatal("plant 2 missing from the states")
	}
	if !got.Ctrl.Stopped() {
		t.Errorf("plant 2: %s is not reported as stopped", got.Ctrl)
	}
	// A plant in CtrlCommError has an unknown state, and the exported
	// predicates have to say so from out here too.
	four, _ := states.Get(4)
	if four.Ctrl.Stopped() || four.Ctrl.Commandable() {
		t.Errorf("plant 4: %s must be neither stopped nor commandable", four.Ctrl)
	}
	if states.Err() != nil {
		t.Errorf("Err() = %v, want nil: both states were readable", states.Err())
	}
}

// The safety contract as a caller has to apply it: InRequestedState, not
// Err == nil.
func TestExternalSafetyContractIsUsableFromOutside(t *testing.T) {
	tr := newScriptedTransport()
	// Plant 2 is running and can be stopped; plant 4 was stopped by Enercon
	// with higher rights and cannot be commanded.
	tr.set("Loc/Wec/Plant2/Ctrl/Ctrl", uint64(energontrol.CtrlStart))
	tr.set("Loc/Wec/Plant4/Ctrl/Ctrl", uint64(energontrol.CtrlStopEnercon))
	for _, plant := range []string{"2"} {
		base := "Loc/Wec/Plant" + plant + "/Ctrl/"
		tr.set(base+"SessionState", 0)
		tr.set(base+"SessionPubKey", 4711)
		tr.set(base+"SessionRequest", 0)
		tr.set(base+"SetCtrl", uint64(energontrol.CtrlStop90))
	}

	res, err := energontrol.New(tr,
		energontrol.WithSessionPolling(time.Millisecond, 5*time.Millisecond)).
		Stop(context.Background(), 1234, true, true, 2, 4)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("results = %v, want one per plant", res)
	}
	// Plant 4 is not permitted, and the reason classifies through the sentinel.
	if res[1].InRequestedState() {
		t.Error("a plant under Enercon control was reported as stopped")
	}
	if !errors.Is(res[1].Err, energontrol.ErrPlantUnderEnerconControl) {
		t.Errorf("plant 4: err = %v, want ErrPlantUnderEnerconControl", res[1].Err)
	}
	if res[1].Outcome != energontrol.OutcomeNotPermitted {
		t.Errorf("plant 4: outcome = %s, want %s",
			res[1].Outcome, energontrol.OutcomeNotPermitted)
	}
	// Plant 2 was permitted and attempted, but this transport never advances
	// the session, so it does not reach the requested state either — and must
	// not claim to.
	if res[0].InRequestedState() {
		t.Error("a session that never advanced was reported as stopped")
	}
	// The batch-level helpers agree with the per-plant ones.
	if res.InRequestedState() {
		t.Error("Results.InRequestedState() must be false while one plant failed")
	}
	if res.Err() == nil {
		t.Error("Results.Err() must report the failed plant")
	}
	// Nothing was written for the plant that could not be commanded.
	if tr.wroteTo("Loc/Wec/Plant4/Ctrl/SetCtrl") {
		t.Error("a command was written for a plant under Enercon control")
	}
}

// An argument error is reported before anything reaches the server, and it
// classifies.
func TestExternalArgumentErrorsClassify(t *testing.T) {
	c := energontrol.New(newScriptedTransport())
	ctx := context.Background()

	for name, tc := range map[string]struct {
		call func() error
		want error
	}{
		"no plants": {
			func() error { _, err := c.Start(ctx, 1); return err },
			energontrol.ErrNoPlants,
		},
		"duplicate plant": {
			func() error { _, err := c.Start(ctx, 1, 2, 2); return err },
			energontrol.ErrDuplicatePlant,
		},
		"user id out of range": {
			func() error { _, err := c.Start(ctx, 1<<40, 2); return err },
			energontrol.ErrInvalidUserID,
		},
		"a value a client may not write": {
			func() error { _, err := c.SetCtrl(ctx, 1, energontrol.CtrlStopEnercon, false, 2); return err },
			energontrol.ErrInvalidValue,
		},
		"nothing requested": {
			func() error {
				_, err := c.ControlAndRbh(ctx, 1, energontrol.ControlAndRbhValue{}, 2)
				return err
			},
			energontrol.ErrNothingRequested,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want it to wrap %v", err, tc.want)
			}
		})
	}
}

// The option surface is usable, and an unusable value comes back as an error
// rather than as a client running on defaults nobody chose.
func TestExternalOptionsAreUsable(t *testing.T) {
	tr := newScriptedTransport()
	c, err := energontrol.NewWithOptions(tr,
		energontrol.WithSessionPolling(10*time.Millisecond, time.Second),
		energontrol.WithSessionLifetime(30*time.Second),
		energontrol.WithCommandTimeout(20*time.Second),
		energontrol.WithMaxStateAge(time.Minute),
		energontrol.WithItemRoot("Park"),
		energontrol.WithLenientWriteConfirmation(),
		energontrol.WithLenientSessionVerification(),
		energontrol.WithSessionRelease(func(context.Context, energontrol.Transport, uint8,
			energontrol.SessionKind, uint16, uint64) error {
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	// The configured root reaches the item names, which is observable from out
	// here only through what the transport was asked for.
	tr.set("Park/LocNo", 4242)
	if _, err := c.ParkNo(context.Background()); err != nil {
		t.Fatalf("ParkNo: %v", err)
	}
	tr.mu.Lock()
	reads := strings.Join(tr.reads, " ")
	tr.mu.Unlock()
	if !strings.Contains(reads, "Park/LocNo") {
		t.Errorf("reads = %q, want the configured root", reads)
	}

	if _, err := energontrol.NewWithOptions(tr, energontrol.WithSessionLifetime(0)); !errors.Is(
		err, energontrol.ErrInvalidOption) {
		t.Errorf("err = %v, want ErrInvalidOption", err)
	}
	if _, err := energontrol.NewWithOptions(nil); err == nil {
		t.Error("a nil Transport was accepted")
	}
}

// A park listing, including the report of a node the package cannot address.
func TestExternalParkListing(t *testing.T) {
	tr := newScriptedTransport()
	tr.set("Loc/LocNo", 4242)
	tr.nodes["Loc/Wec"] = []energontrol.Node{
		{Name: "Plant2", ItemName: "Loc/Wec/Plant2", HasChildren: true},
		{Name: "Plant007", ItemName: "Loc/Wec/Plant007", HasChildren: true},
	}
	tr.nodes["Loc/Wec/Plant2"] = []energontrol.Node{{Name: "Ctrl", HasChildren: true}}
	tr.nodes["Loc/Wec/Plant2/Ctrl"] = []energontrol.Node{{Name: "SetCtrl", IsItem: true}}

	info, err := energontrol.New(tr).Turbines(context.Background())
	if err != nil {
		t.Fatalf("Turbines: %v", err)
	}
	if info.ParkNo != 4242 {
		t.Errorf("ParkNo = %d, want 4242", info.ParkNo)
	}
	if len(info.PlantNo) != 1 || info.PlantNo[0] != 2 {
		t.Fatalf("PlantNo = %v, want [2]", info.PlantNo)
	}
	if !info.Ctrl[2] {
		t.Error("plant 2: Ctrl not reported")
	}
	if len(info.Unsupported) != 1 {
		t.Errorf("Unsupported = %v, want the node that cannot be addressed", info.Unsupported)
	}
}

// The status-word decoders are part of the API a monitoring loop uses.
func TestExternalStatusDecoding(t *testing.T) {
	heating := energontrol.RbhInstalled | energontrol.RbhManualOnSCADA | energontrol.RbhAutoOffWEA
	if !energontrol.RbhIsInstalled(heating) {
		t.Error("RbhIsInstalled said no for a word with the installed bit set")
	}
	if got := energontrol.RbhStatusString(heating); !strings.Contains(got, "SCADA") {
		t.Errorf("RbhStatusString = %q, want it to name the SCADA bits", got)
	}
	if got := energontrol.RbhStatusStrings(0); len(got) != 1 {
		t.Errorf("RbhStatusStrings(0) = %v, want the single not-installed line", got)
	}
	if got := energontrol.IceDetStatusString(energontrol.IceDetExternalSCADA); !strings.Contains(
		got, "SCADA") {
		t.Errorf("IceDetStatusString = %q, want it to name the SCADA signal", got)
	}
	if got := energontrol.IceDetStatusStrings(0); got[0] != "No ice detected" {
		t.Errorf("IceDetStatusStrings(0) = %v", got)
	}
}

// The package-level convenience wrappers take a Transport too.
func TestExternalPackageLevelWrappers(t *testing.T) {
	tr := newScriptedTransport()
	tr.set("Loc/Wec/Plant2/Ctrl/Ctrl", uint64(energontrol.CtrlStop90))

	if err := energontrol.ServerAvailable(context.Background(), tr); err != nil {
		t.Fatalf("ServerAvailable: %v", err)
	}
	states, err := energontrol.PlantCtrlState(context.Background(), tr, 2)
	if err != nil {
		t.Fatalf("PlantCtrlState: %v", err)
	}
	if states[0].Ctrl != energontrol.CtrlStop90 {
		t.Errorf("state = %s, want %s", states[0].Ctrl, energontrol.CtrlStop90)
	}
	// An already-stopped plant needs no session, so this must not touch one.
	res, err := energontrol.Stop(context.Background(), tr, 1234, true, false, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome != energontrol.OutcomeAlreadyInState {
		t.Errorf("outcome = %s, want %s", res[0].Outcome, energontrol.OutcomeAlreadyInState)
	}
}

// A server that is not running is an error, not a state, and it classifies.
func TestExternalServerStateIsHonoured(t *testing.T) {
	tr := newScriptedTransport()
	tr.state = "suspended"
	err := energontrol.ServerAvailable(context.Background(), tr)
	if !errors.Is(err, energontrol.ErrServerNotRunning) {
		t.Fatalf("err = %v, want ErrServerNotRunning", err)
	}
	if !strings.Contains(err.Error(), "suspended") {
		t.Errorf("err = %q, want it to name the observed state", err)
	}
}

// An item the server faults is a per-plant failure that classifies, and it does
// not blind the caller to the other plants.
func TestExternalItemFaultIsPerPlant(t *testing.T) {
	tr := newScriptedTransport()
	tr.set("Loc/Wec/Plant2/Ctrl/Ctrl", uint64(energontrol.CtrlStart))
	tr.faults["Loc/Wec/Plant4/Ctrl/Ctrl"] = "E_UNKNOWNITEMNAME"

	states, err := energontrol.New(tr).PlantCtrlState(context.Background(), 2, 4)
	if err != nil {
		t.Fatalf("PlantCtrlState: %v", err)
	}
	four, _ := states.Get(4)
	if !errors.Is(four.Err, energontrol.ErrItemFault) {
		t.Errorf("plant 4: err = %v, want ErrItemFault", four.Err)
	}
	two, _ := states.Get(2)
	if two.Err != nil {
		t.Errorf("plant 2: err = %v, want the readable plant unaffected", two.Err)
	}
	// And the all-or-nothing answer is available where a caller wants it.
	if states.Err() == nil {
		t.Error("PlantStates.Err() must report the unreadable plant")
	}
}

// An item the server does not answer for is missing, which is not a state.
func TestExternalMissingItemIsNotAState(t *testing.T) {
	tr := newScriptedTransport() // knows no items at all
	states, err := energontrol.New(tr).PlantCtrlState(context.Background(), 2)
	if err != nil {
		t.Fatalf("PlantCtrlState: %v", err)
	}
	if !errors.Is(states[0].Err, energontrol.ErrItemMissing) {
		t.Fatalf("err = %v, want ErrItemMissing", states[0].Err)
	}
	// Ctrl 0 is "running", so the caller must not be able to mistake the zero
	// value for a state — which is what the String form is for.
	if got := states[0].String(); !strings.Contains(got, "unknown") {
		t.Errorf("String() = %q, want it to say the state is unknown", got)
	}
}

// The exported value types reject what a client may not write, at compile time
// for a typo and at run time for a status value.
func TestExternalValueTypesAreClosed(t *testing.T) {
	for _, v := range []energontrol.CtrlValue{
		energontrol.CtrlStart, energontrol.CtrlStop60, energontrol.CtrlStop90,
		energontrol.CtrlGradientStop60, energontrol.CtrlGradientStop90,
		energontrol.CtrlStopIceDetection, energontrol.CtrlStopShadowFlicker,
		energontrol.CtrlStopSpeciesProtection60, energontrol.CtrlStopSpeciesProtection90,
	} {
		if !v.Writable() {
			t.Errorf("%s is documented as writable but Writable() says no", v)
		}
	}
	for _, v := range []energontrol.CtrlValue{
		energontrol.CtrlValueRejected, energontrol.CtrlStop60Enercon,
		energontrol.CtrlStopEnercon, energontrol.CtrlCommError,
	} {
		if v.Writable() {
			t.Errorf("%s is a reported status but Writable() says a client may send it", v)
		}
	}
	if !energontrol.RbhSetManualOn.Writable() || !energontrol.RbhSetPresetDuration.Writable() {
		t.Error("a documented heating value is not writable")
	}
	if energontrol.RbhValue(8).Writable() {
		t.Error("the bare value 8 has no constant and must not be writable")
	}
	if !energontrol.IceDetLampOn.Writable() || !energontrol.IceDetLampOff.Writable() {
		t.Error("a documented ice detection value is not writable")
	}
}

// A SessionStateError carries the plant and both states, and unwraps to the
// sentinel that names the cause.
func TestExternalSessionStateErrorIsInspectable(t *testing.T) {
	tr := newScriptedTransport()
	tr.set("Loc/Wec/Plant2/Ctrl/Ctrl", uint64(energontrol.CtrlStart))
	tr.set("Loc/Wec/Plant2/Ctrl/SessionState", uint64(energontrol.SessionOccupied))

	res, err := energontrol.New(tr,
		energontrol.WithSessionPolling(time.Millisecond, 5*time.Millisecond)).
		Stop(context.Background(), 1234, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !errors.Is(res[0].Err, energontrol.ErrSessionOccupied) {
		t.Fatalf("err = %v, want ErrSessionOccupied", res[0].Err)
	}
	var stateErr *energontrol.SessionStateError
	if !errors.As(res[0].Err, &stateErr) {
		t.Fatalf("err = %v, want a *SessionStateError", res[0].Err)
	}
	if stateErr.PlantNo != 2 {
		t.Errorf("PlantNo = %d, want 2", stateErr.PlantNo)
	}
	if stateErr.Got != energontrol.SessionOccupied {
		t.Errorf("Got = %s, want %s", stateErr.Got, energontrol.SessionOccupied)
	}
	// And an ItemError is inspectable the same way.
	tr2 := newScriptedTransport()
	tr2.faults["Loc/Wec/Plant2/Ctrl/Ctrl"] = "E_UNKNOWNITEMNAME"
	states, _ := energontrol.New(tr2).PlantCtrlState(context.Background(), 2)
	var itemErr *energontrol.ItemError
	if !errors.As(states[0].Err, &itemErr) {
		t.Fatalf("err = %v, want an *ItemError", states[0].Err)
	}
	if itemErr.ItemName != "Loc/Wec/Plant2/Ctrl/Ctrl" {
		t.Errorf("ItemName = %q, want the faulted item", itemErr.ItemName)
	}
}

// The example from the README, compiled: what a caller actually writes.
func ExampleClient_Stop() {
	tr := newScriptedTransport()
	tr.set("Loc/Wec/Plant2/Ctrl/Ctrl", uint64(energontrol.CtrlStop90))

	client := energontrol.New(tr)
	res, err := client.Stop(context.Background(), 1234, true, false, 2)
	if err != nil {
		fmt.Println("the command could not be attempted:", err)
		return
	}
	for _, r := range res {
		if r.InRequestedState() {
			fmt.Printf("plant %d: %s\n", r.PlantNo, r.Outcome)
		} else {
			fmt.Printf("plant %d is NOT stopped: %v\n", r.PlantNo, r.Err)
		}
	}
	// Output: plant 2: already in state
}
