package energontrol

// Listing a park: which plants it holds, which functions each one offers, and
// which nodes this package cannot address.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func parkFake() *fakeOPC {
	f := newFakeOPC()
	f.ParkNo = 4242
	f.Branches = map[uint8][]string{
		2: {"Ctrl", "Reset", "Para", "SetCtrl", "SetRbh", "SetIceDet", "SetReset"},
		5: {"Ctrl", "SetCtrl"},
		9: {"Ctrl", "Reset", "SetCtrl", "SetRbh", "SetReset"},
	}
	return f
}

// A plant node the package cannot address is logged as well as reported, so it
// shows up in an operator's log and not only in a struct field nobody reads.
func TestUnsupportedPlantNodeIsLogged(t *testing.T) {
	f := parkFake()
	f.ExtraBranches = []string{"Loc/Wec/Plant300"}

	rec, logger := newRecordingLogger()
	if _, err := New(f, WithLogger(logger)).Turbines(context.Background()); err != nil {
		t.Fatalf("Turbines: %v", err)
	}
	if got := rec.text(); !strings.Contains(got, "Plant300") {
		t.Errorf("the unaddressable plant was not logged:\n%s", got)
	}
}

// A browse failure surfaces unchanged rather than as an empty park listing.
// Naming the path that was refused is the transport's job, since the transport
// is what talks to the SCADA — see TestAdapterBrowseFailureNamesThePath.
func TestBrowseFailureSurfaces(t *testing.T) {
	for _, path := range []string{"Loc/Wec", "Loc/Wec/Plant5"} {
		t.Run(path, func(t *testing.T) {
			f := parkFake()
			f.BrowseErrOn[path] = errTransport

			info, err := New(f).Turbines(context.Background())
			if !errors.Is(err, errTransport) {
				t.Fatalf("err = %v, want it to wrap the transport error", err)
			}
			if len(info.PlantNo) != 0 {
				t.Errorf("a park listing was returned alongside the error: %+v", info)
			}
		})
	}
}

// Before the fix a node named Plant007 was taken as plant 7 and then addressed
// as Plant7 — an item name the server does not have. The command failed with
// ErrItemMissing, which says nothing about the cause, and the listing claimed
// the plant was addressable. A node this package cannot address under the name
// the server gave it belongs in Unsupported.
func TestPlantNodeThatCannotBeAddressedUnderItsOwnNameIsReported(t *testing.T) {
	f := newFakeOPC()
	f.ExtraNodes = []Node{
		{Name: "Plant007", ItemName: "Loc/Wec/Plant007", HasChildren: true},
	}
	f.Branches = map[uint8][]string{2: {"Ctrl", "SetCtrl"}}

	info, err := New(f).Turbines(context.Background())
	if err != nil {
		t.Fatalf("Turbines: %v", err)
	}
	for _, p := range info.PlantNo {
		if p == 7 {
			t.Error("Plant007 was listed as plant 7, which this package would address as Plant7")
		}
	}
	joined := strings.Join(info.Unsupported, " ")
	if !strings.Contains(joined, "Plant007") {
		t.Errorf("Unsupported = %v, want it to name Plant007", info.Unsupported)
	}
}

// The ordinary spelling stays addressable, of course.
func TestCanonicallyNamedPlantNodeIsAddressable(t *testing.T) {
	f := newFakeOPC()
	f.Branches = map[uint8][]string{2: {"Ctrl", "SetCtrl"}, 11: {"Ctrl", "SetCtrl"}}

	info, err := New(f).Turbines(context.Background())
	if err != nil {
		t.Fatalf("Turbines: %v", err)
	}
	if len(info.PlantNo) != 2 || info.PlantNo[0] != 2 || info.PlantNo[1] != 11 {
		t.Errorf("plants = %v, want [2 11]", info.PlantNo)
	}
	if len(info.Unsupported) != 0 {
		t.Errorf("Unsupported = %v, want none", info.Unsupported)
	}
}

func TestTurbinesReportsEveryFunctionOfEveryPlant(t *testing.T) {
	f := parkFake()
	info, err := New(f).Turbines(context.Background())
	if err != nil {
		t.Fatalf("Turbines: %v", err)
	}
	if info.ParkNo != 4242 {
		t.Errorf("park = %d, want 4242", info.ParkNo)
	}
	if len(info.PlantNo) != 3 {
		t.Fatalf("plants = %v, want three", info.PlantNo)
	}
	// The listing is ordered, so a caller can compare two runs.
	for i, want := range []uint8{2, 5, 9} {
		if info.PlantNo[i] != want {
			t.Errorf("plant %d of the listing is %d, want %d", i, info.PlantNo[i], want)
		}
	}
	want := map[uint8][5]bool{
		// ctrl, rbh, reset, para, icedet
		2: {true, true, true, true, true},
		5: {true, false, false, false, false},
		9: {true, true, true, false, false},
	}
	for plant, w := range want {
		got := [5]bool{info.Ctrl[plant], info.Rbh[plant], info.Reset[plant],
			info.Para[plant], info.IceDet[plant]}
		if got != w {
			t.Errorf("plant %d: ctrl/rbh/reset/para/icedet = %v, want %v", plant, got, w)
		}
	}
	// Every plant is present in every map, so a lookup never reads a missing
	// key as "this plant has no Ctrl".
	for _, plant := range info.PlantNo {
		for name, m := range map[string]map[uint8]bool{
			"Ctrl": info.Ctrl, "Rbh": info.Rbh, "Reset": info.Reset,
			"Para": info.Para, "IceDet": info.IceDet,
		} {
			if _, ok := m[plant]; !ok {
				t.Errorf("plant %d is missing from the %s map", plant, name)
			}
		}
	}
}

// A plant number this package cannot represent used to vanish without a
// trace, and a plant that is not listed is never commanded and never monitored.
func TestTurbinesReportsPlantsItCannotRepresent(t *testing.T) {
	f := parkFake()
	f.ExtraBranches = []string{
		"Loc/Wec/Plant300",   // out of range for uint8
		"Loc/Wec/PlantA",     // not a number
		"Loc/Wec/Substation", // not a plant at all
	}

	info, err := New(f).Turbines(context.Background())
	if err != nil {
		t.Fatalf("Turbines: %v", err)
	}
	if len(info.PlantNo) != 3 {
		t.Errorf("plants = %v, want only the three representable ones", info.PlantNo)
	}
	if len(info.Unsupported) == 0 {
		t.Fatal("a plant that cannot be represented disappeared without a trace")
	}
	joined := strings.Join(info.Unsupported, " ")
	for _, want := range []string{"Plant300", "PlantA"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Unsupported = %v, want it to name %s", info.Unsupported, want)
		}
	}
	// A node that does not even look like a plant is not a lost plant.
	if strings.Contains(joined, "Substation") {
		t.Errorf("Unsupported = %v, should not list a node that is not a plant", info.Unsupported)
	}
}

// A failure while collecting the functions of one plant must not come back
// as a half-filled park listing next to an error. A caller who ignores the error
// would read "not listed" as "has no Ctrl".
func TestTurbinesDoesNotReturnAPartialPark(t *testing.T) {
	f := parkFake()
	f.BrowseErrOn = map[string]error{"Loc/Wec/Plant5": errors.New("browse failed")}

	info, err := New(f).Turbines(context.Background())
	if err == nil {
		t.Fatal("a failed browse produced no error")
	}
	if len(info.PlantNo) != 0 || len(info.Ctrl) != 0 {
		t.Errorf("a partial park was returned alongside the error: %+v", info)
	}
}

func TestTurbinesFailsOnASuspendedServer(t *testing.T) {
	f := parkFake()
	f.ServerState = "suspended"
	if _, err := New(f).Turbines(context.Background()); !errors.Is(err, ErrServerNotRunning) {
		t.Fatalf("err = %v, want it to wrap ErrServerNotRunning", err)
	}
}

// The per-plant browses are independent, so they are issued concurrently.
// The point of the test is the request count, not the wall clock: three plants
// must not cost more than two browses each plus the two park-level requests.
func TestTurbinesDoesNotBrowseMoreThanNecessary(t *testing.T) {
	f := parkFake()
	if _, err := New(f).Turbines(context.Background()); err != nil {
		t.Fatalf("Turbines: %v", err)
	}
	// 1 browse of Loc/Wec, then per plant: the branch browse plus at most one
	// browse each of Ctrl and Reset.
	if limit := 1 + 3*3; f.BrowseCalls > limit {
		t.Errorf("%d browse requests for 3 plants, want at most %d", f.BrowseCalls, limit)
	}
}

func TestParkNo(t *testing.T) {
	f := parkFake()
	got, err := New(f).ParkNo(context.Background())
	if err != nil {
		t.Fatalf("ParkNo: %v", err)
	}
	if got != 4242 {
		t.Errorf("ParkNo = %d, want 4242", got)
	}
}

func TestParkNoRejectsAnUnusableValue(t *testing.T) {
	f := parkFake()
	f.ItemFault[parkNoItem] = "E_UNKNOWN_ITEM_NAME"
	if _, err := New(f).ParkNo(context.Background()); !errors.Is(err, ErrItemFault) {
		t.Fatalf("err = %v, want it to wrap ErrItemFault", err)
	}
}

// A park match read from a server that is not running is not a match a
// caller may act on. It used to come back as (true, nil) whenever
// checkAvailable was false, which is how the live tests guard themselves.
func TestParkNoMatch(t *testing.T) {
	f := parkFake()
	c := New(f)
	ctx := context.Background()

	match, err := c.ParkNoMatch(ctx, 4242, true)
	if err != nil || !match {
		t.Fatalf("ParkNoMatch(4242) = %t, %v; want true, nil", match, err)
	}
	match, err = c.ParkNoMatch(ctx, 1, true)
	if err != nil || match {
		t.Fatalf("ParkNoMatch(1) = %t, %v; want false, nil", match, err)
	}

	f.ServerState = "suspended"
	if _, err := c.ParkNoMatch(ctx, 4242, true); !errors.Is(err, ErrServerNotRunning) {
		t.Errorf("checkAvailable=true on a suspended server: err = %v", err)
	}
	// Even without the explicit availability check, the ServerState carried by
	// the read itself has to be honoured.
	f.ResponseServerState = "suspended"
	if _, err := c.ParkNoMatch(ctx, 4242, false); !errors.Is(err, ErrServerNotRunning) {
		t.Errorf("checkAvailable=false on a suspended server: err = %v", err)
	}
}

func TestFilterPlants(t *testing.T) {
	var nodes []Node
	for _, name := range []string{
		"Loc/Wec/Plant7", "Loc/Wec/Plant300", "Loc/Wec/Plant12",
		"Loc/Wec/PlantX", "Loc/Wec/Weather", "Loc/Wec/Plant7x",
	} {
		nodes = append(nodes, Node{
			Name:        strings.TrimPrefix(name, "Loc/Wec/"),
			ItemName:    name,
			HasChildren: true,
		})
	}
	plants, unsupported := filterPlants(nodes, defaultNamer)
	// Sorted, so two runs against the same park compare equal whatever order
	// the server listed the nodes in.
	if len(plants) != 2 || plants[0] != 7 || plants[1] != 12 {
		t.Errorf("plants = %v, want [7 12]", plants)
	}
	// Everything under Loc/Wec whose name starts with "Plant" but could not be
	// used is a possible lost plant and is reported. A node that is not a plant
	// node at all is not.
	if len(unsupported) != 3 {
		t.Fatalf("unsupported = %v, want the three unusable plant-shaped names", unsupported)
	}
	joined := strings.Join(unsupported, " ")
	for _, want := range []string{"Plant300", "PlantX", "Plant7x"} {
		if !strings.Contains(joined, want) {
			t.Errorf("unsupported = %v, want it to name %s", unsupported, want)
		}
	}
	if strings.Contains(joined, "Weather") {
		t.Errorf("unsupported = %v, should not list a node that is not a plant", unsupported)
	}
}

// OPC XML-DA requires ItemName only for items, so a server may leave it
// empty for a branch. Matching the item name alone let such a plant fall
// through both branches — not addressable and not reported — which is the exact
// silent loss the Unsupported list exists to prevent.
func TestFilterPlantsAcceptsAPlantNamedOnlyInName(t *testing.T) {
	plants, unsupported := filterPlants([]Node{
		{Name: "Plant2", HasChildren: true},
		{Name: "Plant11", HasChildren: true},
	}, defaultNamer)
	if len(plants) != 2 || plants[0] != 2 || plants[1] != 11 {
		t.Errorf("plants = %v, want [2 11]: a plant named only in Name still exists", plants)
	}
	if len(unsupported) != 0 {
		t.Errorf("unsupported = %v, want none: both plants were addressable", unsupported)
	}
}

// The same rule from the other side: a node this function cannot make sense of
// at all is reported, because deciding to stay silent about it would require
// being sure it is not a plant.
func TestFilterPlantsReportsANodeWithNoNameAtAll(t *testing.T) {
	plants, unsupported := filterPlants([]Node{{HasChildren: true}}, defaultNamer)
	if len(plants) != 0 {
		t.Errorf("plants = %v, want none", plants)
	}
	if len(unsupported) != 1 {
		t.Fatalf("unsupported = %v, want the nameless node reported", unsupported)
	}
}

// A number that arrives twice — once as an item name and once as a bare name —
// must not produce two entries, or the plant would be browsed and commanded
// twice.
func TestFilterPlantsDeduplicates(t *testing.T) {
	plants, _ := filterPlants([]Node{
		{Name: "Plant4", ItemName: "Loc/Wec/Plant4", HasChildren: true},
		{Name: "Plant4", HasChildren: true},
	}, defaultNamer)
	if len(plants) != 1 || plants[0] != 4 {
		t.Errorf("plants = %v, want [4]", plants)
	}
}

// End to end: a park whose branches carry no item names is still a park.
func TestTurbinesFindsPlantsWithoutItemNames(t *testing.T) {
	f := newFakeOPC()
	f.ParkNo = 4242
	// No plants via Branches, so Loc/Wec is listed from ExtraNodes alone.
	f.ExtraNodes = []Node{{Name: "Plant3", HasChildren: true}}
	f.Branches = map[uint8][]string{3: {"Ctrl", "SetCtrl"}}

	info, err := New(f).Turbines(context.Background())
	if err != nil {
		t.Fatalf("Turbines: %v", err)
	}
	// Plant 3 appears once, from Branches and from ExtraNodes deduplicated.
	if len(info.PlantNo) != 1 || info.PlantNo[0] != 3 {
		t.Fatalf("plants = %v, want [3]", info.PlantNo)
	}
	if !info.Ctrl[3] {
		t.Error("plant 3 was found but its Ctrl function was not")
	}
	if len(info.Unsupported) != 0 {
		t.Errorf("Unsupported = %v, want none", info.Unsupported)
	}
}

// One plant failing cancels the browses of the others, and those then fail with
// context.Canceled. The error Turbines reports has to be the failure that caused
// the cancellation, not one of the cancellations it triggered — otherwise an
// operator is told "context canceled" for a SCADA that refused a node.
//
// The ordering is forced rather than raced: plant 2's browse is held until plant
// 5 has failed, so plant 2 — the lower index — is the one that comes back
// cancelled.
func TestTurbinesReportsTheCauseNotTheCancellation(t *testing.T) {
	f := parkFake()
	failed := make(chan struct{})
	f.BrowseErrOn["Loc/Wec/Plant5"] = errors.New("browse refused")
	f.OnBrowse = func(path string) {
		switch path {
		case "Loc/Wec/Plant5":
			close(failed)
		case "Loc/Wec/Plant2":
			<-failed
			// Give the failure time to cancel the shared context, so this
			// browse is the one that observes the cancellation.
			time.Sleep(50 * time.Millisecond)
		}
	}

	_, err := New(f).Turbines(context.Background())
	if err == nil {
		t.Fatal("a failed browse produced no error")
	}
	if !strings.Contains(err.Error(), "browse refused") {
		t.Errorf("err = %v, want it to name the underlying failure", err)
	}
}

// A caller who cancels gets that reported, rather than a park listing built from
// plants that were never browsed.
func TestTurbinesHonoursACancelledContext(t *testing.T) {
	f := parkFake()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	info, err := New(f).Turbines(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(info.PlantNo) != 0 {
		t.Errorf("a park listing was returned for a cancelled call: %+v", info)
	}
}
