package energontrol

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A transport failure can hit any phase of the session procedure, and what has
// to happen differs by phase: before the reservation nothing can be left
// behind, after it the server may hold a session this client can no longer
// finish. None of these paths was covered before — the fake had the knobs for
// them and no test set one.

var errTransport = errors.New("connection reset by peer")

// A failure before anything was reserved fails the command and leaves nothing
// open.
func TestTransportFailureWhileReadingTheStateFailsTheCall(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.ReadErrOn[ctrlItem(2)] = errTransport

	_, err := New(f).Stop(context.Background(), testUser, true, true, 2)
	if !errors.Is(err, errTransport) {
		t.Fatalf("err = %v, want it to wrap the transport error", err)
	}
	if f.WriteCalls != 0 {
		t.Errorf("%d writes went out after the state could not be read", f.WriteCalls)
	}
}

// A failure on the reservation write is the interesting case: the server may
// have reserved the session even though the response never arrived, so the
// session has to be accounted for.
func TestTransportFailureOnTheReservationIsReportedPerPlant(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.WriteErrOn[sessionRequestItem(2, SessionCtrl)] = errTransport

	res, err := New(f).Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome == OutcomeCommanded {
		t.Fatal("a plant whose reservation never completed was reported as commanded")
	}
	if !errors.Is(res[0].Err, errTransport) {
		t.Errorf("err = %v, want it to wrap the transport error", res[0].Err)
	}
	// The fake never applied the write, so the session is provably free and
	// must not be reported as left open. The cleanup has to establish that by
	// reading the state, not by guessing.
	if errors.Is(res[0].Err, ErrSessionLeftOpen) {
		t.Errorf("a session that was never reserved was reported as left open: %v", res[0].Err)
	}
}

// The same failure, but the server did reserve the session before the
// connection broke: now it is left open and the caller has to be told.
func TestTransportFailureAfterAReservationReportsTheOpenSession(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	// The value write fails at transport level, after SessionRequest landed.
	f.WriteErrOn[setCtrlItem(2)] = errTransport

	res, err := New(f).Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].Outcome == OutcomeCommanded {
		t.Fatal("a plant whose value write failed was reported as commanded")
	}
	if !errors.Is(res[0].Err, ErrSessionLeftOpen) {
		t.Errorf("err = %v, want it to wrap ErrSessionLeftOpen", res[0].Err)
	}
	if f.SessionStateOf(2, SessionCtrl) != SessionReserved {
		t.Errorf("session state = %v, want it still reserved on the server",
			f.SessionStateOf(2, SessionCtrl))
	}
}

// A failure on the submit leaves a session that holds a value but was never
// committed. Nothing may claim the command was carried out.
func TestTransportFailureOnTheSubmitIsNotASuccess(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.WriteErrOn[sessionSubmitItem(2, SessionCtrl)] = errTransport

	res, err := New(f).Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res[0].InRequestedState() {
		t.Fatal("a session that was never submitted was reported as in the requested state")
	}
	if got := f.Ctrl[2]; got != uint64(CtrlStart) {
		t.Errorf("plant state = %d, want it unchanged at %d", got, CtrlStart)
	}
}

// A failure while polling for a state transition is a failure of the whole
// request: it says nothing about the individual plants, so every plant still in
// play inherits it.
func TestTransportFailureWhilePollingFailsEveryActivePlant(t *testing.T) {
	f := newFakeOPC()
	for _, p := range []uint8{2, 5} {
		f.Ctrl[p] = uint64(CtrlStart)
	}
	f.ReadErrOn[sessionStateItem(5, SessionCtrl)] = errTransport

	res, err := New(f, WithSessionPolling(time.Millisecond, 10*time.Millisecond)).
		Stop(context.Background(), testUser, true, true, 2, 5)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for _, r := range res {
		if r.InRequestedState() {
			t.Errorf("plant %d was reported as stopped although polling failed", r.PlantNo)
		}
		if !errors.Is(r.Err, errTransport) {
			t.Errorf("plant %d: err = %v, want it to wrap the transport error", r.PlantNo, r.Err)
		}
	}
}

// The cleanup read runs on a context of its own, so it still happens when the
// caller's context is already cancelled — and if it too fails, the session is
// reported as left open rather than assumed to be free.
func TestCleanupFailureReportsTheSessionAsLeftOpen(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStart)
	f.StuckAt[2] = SessionReserved

	c := New(f, WithSessionPolling(time.Millisecond, 10*time.Millisecond))
	// Break the connection only once the session has been reserved and the
	// value written, so the read the cleanup needs is the one that fails.
	f.OnWrite = func(name string) {
		if name == setCtrlItem(2) {
			f.ReadErr = errTransport
		}
	}

	res, err := c.Stop(context.Background(), testUser, true, true, 2)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !errors.Is(res[0].Err, ErrSessionLeftOpen) {
		t.Errorf("err = %v, want it to wrap ErrSessionLeftOpen", res[0].Err)
	}
	// The transport failure that caused it stays visible next to it.
	if !errors.Is(res[0].Err, errTransport) {
		t.Errorf("err = %v, want the transport error to remain visible", res[0].Err)
	}
}

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

// GetStatus failing means the server could not be reached at all.
func TestGetStatusFailureSurfaces(t *testing.T) {
	f := newFakeOPC()
	f.StatusErr = errTransport

	if err := New(f).ServerAvailable(context.Background()); !errors.Is(err, errTransport) {
		t.Fatalf("err = %v, want it to wrap the transport error", err)
	}
	if _, err := New(f).Stop(context.Background(), testUser, true, true, 2); !errors.Is(err, errTransport) {
		t.Fatalf("Stop: err = %v, want it to wrap the transport error", err)
	}
}

// A browse failure surfaces with the path that was being browsed, so an
// operator can see which node the SCADA refused.
func TestBrowseFailureNamesThePath(t *testing.T) {
	f := parkFake()
	f.BrowseErrOn["Loc/Wec"] = errTransport

	_, err := New(f).Turbines(context.Background())
	if !errors.Is(err, errTransport) {
		t.Fatalf("err = %v, want it to wrap the transport error", err)
	}
	if want := "browse Loc/Wec"; !strings.Contains(err.Error(), want) {
		t.Errorf("err = %q, want it to mention %q", err, want)
	}
}
