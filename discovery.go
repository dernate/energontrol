package energontrol

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	values, err := c.readValues(ctx, []string{c.items.parkNo()})
	if err != nil {
		return 0, err
	}
	v := values[c.items.parkNo()]
	if v.Err != nil {
		return 0, v.Err
	}
	return v.Value, nil
}

// ParkNoMatch reports whether the connected server serves the given park.
//
// It is the guard a caller puts in front of a command when a set of credentials
// could be pointed at the wrong park. The read itself honours the ServerState
// the response carries, so a server that reports a state other than "running"
// never reports a match — with checkAvailable false that used to come back as
// (true, nil). A server that reports no state at all is tolerated, here as
// everywhere: that is the absence of a statement rather than a statement of
// failure.
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
// has no Ctrl", which is the more dangerous of the two ways to be wrong. That
// is also why a browse the server pages is followed to its end by the transport
// and reported with ErrBrowseIncomplete if it cannot be: a listing that is
// merely short is indistinguishable from a park that has fewer turbines.
//
// Plant nodes the package cannot address are not dropped silently either: they
// are reported in TurbineInfo.Unsupported. Plants are listed in ascending
// number, so two runs against the same park compare equal.
func (c *Client) Turbines(ctx context.Context) (TurbineInfo, error) {
	if err := c.ServerAvailable(ctx); err != nil {
		return TurbineInfo{}, err
	}
	nodes, err := c.transport.Browse(ctx, c.items.parkBranch(), BrowseFilter{})
	if err != nil {
		return TurbineInfo{}, err
	}
	plants, unsupported := filterPlants(nodes, c.items)
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
func (c *Client) plantFunctions(ctx context.Context, plant uint8) (plantFunctionSet, error) {
	set := plantFunctionSet{plant: plant}
	branches, err := c.transport.Browse(ctx, c.items.plantBranch(plant),
		BrowseFilter{BranchesOnly: true})
	if err != nil {
		return set, err
	}
	for _, branch := range branches {
		if !branch.HasChildren {
			continue
		}
		switch branch.Name {
		case string(SessionCtrl):
			setters, err := c.transport.Browse(ctx, c.items.plantSubBranch(plant, SessionCtrl),
				BrowseFilter{NamePattern: "Set*"})
			if err != nil {
				return set, err
			}
			for _, item := range setters {
				switch item.Name {
				case "SetCtrl":
					set.ctrl = true
				case "SetRbh":
					set.rbh = true
				case "SetIceDet":
					set.iceDet = true
				}
			}
		case string(SessionReset):
			setters, err := c.transport.Browse(ctx, c.items.plantSubBranch(plant, SessionReset),
				BrowseFilter{NamePattern: "SetReset"})
			if err != nil {
				return set, err
			}
			for _, item := range setters {
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

// plantNamePattern matches the element name of a plant node.
//
// The element name is taken from the full item name where the server supplied
// one and from the bare Name otherwise, because OPC XML-DA requires ItemName
// only for items: a server may leave it empty for a branch, and a plant named
// only in Name still exists. Matching the item name alone let such a plant fall
// through every branch of filterPlants — neither addressable nor reported —
// which is the exact silent loss this listing promises not to have.
var plantNamePattern = regexp.MustCompile(`^Plant(\d+)$`)

// plantishPattern matches anything that mentions a plant at all, so a node that
// looks like one but cannot be addressed is reported rather than dropped. A
// park's other branches — Sum and the like — do not match and are skipped
// without noise.
var plantishPattern = regexp.MustCompile(`(?i)plant`)

// filterPlants splits a browse of the park branch into the plants this package
// can address, sorted, and the plant nodes it cannot.
//
// The second return value is the point of the split. Plant numbers are uint8 on
// purpose: an Enercon park holds 20 to 30 turbines, so 255 is an order of
// magnitude of headroom, and the narrow type keeps the number usable as a map
// key and struct field everywhere without conversions. But a plant that is
// missing from the listing is never commanded and never monitored, so the range
// must not be allowed to swallow one silently — which is what happened before.
// A park that does exceed it shows up here, not as a turbine that quietly does
// not exist.
//
// Two more rules follow from the same principle. A node this function cannot
// make sense of at all is reported rather than skipped, because deciding to
// stay silent about one requires being sure it is not a plant. And a node whose
// own name is not the name this package would build for its number — Plant007
// for 7, say — is reported too: commanding it would address the canonical name,
// which such a server does not have, so it is not addressable and must not be
// listed as though it were.
func filterPlants(nodes []Node, items itemNamer) (plants []uint8, unsupported []string) {
	seen := make(map[uint8]struct{}, len(nodes))
	branch := items.parkBranch() + "/"
	for _, node := range nodes {
		label := node.ItemName
		if label == "" {
			label = node.Name
		}
		element := node.Name
		if node.ItemName != "" {
			rest, ok := strings.CutPrefix(node.ItemName, branch)
			if !ok {
				if plantishPattern.MatchString(label) {
					unsupported = append(unsupported,
						label+" (not directly below "+items.parkBranch()+")")
				}
				continue
			}
			element = rest
		}
		match := plantNamePattern.FindStringSubmatch(element)
		if match == nil {
			switch {
			case label == "":
				unsupported = append(unsupported,
					"<a browse element with neither a name nor an item name>")
			case plantishPattern.MatchString(label):
				unsupported = append(unsupported, label+" (not a Plant<n> node)")
			}
			continue
		}
		num, err := strconv.ParseUint(match[1], 10, 8)
		if err != nil {
			// A well-formed plant number that does not fit in a uint8.
			unsupported = append(unsupported, label+" (plant number out of range)")
			continue
		}
		if element != plantElementName(uint8(num)) {
			unsupported = append(unsupported, label+
				" (this package would address it as "+items.plantBranch(uint8(num))+")")
			continue
		}
		if _, dup := seen[uint8(num)]; dup {
			continue
		}
		seen[uint8(num)] = struct{}{}
		plants = append(plants, uint8(num))
	}
	slices.Sort(plants)
	return plants, unsupported
}
