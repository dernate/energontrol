package energontrol

import (
	"context"
	"fmt"
	"regexp"
	"strconv"

	"github.com/dernate/gopcxmlda"
)

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
func (c *Client) Turbines(ctx context.Context) (TurbineInfo, error) {
	if err := c.ServerAvailable(ctx); err != nil {
		return TurbineInfo{}, err
	}
	var handle string
	browse, err := c.opc.Browse(ctx, "Loc/Wec", &handle, "", gopcxmlda.TBrowseOptions{})
	if err != nil {
		return TurbineInfo{}, wrapf(err, "browse Loc/Wec")
	}
	info := TurbineInfo{
		PlantNo: filterPlants(browse),
		Ctrl:    make(map[uint8]bool),
		Rbh:     make(map[uint8]bool),
		Reset:   make(map[uint8]bool),
		Para:    make(map[uint8]bool),
		IceDet:  make(map[uint8]bool),
	}
	parkNo, err := c.ParkNo(ctx)
	if err != nil {
		return info, err
	}
	info.ParkNo = parkNo
	for _, plant := range info.PlantNo {
		if err := c.plantFunctions(ctx, plant, &info); err != nil {
			return info, err
		}
	}
	return info, nil
}

// plantFunctions fills in which functions one plant offers. Every request uses
// its own request and item handles: reusing them across calls made the request
// count and the handle count drift apart, which gopcxmlda rejects.
func (c *Client) plantFunctions(ctx context.Context, plant uint8, info *TurbineInfo) error {
	// Record an explicit false for every function first, so a plant that offers
	// none is still present in every map.
	for _, m := range []map[uint8]bool{info.Ctrl, info.Rbh, info.Reset, info.Para, info.IceDet} {
		if _, ok := m[plant]; !ok {
			m[plant] = false
		}
	}
	var handle string
	branches, err := c.opc.Browse(ctx, fmt.Sprintf("Loc/Wec/Plant%d", plant), &handle, "",
		gopcxmlda.TBrowseOptions{BrowseFilter: "branch"})
	if err != nil {
		return wrapf(err, "browse plant %d", plant)
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
				return wrapf(err, "browse plant %d Ctrl", plant)
			}
			for _, item := range setters.Response.Elements {
				switch item.Name {
				case "SetCtrl":
					info.Ctrl[plant] = true
				case "SetRbh":
					info.Rbh[plant] = true
				case "SetIceDet":
					info.IceDet[plant] = true
				}
			}
		case "Reset":
			var h string
			setters, err := c.opc.Browse(ctx, fmt.Sprintf("Loc/Wec/Plant%d/Reset", plant), &h, "",
				gopcxmlda.TBrowseOptions{ElementNameFilter: "SetReset"})
			if err != nil {
				return wrapf(err, "browse plant %d Reset", plant)
			}
			for _, item := range setters.Response.Elements {
				if item.Name == "SetReset" {
					info.Reset[plant] = true
				}
			}
		case "Para":
			info.Para[plant] = true
		}
	}
	return nil
}

var plantPattern = regexp.MustCompile(`^Loc/Wec/Plant(\d+)$`)

func filterPlants(browse gopcxmlda.TBrowse) []uint8 {
	var plants []uint8
	for _, item := range browse.Response.Elements {
		matches := plantPattern.FindStringSubmatch(item.ItemName)
		if matches == nil {
			continue
		}
		num, err := strconv.ParseUint(matches[1], 10, 8)
		if err != nil {
			continue
		}
		plants = append(plants, uint8(num))
	}
	return plants
}
