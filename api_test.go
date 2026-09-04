package energontrol

import (
	"context"
	"testing"
)

// The package-level wrappers were never called by a test, so a wrong or
// forgotten argument in one of them would have gone unnoticed — they are
// trivial, which is exactly why nobody looks at them twice.
//
// Each case here calls the wrapper and asserts the observable effect the
// corresponding method has, so a wrapper that drops a parameter or calls the
// wrong method fails.

func TestPackageLevelCommandWrappers(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name string
		// setup prepares the plant so the command has something to do.
		setup func(*fakeOPC)
		call  func(*fakeOPC) (Results, error)
		// wantItem is the item the command has to write.
		wantItem string
		// wantValue is the first element it has to write there.
		wantValue uint32
	}{
		{
			name:      "Start",
			setup:     func(f *fakeOPC) { f.Ctrl[2] = uint64(CtrlStop90) },
			call:      func(f *fakeOPC) (Results, error) { return Start(ctx, f, testUser, 2) },
			wantItem:  setCtrlItem(2),
			wantValue: uint32(CtrlStart),
		},
		{
			name:  "Stop at 60 degrees",
			setup: func(f *fakeOPC) { f.Ctrl[2] = uint64(CtrlStart) },
			call: func(f *fakeOPC) (Results, error) {
				return Stop(ctx, f, testUser, false, true, 2)
			},
			wantItem:  setCtrlItem(2),
			wantValue: uint32(CtrlStop60),
		},
		{
			name:  "Stop at 90 degrees",
			setup: func(f *fakeOPC) { f.Ctrl[2] = uint64(CtrlStart) },
			call: func(f *fakeOPC) (Results, error) {
				return Stop(ctx, f, testUser, true, true, 2)
			},
			wantItem:  setCtrlItem(2),
			wantValue: uint32(CtrlStop90),
		},
		{
			name:  "SetCtrl",
			setup: func(f *fakeOPC) { f.Ctrl[2] = uint64(CtrlStart) },
			call: func(f *fakeOPC) (Results, error) {
				return SetCtrl(ctx, f, testUser, CtrlGradientStop90, true, 2)
			},
			wantItem:  setCtrlItem(2),
			wantValue: uint32(CtrlGradientStop90),
		},
		{
			name:  "SetRbh",
			setup: func(f *fakeOPC) { f.Rbh[2] = RbhInstalled | RbhAutoDeicingAllowed },
			call: func(f *fakeOPC) (Results, error) {
				return SetRbh(ctx, f, testUser, RbhSetPresetDuration, 2)
			},
			wantItem:  setRbhItem(2),
			wantValue: uint32(RbhSetPresetDuration),
		},
		{
			name:      "RbhOn",
			setup:     func(f *fakeOPC) { f.Rbh[2] = RbhInstalled | RbhAutoDeicingAllowed },
			call:      func(f *fakeOPC) (Results, error) { return RbhOn(ctx, f, testUser, 2) },
			wantItem:  setRbhItem(2),
			wantValue: uint32(RbhSetManualOn),
		},
		{
			name:      "RbhAutoOff",
			setup:     func(f *fakeOPC) { f.Rbh[2] = RbhInstalled | RbhAutoDeicingAllowed },
			call:      func(f *fakeOPC) (Results, error) { return RbhAutoOff(ctx, f, testUser, 2) },
			wantItem:  setRbhItem(2),
			wantValue: uint32(RbhSetAutoOff),
		},
		{
			name: "RbhStandard",
			setup: func(f *fakeOPC) {
				f.Rbh[2] = RbhInstalled | RbhAutoOffWEA | RbhManualOnSCADA
			},
			call:      func(f *fakeOPC) (Results, error) { return RbhStandard(ctx, f, testUser, 2) },
			wantItem:  setRbhItem(2),
			wantValue: uint32(RbhSetStandard),
		},
		{
			name:      "IceDetOn",
			setup:     func(f *fakeOPC) { f.IceDet[2] = 0 },
			call:      func(f *fakeOPC) (Results, error) { return IceDetOn(ctx, f, testUser, 2) },
			wantItem:  setIceDetItem(2),
			wantValue: uint32(IceDetLampOn),
		},
		{
			name:      "IceDetOff",
			setup:     func(f *fakeOPC) { f.IceDet[2] = IceDetExternalSCADA },
			call:      func(f *fakeOPC) (Results, error) { return IceDetOff(ctx, f, testUser, 2) },
			wantItem:  setIceDetItem(2),
			wantValue: uint32(IceDetLampOff),
		},
		{
			name:  "SetIceDet",
			setup: func(f *fakeOPC) { f.IceDet[2] = 0 },
			call: func(f *fakeOPC) (Results, error) {
				return SetIceDet(ctx, f, testUser, IceDetLampOn, 2)
			},
			wantItem:  setIceDetItem(2),
			wantValue: uint32(IceDetLampOn),
		},
		{
			name:      "Reset",
			setup:     func(*fakeOPC) {},
			call:      func(f *fakeOPC) (Results, error) { return Reset(ctx, f, testUser, 2) },
			wantItem:  setResetItem(2),
			wantValue: 2, // SetReset carries the plant number
		},
		{
			name:  "ControlAndRbh",
			setup: func(f *fakeOPC) { f.Ctrl[2] = uint64(CtrlStart) },
			call: func(f *fakeOPC) (Results, error) {
				return ControlAndRbh(ctx, f, testUser, ControlAndRbhValue{
					SetCtrlValue: true, CtrlValue: CtrlStop90, ForceExplicitCommand: true,
				}, 2)
			},
			wantItem:  setCtrlItem(2),
			wantValue: uint32(CtrlStop90),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeOPC()
			tc.setup(f)

			res, err := tc.call(f)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if len(res) != 1 || res[0].PlantNo != 2 {
				t.Fatalf("results = %v, want one result for plant 2", res)
			}
			if res[0].Outcome != OutcomeCommanded {
				t.Fatalf("result = %v, want commanded", res[0])
			}
			var written []uint32
			for _, w := range f.Writes {
				if w.ItemName == tc.wantItem {
					written = w.Value
				}
			}
			if len(written) == 0 {
				t.Fatalf("%s was not written; writes: %v", tc.wantItem, f.WrittenNames())
			}
			if written[0] != tc.wantValue {
				t.Errorf("%s written as %d, want %d", tc.wantItem, written[0], tc.wantValue)
			}
		})
	}
}

func TestPackageLevelReadWrappers(t *testing.T) {
	ctx := context.Background()
	f := parkFake()
	f.Ctrl[2] = uint64(CtrlStop90)
	f.Rbh[2] = RbhInstalled | RbhAutoOffWEA
	f.IceDet[2] = IceDetPowerCurve

	if err := ServerAvailable(ctx, f); err != nil {
		t.Errorf("ServerAvailable: %v", err)
	}

	states, err := PlantCtrlState(ctx, f, 2)
	if err != nil || len(states) != 1 || states[0].Ctrl != CtrlStop90 {
		t.Errorf("PlantCtrlState = %v, %v", states, err)
	}

	rbh, err := PlantRbhState(ctx, f, 2)
	if err != nil || len(rbh) != 1 || rbh[0].Status != RbhInstalled|RbhAutoOffWEA {
		t.Errorf("PlantRbhState = %v, %v", rbh, err)
	}

	ice, err := PlantIceDetState(ctx, f, 2)
	if err != nil || len(ice) != 1 || ice[0].Status != IceDetPowerCurve {
		t.Errorf("PlantIceDetState = %v, %v", ice, err)
	}

	info, err := Turbines(ctx, f)
	if err != nil || info.ParkNo != 4242 || len(info.PlantNo) != 3 {
		t.Errorf("Turbines = %+v, %v", info, err)
	}

	match, err := ParkNoMatch(ctx, f, 4242, true)
	if err != nil || !match {
		t.Errorf("ParkNoMatch = %t, %v", match, err)
	}
}

// The state slices carry their own aggregate helpers, so a caller who wants the
// old all-or-nothing behaviour has one call for it.
func TestStateSliceHelpers(t *testing.T) {
	f := newFakeOPC()
	f.Ctrl[2] = uint64(CtrlStop90)
	f.Ctrl[5] = uint64(CtrlStart)
	f.ItemFault[ctrlItem(5)] = "E_UNKNOWN_ITEM_NAME"

	states, err := New(f).PlantCtrlState(context.Background(), 2, 5)
	if err != nil {
		t.Fatalf("PlantCtrlState: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("got %d states for 2 plants", len(states))
	}
	// The readable plant is still readable.
	got, ok := states.Get(2)
	if !ok || got.Err != nil || got.Ctrl != CtrlStop90 {
		t.Errorf("plant 2 = %v (ok=%t), want %s", got, ok, CtrlStop90)
	}
	// The unreadable one carries its reason rather than a state.
	got, ok = states.Get(5)
	if !ok || got.Err == nil {
		t.Errorf("plant 5 = %v (ok=%t), want an error", got, ok)
	}
	if _, ok := states.Get(9); ok {
		t.Error("Get returned a plant that was never requested")
	}
	if states.Err() == nil {
		t.Error("Err() should surface the unreadable plant")
	}

	// And with every plant readable, Err is nil.
	f2 := newFakeOPC()
	f2.Ctrl[2] = uint64(CtrlStop90)
	clean, err := New(f2).PlantCtrlState(context.Background(), 2)
	if err != nil {
		t.Fatalf("PlantCtrlState: %v", err)
	}
	if clean.Err() != nil {
		t.Errorf("Err() = %v, want nil", clean.Err())
	}
}
