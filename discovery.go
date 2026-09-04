package energontrol

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/dernate/gopcxmlda"
)

// discoveryParallelism bounds how many plants are browsed at once.
//
// The per-plant browses are independent, and a park of forty plants otherwise
// costs well over a hundred sequential round trips — several seconds of startup
// for a call many callers make before anything else. The SCADA server is the
// bottleneck, not the client, so this stays low enough not to overrun it.
const discoveryParallelism = 6

// ParkNo reads the park number of the connected server.
func (c *Client) ParkNo(ctx context.Context) (uint64, error) {
	values, err := c.readValues(ctx, []string{parkNoItem})
	if err != nil {
		return 0, err
	}
	v := values[parkNoItem]
	if v.Err != nil {
		return 0, v.Err
	}
	return v.Value, nil
}

// ParkNoMatch reports whether the connected server serves the given park.
//
// It is the guard a caller puts in front of a command when a set of credentials
// could be pointed at the wrong park. The read itself honours the ServerState
// the response carries, so a match is never reported by a server that is not
// running — with checkAvailable false that used to come back as (true, nil).
//
// v1 compared the raw interface value against the expected number, so a server
// that typed the item as anything other than unsignedLong reported "no match"
// rather than an error — and an empty response panicked.
func (c *Client) ParkNoMatch(ctx context.Context, parkNo uint64, checkAvailable bool) (bool, error) {
	if checkAvailable {
		if err := c.ServerAvailable(ctx); err != nil {
			return false, err
		}
	}
	got, err := c.ParkNo(ctx)
	if err != nil {
		return false, err
	}
	return got == parkNo, nil
}

// Turbines lists the plants of the park and which functions each one offers.
//
// The listing is all or nothing: on any failure it returns the zero
// TurbineInfo and the error, never a half-filled park next to one. A caller who
// overlooked the error would otherwise read "not in the listing" as "this plant
// has no Ctrl", which is the more dangerous of the two ways to be wrong.
//
// Plant nodes the package cannot address are not dropped silently either: they
// are reported in TurbineInfo.Unsupported. Plants are listed in ascending
// number, so two runs against the same park compare equal.
func (c *Client) Turbines(ctx context.Context) (TurbineInfo, error) {
	if err := c.ServerAvailable(ctx); err != nil {
		return TurbineInfo{}, err
	}
	var handle string
	browse, err := c.opc.Browse(ctx, "Loc/Wec", &handle, "", gopcxmlda.TBrowseOptions{})
	if err != nil {
		return TurbineInfo{}, wrapf(err, "browse Loc/Wec")
	}
	plants, unsupported := filterPlants(browse)
	for _, name := range unsupported {
		c.log.Warn("plant node cannot be addressed by this package and is not in the listing",
			"itemName", name)
	}
	parkNo, err := c.ParkNo(ctx)
	if err != nil {
		return TurbineInfo{}, err
	}

	sets, err := c.browsePlantFunctions(ctx, plants)
	if err != nil {
		return TurbineInfo{}, err
	}

	info := TurbineInfo{
		ParkNo:      parkNo,
		PlantNo:     plants,
		Unsupported: unsupported,
		Ctrl:        make(map[uint8]bool, len(plants)),
		Rbh:         make(map[uint8]bool, len(plants)),
		Reset:       make(map[uint8]bool, len(plants)),
		Para:        make(map[uint8]bool, len(plants)),
		IceDet:      make(map[uint8]bool, len(plants)),
	}
	// Every plant gets an explicit entry in every map, so a lookup for a plant
	// that offers nothing is false rather than a missing key.
	for _, set := range sets {
		info.Ctrl[set.plant] = set.ctrl
		info.Rbh[set.plant] = set.rbh
		info.Reset[set.plant] = set.reset
		info.Para[set.plant] = set.para
		info.IceDet[set.plant] = set.iceDet
	}
	return info, nil
}

// plantFunctionSet is which functions one plant offers. It is a value rather
// than a mutation of TurbineInfo so the per-plant browses can run concurrently
// without sharing maps.
type plantFunctionSet struct {
	plant                          uint8
	ctrl, rbh, reset, para, iceDet bool
}

// browsePlantFunctions collects the function set of every plant, at most
// discoveryParallelism at a time. A partial result is never returned.
//
// The error that comes back is the one that arrived first, not the earliest by
// plant number: the first failure cancels the others, and those then fail with
// context.Canceled. Reporting one of those would tell an operator "context
// canceled" for a SCADA that refused a node. The first recorded error therefore
// wins and is the only one that triggers the cancellation, so a cancellation
// this function caused itself can never overwrite its cause. A cancellation
// that comes from the caller's own context still surfaces, because then nothing
// has been recorded yet.
func (c *Client) browsePlantFunctions(ctx context.Context, plants []uint8) ([]plantFunctionSet, error) {
	sets := make([]plantFunctionSet, len(plants))

	// Cancelling stops the plants that have not started yet, so one failure
	// does not keep issuing requests for the rest of the park.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu       sync.Mutex
		firstErr error
	)
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if firstErr != nil {
			return
		}
		firstErr = err
		cancel()
	}

	sem := make(chan struct{}, discoveryParallelism)
	var wg sync.WaitGroup
	for i, plant := range plants {
		wg.Add(1)
		go func(i int, plant uint8) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				fail(ctx.Err())
				return
			}
			set, err := c.plantFunctions(ctx, plant)
			if err != nil {
				fail(err)
				return
			}
			sets[i] = set
		}(i, plant)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if firstErr != nil {
		return nil, firstErr
	}
	return sets, nil
}

// plantFunctions reports which functions one plant offers.
//
// It browses the plant's branches first and only descends into the ones that
// exist, rather than guessing at Ctrl and Reset and swallowing the error of a
// node that is not there — a swallowed error would turn a broken connection
// into "this plant offers nothing".
//
// Every request uses its own request and item handles: reusing them across
// calls made the request count and the handle count drift apart, which
// gopcxmlda rejects.
func (c *Client) plantFunctions(ctx context.Context, plant uint8) (plantFunctionSet, error) {
	set := plantFunctionSet{plant: plant}
	var handle string
	branches, err := c.opc.Browse(ctx, fmt.Sprintf("Loc/Wec/Plant%d", plant), &handle, "",
		gopcxmlda.TBrowseOptions{BrowseFilter: "branch"})
	if err != nil {
		return set, wrapf(err, "browse plant %d", plant)
	}
	for _, branch := range branches.Response.Elements {
		if !branch.HasChildren {
			continue
		}
		switch branch.Name {
		case "Ctrl":
			var h string
			setters, err := c.opc.Browse(ctx, fmt.Sprintf("Loc/Wec/Plant%d/Ctrl", plant), &h, "",
				gopcxmlda.TBrowseOptions{ElementNameFilter: "Set*"})
			if err != nil {
				return set, wrapf(err, "browse plant %d Ctrl", plant)
			}
			for _, item := range setters.Response.Elements {
				switch item.Name {
				case "SetCtrl":
					set.ctrl = true
				case "SetRbh":
					set.rbh = true
				case "SetIceDet":
					set.iceDet = true
				}
			}
		case "Reset":
			var h string
			setters, err := c.opc.Browse(ctx, fmt.Sprintf("Loc/Wec/Plant%d/Reset", plant), &h, "",
				gopcxmlda.TBrowseOptions{ElementNameFilter: "SetReset"})
			if err != nil {
				return set, wrapf(err, "browse plant %d Reset", plant)
			}
			for _, item := range setters.Response.Elements {
				if item.Name == "SetReset" {
					set.reset = true
				}
			}
		case "Para":
			set.para = true
		}
	}
	return set, nil
}

// plantPattern matches the plant nodes this package can address.
var plantPattern = regexp.MustCompile(`^Loc/Wec/Plant(\d+)$`)

// plantNodePrefix is what a node has to start with to be a plant node at all.
// Anything under it that plantPattern does not match is a plant this package
// cannot address, which is a loss and is reported rather than dropped.
const plantNodePrefix = "Loc/Wec/Plant"

// filterPlants splits a browse of Loc/Wec into the plants this package can
// address, sorted, and the plant nodes it cannot.
//
// The second return value is the point of the split. Plant numbers are uint8 on
// purpose: an Enercon park holds 20 to 30 turbines, so 255 is an order of
// magnitude of headroom, and the narrow type keeps the number usable as a map
// key and struct field everywhere without conversions. But a plant that is
// missing from the listing is never commanded and never monitored, so the range
// must not be allowed to swallow one silently — which is what happened before.
// A park that does exceed it shows up here, not as a turbine that quietly does
// not exist.
func filterPlants(browse gopcxmlda.TBrowse) (plants []uint8, unsupported []string) {
	for _, item := range browse.Response.Elements {
		if matches := plantPattern.FindStringSubmatch(item.ItemName); matches != nil {
			num, err := strconv.ParseUint(matches[1], 10, 8)
			if err == nil {
				plants = append(plants, uint8(num))
				continue
			}
			// A well-formed plant number that does not fit in a uint8.
			unsupported = append(unsupported,
				fmt.Sprintf("%s (plant number out of range)", item.ItemName))
			continue
		}
		if strings.HasPrefix(item.ItemName, plantNodePrefix) {
			unsupported = append(unsupported,
				fmt.Sprintf("%s (not a Loc/Wec/Plant<n> node)", item.ItemName))
		}
	}
	slices.Sort(plants)
	return plants, unsupported
}
