package energontrol

// The OPC item layer: item names, batched reads and writes, and the checks that
// decide whether a value may be used at all — fault codes, quality, freshness.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A read that fails wholesale must not be turned into per-item results.
func TestReadFailureIsARequestLevelError(t *testing.T) {
	f := newFakeOPC()
	f.ReadErr = errTransport

	if _, err := New(f).PlantCtrlState(context.Background(), 2, 5); !errors.Is(err, errTransport) {
		t.Fatalf("err = %v, want it to wrap the transport error", err)
	}
	if _, err := New(f).PlantRbhState(context.Background(), 2); !errors.Is(err, errTransport) {
		t.Fatalf("PlantRbhState: err = %v, want it to wrap the transport error", err)
	}
	if _, err := New(f).ParkNo(context.Background()); !errors.Is(err, errTransport) {
		t.Fatalf("ParkNo: err = %v, want it to wrap the transport error", err)
	}
}

func TestQualityUsable(t *testing.T) {
	usable := []string{"", "good", "goodLocalOverride"}
	for _, q := range usable {
		if !qualityUsable(q) {
			t.Errorf("qualityUsable(%q) = false, want true", q)
		}
	}
	unusable := []string{"bad", "badConfigurationError", "uncertain", "uncertainLastUsableValue"}
	for _, q := range unusable {
		if qualityUsable(q) {
			t.Errorf("qualityUsable(%q) = true, want false", q)
		}
	}
}

func TestSessionCredentialsAreDistinct(t *testing.T) {
	seen := make(map[uint16]int)
	for i := 0; i < 200; i++ {
		cred, err := newSessionCred()
		if err != nil {
			t.Fatalf("newSessionCred: %v", err)
		}
		if cred.privateKey == 0 {
			t.Fatal("private key must not be zero")
		}
		if cred.sessionID >= 20 {
			t.Fatalf("session id %d out of range", cred.sessionID)
		}
		seen[cred.privateKey]++
	}
	// v1 reseeded math/rand from the clock on every call, which produced runs of
	// identical keys. Anything close to that shows up as a dominant value here.
	for key, n := range seen {
		if n > 10 {
			t.Errorf("private key %d drawn %d times out of 200", key, n)
		}
	}
}

func TestItemNamesMatchTheSCADAAddressSpace(t *testing.T) {
	cases := map[string]string{
		ctrlItem(7):                         "Loc/Wec/Plant7/Ctrl/Ctrl",
		rbhItem(7):                          "Loc/Wec/Plant7/Ctrl/Rbh",
		setCtrlItem(7):                      "Loc/Wec/Plant7/Ctrl/SetCtrl",
		setRbhItem(7):                       "Loc/Wec/Plant7/Ctrl/SetRbh",
		setResetItem(7):                     "Loc/Wec/Plant7/Reset/SetReset",
		sessionStateItem(7, SessionCtrl):    "Loc/Wec/Plant7/Ctrl/SessionState",
		sessionRequestItem(7, SessionReset): "Loc/Wec/Plant7/Reset/SessionRequest",
		sessionPubKeyItem(7, SessionCtrl):   "Loc/Wec/Plant7/Ctrl/SessionPubKey",
		sessionSubmitItem(7, SessionReset):  "Loc/Wec/Plant7/Reset/SessionSubmit",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("item name = %q, want %q", got, want)
		}
	}
}

// The ServerState every OPC XML-DA response carries has to be checked, not
// only the one GetStatus reports before the command starts. A server can
// degrade in the middle of a session.
func TestServerStateInAReadResponseStopsTheCommand(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.ResponseServerState = "failed"

	res, err := New(f).Stop(context.Background(), testUser, true, true, 2)
	if err == nil && res[0].Outcome == OutcomeCommanded {
		t.Fatal("a turbine was commanded while the server reported ServerState=failed")
	}
	if err != nil && !errors.Is(err, ErrServerNotRunning) {
		t.Errorf("err = %v, want it to wrap ErrServerNotRunning", err)
	}
	if err == nil && !errors.Is(res[0].Err, ErrServerNotRunning) {
		t.Errorf("err = %v, want it to wrap ErrServerNotRunning", res[0].Err)
	}
	if !strings.Contains(errOrResult(err, res), "failed") {
		t.Errorf("the observed state is not named: %v / %v", err, res)
	}
}

// A server that does not fill ServerState on a Read is not making a statement,
// so it must not be treated as broken.
func TestEmptyServerStateInAResponseIsTolerated(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.ResponseServerState = " " // whitespace only: no state reported

	if _, err := New(f).PlantCtrlState(context.Background(), 2); err != nil {
		t.Fatalf("PlantCtrlState: %v", err)
	}
}

// The check also has to cover a read that is not part of a command.
func TestServerStateIsCheckedOnAPlainRead(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStop90)
	f.ResponseServerState = "suspended"

	if _, err := New(f).PlantCtrlState(context.Background(), 2); !errors.Is(err, ErrServerNotRunning) {
		t.Fatalf("err = %v, want it to wrap ErrServerNotRunning", err)
	}
}

// WithMaxStateAge used to fail open. A server that returns no item
// timestamp made the check the caller explicitly asked for disappear.
func TestMaxStateAgeRejectsAnItemWithoutATimestamp(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.Timestamps = false // the server does not support ReturnItemTime

	c := New(f, WithMaxStateAge(time.Second))
	res, err := c.Start(context.Background(), testUser, 2)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res[0].Err == nil {
		t.Fatal("an item without a timestamp was accepted although a maximum age was required")
	}
	if !errors.Is(res[0].Err, ErrNoItemTime) {
		t.Errorf("err = %v, want it to wrap ErrNoItemTime", res[0].Err)
	}
	// A caller that classifies staleness must still catch it.
	if !errors.Is(res[0].Err, ErrStaleValue) {
		t.Errorf("err = %v, want it to wrap ErrStaleValue as well", res[0].Err)
	}
}

// Without the option nothing changes: no age requirement, no timestamp needed.
func TestMissingTimestampIsFineWithoutMaxStateAge(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.Timestamps = false

	res, err := New(f).Start(context.Background(), testUser, 2)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !res[0].InRequestedState() {
		t.Fatalf("result = %v, want already-in-state", res[0])
	}
}

// Every read carries the requirement, the session items included — a stale
// SessionState is the most dangerous of them all, and it is read in four of the
// ten steps of the procedure.
func TestMaxAgeAppliesToEveryReadOfACommand(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.Timestamps = true

	res, err := New(f, WithMaxStateAge(45*time.Second)).
		Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome != OutcomeCommanded {
		t.Fatalf("result = %v, want commanded", res[0])
	}
	if len(f.ReadMaxAge) < 4 {
		t.Fatalf("only %d reads in a whole command", len(f.ReadMaxAge))
	}
	for i, got := range f.ReadMaxAge {
		if got != 45*time.Second {
			t.Errorf("read %d asked for MaxAge %s, want 45s", i, got)
		}
	}
}

// And no read carries one when the option is off.
func TestNoMaxAgeOnAnyReadWithoutTheOption(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)

	if _, err := New(f).Stop(context.Background(), testUser, true, true, 2); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for i, got := range f.ReadMaxAge {
		if got != 0 {
			t.Errorf("read %d asked for MaxAge %s although the option is off", i, got)
		}
	}
}

// The value type of an OPC item depends on the xsi:type the server chose.
// v1 asserted it with .(uint64) and .(uint16) and panicked the calling process
// when a server disagreed.
func TestUndecodableValueIsAnErrorNotAPanic(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.ValueErr[ctrlItem(2)] = &ItemError{
		ItemName: ctrlItem(2), Reason: ErrUnexpectedType, Detail: "cannot use string as an unsigned integer",
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic instead of an error: %v", r)
		}
	}()
	res, err := New(f).Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].InRequestedState() {
		t.Fatal("an unusable value was accepted as the plant state")
	}
	if !errors.Is(res[0].Err, ErrUnexpectedType) {
		t.Errorf("err = %v, want it to wrap ErrUnexpectedType", res[0].Err)
	}
}

// The other half of the same rule: a scalar item that comes back with anything
// other than exactly one value is not a state either.
func TestScalarItemWithTheWrongArityIsRejected(t *testing.T) {
	for name, values := range map[string][]uint64{
		"no value":     {},
		"three values": {1, 2, 3},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeOPC()
			f.RawValues[ctrlItem(2)] = values

			res, err := New(f).Start(context.Background(), testUser, 2)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if !errors.Is(res[0].Err, ErrUnexpectedType) {
				t.Errorf("err = %v, want it to wrap ErrUnexpectedType", res[0].Err)
			}
		})
	}
}

// v1 ignored the quality field, so a value the server had explicitly marked
// as unusable was used as a process value.
func TestBadQualityIsRejected(t *testing.T) {
	for _, q := range []string{"bad", "uncertain", "badConfigurationError"} {
		t.Run(q, func(t *testing.T) {
			f := newFakeOPC()
			f.Ctrl[2] = uint64(CtrlStart)
			f.Quality[ctrlItem(2)] = q

			res, err := New(f).Start(context.Background(), testUser, 2)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if res[0].InRequestedState() {
				t.Fatalf("a value with quality %q was accepted as the plant state", q)
			}
			if !errors.Is(res[0].Err, ErrBadQuality) {
				t.Errorf("err = %v, want it to wrap ErrBadQuality", res[0].Err)
			}
		})
	}
	t.Run("goodLocalOverride is usable", func(t *testing.T) {
		f := newFakeOPC()
		f.Ctrl[2] = uint64(CtrlStart)
		f.Quality[ctrlItem(2)] = "goodLocalOverride"
		res, err := New(f).Start(context.Background(), testUser, 2)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if !res[0].InRequestedState() {
			t.Errorf("good quality rejected: %v", res[0])
		}
	})
}

// An item the server flags with a ResultID must not be read as a state.
func TestItemFaultIsRejected(t *testing.T) {
	f := newFakeOPC()
	f.ItemFault[ctrlItem(2)] = "E_UNKNOWN_ITEM_NAME"

	res, err := New(f).Start(context.Background(), testUser, 2)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !errors.Is(res[0].Err, ErrItemFault) {
		t.Fatalf("err = %v, want it to wrap ErrItemFault", res[0].Err)
	}
}

// A response that omits an item left v1 with the zero value — plant 0,
// CtrlState 0 — and CtrlState 0 means "running".
func TestMissingItemIsAnErrorNotARunningPlant(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStop90)
	f.Ctrl[5] = uint64(CtrlStop90)
	f.Omit[ctrlItem(5)] = true

	res, err := New(f).Start(context.Background(), testUser, 2, 5)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !errors.Is(res[1].Err, ErrItemMissing) {
		t.Fatalf("plant 5: err = %v, want it to wrap ErrItemMissing", res[1].Err)
	}
	if res[1].InRequestedState() {
		t.Error("a plant whose state was never returned was reported as running")
	}
	if res[0].Outcome != OutcomeCommanded {
		t.Errorf("plant 2 should still be commanded, got %v (%v)", res[0].Outcome, res[0].Err)
	}
}

// Nothing above the Transport may depend on the order a response arrives
// in. The wire-level guarantee — correlation by ClientItemHandle rather than by
// position — is the adapter's, and is tested in
// TestAdapterCorrelatesReorderedResponses.
func TestResponseOrderDoesNotMatter(t *testing.T) {
	f := newFakeOPC()
	f.Reverse = true
	f.Ctrl[2] = uint64(CtrlStart)  // running
	f.Ctrl[5] = uint64(CtrlStop90) // stopped
	f.Ctrl[7] = uint64(CtrlStop60) // stopped

	states, err := New(f).PlantCtrlState(context.Background(), 2, 5, 7)
	if err != nil {
		t.Fatalf("PlantCtrlState: %v", err)
	}
	want := map[uint8]CtrlValue{2: CtrlStart, 5: CtrlStop90, 7: CtrlStop60}
	for _, s := range states {
		if s.Ctrl != want[s.PlantNo] {
			t.Errorf("plant %d: state %s, want %s", s.PlantNo, s.Ctrl, want[s.PlantNo])
		}
	}
}

// A Transport is part of the public API, so a caller may supply one. This
// package checks the contract rather than trusting it: an item nobody asked
// for, or one answered for twice, would otherwise be attributed to whichever
// plant happened to match.
func TestTransportContractIsChecked(t *testing.T) {
	t.Run("an unrequested item", func(t *testing.T) {
		f := newFakeOPC()
		f.Ctrl[2] = uint64(CtrlStart)
		f.ExtraItem = ctrlItem(9)
		if _, err := New(f).PlantCtrlState(context.Background(), 2); !errors.Is(err, ErrUncorrelatable) {
			t.Fatalf("err = %v, want it to wrap ErrUncorrelatable", err)
		}
	})
	t.Run("a duplicated item", func(t *testing.T) {
		f := newFakeOPC()
		f.Ctrl[2] = uint64(CtrlStart)
		f.DuplicateItem = ctrlItem(2)
		if _, err := New(f).PlantCtrlState(context.Background(), 2); !errors.Is(err, ErrUncorrelatable) {
			t.Fatalf("err = %v, want it to wrap ErrUncorrelatable", err)
		}
	})
}

// A stale value must not drive a control decision once the check is enabled.
func TestStaleValueIsRejectedWhenEnabled(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.Timestamps = true

	c := New(f, WithMaxStateAge(time.Second))
	c.now = func() time.Time { return time.Now().Add(time.Hour) }

	res, err := c.Start(context.Background(), testUser, 2)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !errors.Is(res[0].Err, ErrStaleValue) {
		t.Fatalf("err = %v, want it to wrap ErrStaleValue", res[0].Err)
	}
}

// Abb. 34: the item names of the Ctrl node, verbatim.
func TestSpecCtrlNodeItemNames(t *testing.T) {
	want := []string{
		"Loc/Wec/Plant1/Ctrl/SessionState",
		"Loc/Wec/Plant1/Ctrl/SessionTimeOut",
		"Loc/Wec/Plant1/Ctrl/SessionPubKey",
		"Loc/Wec/Plant1/Ctrl/SessionRequest",
		"Loc/Wec/Plant1/Ctrl/SessionSubmit",
		"Loc/Wec/Plant1/Ctrl/SetCtrl",
		"Loc/Wec/Plant1/Ctrl/Ctrl",
		"Loc/Wec/Plant1/Ctrl/SetRbh",
		"Loc/Wec/Plant1/Ctrl/Rbh",
	}
	got := []string{
		sessionStateItem(1, SessionCtrl),
		sessionTimeoutItem(1, SessionCtrl),
		sessionPubKeyItem(1, SessionCtrl),
		sessionRequestItem(1, SessionCtrl),
		sessionSubmitItem(1, SessionCtrl),
		setCtrlItem(1),
		ctrlItem(1),
		setRbhItem(1),
		rbhItem(1),
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("item name = %q, want %q", got[i], want[i])
		}
	}
}
