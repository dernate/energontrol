package energontrol

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dernate/gopcxmlda"
)

// fakeOPC is an in-memory OPC XML-DA server. It models the Enercon session state
// machine faithfully enough to drive the whole package, and exposes knobs for
// the failure modes a real SCADA can produce: values of an unexpected type,
// items with a fault or bad quality, items missing from a response, responses in
// a different order, and sessions that refuse to advance.
//
// Being able to write this at all is the point of the OpcClient interface: in v1
// none of the protocol logic could be exercised without a live SCADA, so the
// tests commanded real turbines.
type fakeOPC struct {
	mu sync.Mutex

	ServerState string             // default "running"
	Ctrl        map[uint8]uint64   // Loc/Wec/Plant<n>/Ctrl/Ctrl
	Rbh         map[uint8]uint64   // Loc/Wec/Plant<n>/Ctrl/Rbh
	IceDet      map[uint8]uint64   // Loc/Wec/Plant<n>/Ctrl/IceDet
	ParkNo      uint64             //
	PubKey      uint64             // default 4711
	Branches    map[uint8][]string // browse result per plant

	// Failure injection, keyed by item name unless noted otherwise.
	ValueType  map[string]string      // "uint16", "int", "float64", "nil", "string"
	Quality    map[string]string      // e.g. "bad", "uncertain"
	ItemFault  map[string]string      // ResultID on a read item
	Omit       map[string]bool        // drop the item from the response
	WriteBad   map[string]string      // ResultID on a written item
	StuckAt    map[uint8]SessionState // session never advances past this state
	Occupied   map[uint8]bool         // session reads as "occupied"
	SessionID  map[uint8]uint64       // session id the server reports back
	SessionTO  uint64                 // seconds left, reported by SessionTimeOut
	ReadBack   map[string][]uint64    // override what a Set… item reads back
	ReadErr    error                  // fail every Read
	WriteErr   error                  // fail every Write
	Reverse    bool                   // return response items in reverse order
	NoHandles  bool                   // do not echo ClientItemHandle
	StripNames bool                   // do not echo ItemName either
	Timestamps bool                   // stamp response items with the current time

	// Observations.
	Writes     []WriteRecord
	ReadCalls  int
	WriteCalls int

	session   map[string]SessionState // "<plant>/<kind>"
	sessionID map[uint8]uint64        // session id the client reserved with
	staged    map[string][]uint32     // value staged in a session, by item name
}

// WriteRecord is one item written by the package under test.
type WriteRecord struct {
	ItemName string
	Value    []uint32
}

func newFakeOPC() *fakeOPC {
	return &fakeOPC{
		ServerState: "running",
		Ctrl:        map[uint8]uint64{},
		Rbh:         map[uint8]uint64{},
		IceDet:      map[uint8]uint64{},
		PubKey:      4711,
		ParkNo:      1234,
		ValueType:   map[string]string{},
		Quality:     map[string]string{},
		ItemFault:   map[string]string{},
		Omit:        map[string]bool{},
		WriteBad:    map[string]string{},
		StuckAt:     map[uint8]SessionState{},
		Occupied:    map[uint8]bool{},
		Branches:    map[uint8][]string{},
		SessionID:   map[uint8]uint64{},
		ReadBack:    map[string][]uint64{},
		SessionTO:   60,
		session:     map[string]SessionState{},
		sessionID:   map[uint8]uint64{},
		staged:      map[string][]uint32{},
	}
}

// WrittenNames returns the item names written so far, in order.
func (f *fakeOPC) WrittenNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.Writes))
	for _, w := range f.Writes {
		out = append(out, w.ItemName)
	}
	return out
}

// Wrote reports whether an item name containing sub was written.
func (f *fakeOPC) Wrote(sub string) bool {
	for _, n := range f.WrittenNames() {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

// EndSessions returns every session to "free", the way the server does once the
// session end timeout has elapsed. A test that issues a second command to the
// same plant has to call it — on a real plant that wait is 360 s after a stop
// and 0 s after a start.
func (f *fakeOPC) EndSessions() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key, state := range f.session {
		if state == SessionWaitEnd {
			f.session[key] = SessionFree
		}
	}
}

// SessionStateOf exposes the simulated session state of a plant.
func (f *fakeOPC) SessionStateOf(plant uint8, kind SessionKind) SessionState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.session[sessionKey(plant, kind)]
}

func sessionKey(plant uint8, kind SessionKind) string {
	return fmt.Sprintf("%d/%s", plant, kind)
}

func (f *fakeOPC) GetStatus(_ context.Context, handle *string, _ string) (gopcxmlda.TGetStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if *handle == "" {
		*handle = "req"
	}
	var st gopcxmlda.TGetStatus
	st.Response.Result.ServerState = f.ServerState
	return st, nil
}

func (f *fakeOPC) Read(_ context.Context, items []gopcxmlda.TItem, requestHandle *string,
	itemHandles *[]string, _ string, _ map[string]interface{}) (gopcxmlda.TRead, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ReadCalls++
	if f.ReadErr != nil {
		return gopcxmlda.TRead{}, f.ReadErr
	}
	f.fillHandles(requestHandle, itemHandles, len(items))

	var out []gopcxmlda.TItem
	for i, req := range items {
		name := req.ItemName
		if f.Omit[name] {
			continue
		}
		item := gopcxmlda.TItem{ItemName: name}
		if f.StripNames {
			item.ItemName = ""
		}
		if !f.NoHandles {
			item.ClientItemHandle = (*itemHandles)[i]
		}
		if f.Timestamps {
			item.Timestamp = time.Now()
		}
		if rid, ok := f.ItemFault[name]; ok {
			item.Error = rid
			out = append(out, item)
			continue
		}
		item.Quality.QualityField = f.qualityOf(name)
		if arr, ok := f.arrayValueOf(name); ok {
			// Enercon defines SessionRequest and the Set… items as arrays of
			// long words; gopcxmlda decodes those into []interface{}.
			boxed := make([]interface{}, 0, len(arr))
			for _, v := range arr {
				boxed = append(boxed, v)
			}
			item.Value.Value = boxed
		} else {
			item.Value.Value = f.typedValue(name, f.valueOf(name))
		}
		out = append(out, item)
	}
	if f.Reverse {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	var r gopcxmlda.TRead
	r.Response.Result.ServerState = f.ServerState
	r.Response.ItemList.Items = out
	return r, nil
}

func (f *fakeOPC) qualityOf(name string) string {
	if q, ok := f.Quality[name]; ok {
		return q
	}
	return "good"
}

// arrayValueOf answers a read for the array items: SessionRequest reports the
// session id (the other two elements are not readable), and the Set… items
// report back the value that was written.
func (f *fakeOPC) arrayValueOf(name string) ([]uint64, bool) {
	var plant uint8
	switch {
	case strings.HasSuffix(name, "/SessionRequest"):
		kindOf(name, "/SessionRequest", &plant)
		id, ok := f.SessionID[plant]
		if !ok {
			id = f.sessionID[plant]
		}
		return []uint64{id, 0, 0}, true
	case strings.HasSuffix(name, "/SetCtrl"), strings.HasSuffix(name, "/SetRbh"),
		strings.HasSuffix(name, "/SetIceDet"), strings.HasSuffix(name, "/SetReset"):
		if v, ok := f.ReadBack[name]; ok {
			return v, true
		}
		if v, ok := f.staged[name]; ok {
			out := make([]uint64, 0, len(v))
			for _, e := range v {
				out = append(out, uint64(e))
			}
			return out, true
		}
		return []uint64{0, 0, 0}, true
	}
	return nil, false
}

// valueOf answers a read for one item from the simulated plant state.
func (f *fakeOPC) valueOf(name string) uint64 {
	if name == parkNoItem {
		return f.ParkNo
	}
	var plant uint8
	switch {
	case strings.HasSuffix(name, "/SessionState"):
		var kind string
		fmt.Sscanf(name, "Loc/Wec/Plant%d/%s", &plant, &kind)
		kind = strings.TrimSuffix(kind, "/SessionState")
		if f.Occupied[plant] {
			return uint64(SessionOccupied)
		}
		return uint64(f.session[sessionKey(plant, SessionKind(kind))])
	case strings.HasSuffix(name, "/SessionTimeOut"):
		return f.SessionTO
	case strings.HasSuffix(name, "/SessionPubKey"):
		return f.PubKey
	case strings.HasSuffix(name, "/Ctrl/Rbh"):
		fmt.Sscanf(name, "Loc/Wec/Plant%d/Ctrl/Rbh", &plant)
		return f.Rbh[plant]
	case strings.HasSuffix(name, "/Ctrl/IceDet"):
		fmt.Sscanf(name, "Loc/Wec/Plant%d/Ctrl/IceDet", &plant)
		return f.IceDet[plant]
	case strings.HasSuffix(name, "/Ctrl/Ctrl"):
		fmt.Sscanf(name, "Loc/Wec/Plant%d/Ctrl/Ctrl", &plant)
		return f.Ctrl[plant]
	}
	return 0
}

// typedValue reproduces the fact that the Go type of an OPC value depends on the
// xsi:type the server chose, not on what the client expects.
func (f *fakeOPC) typedValue(name string, v uint64) any {
	switch f.ValueType[name] {
	case "uint16":
		return uint16(v)
	case "uint32":
		return uint32(v)
	case "int":
		return int(v)
	case "float64":
		return float64(v)
	case "nil":
		return nil
	case "string":
		return fmt.Sprint(v)
	case "uint64", "":
		// Session states are unsignedShort on a real Enercon SCADA, everything
		// else unsignedLong. Reproduce that by default.
		if strings.HasSuffix(name, "/SessionState") {
			return uint16(v)
		}
		return v
	default:
		return v
	}
}

func (f *fakeOPC) Write(_ context.Context, items []gopcxmlda.TItem, requestHandle *string,
	itemHandles *[]string, _ string, _ map[string]interface{}) (gopcxmlda.TWrite, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.WriteCalls++
	if f.WriteErr != nil {
		return gopcxmlda.TWrite{}, f.WriteErr
	}
	f.fillHandles(requestHandle, itemHandles, len(items))

	var out []gopcxmlda.TItem
	for i, req := range items {
		name := req.ItemName
		value, _ := req.Value.Value.([]uint32)
		item := gopcxmlda.TItem{ItemName: name}
		if !f.NoHandles {
			item.ClientItemHandle = (*itemHandles)[i]
		}
		if rid, ok := f.WriteBad[name]; ok {
			item.Error = rid
			out = append(out, item)
			continue
		}
		f.Writes = append(f.Writes, WriteRecord{ItemName: name, Value: value})
		f.applyWrite(name, value)
		out = append(out, item)
	}
	var w gopcxmlda.TWrite
	w.Response.Result.ServerState = f.ServerState
	w.Response.ItemList.Items = out
	return w, nil
}

// applyWrite advances the simulated session state machine:
// free -> reserved on SessionRequest, -> parameter input on a value write,
// -> waiting time session end on SessionSubmit, at which point the staged value
// becomes the plant's new state.
func (f *fakeOPC) applyWrite(name string, value []uint32) {
	var plant uint8
	switch {
	case strings.HasSuffix(name, "/SessionRequest"):
		kind := kindOf(name, "/SessionRequest", &plant)
		if len(value) > 0 {
			f.sessionID[plant] = uint64(value[0])
		}
		f.advance(plant, kind, SessionReserved)
	case strings.HasSuffix(name, "/Ctrl/SetCtrl"), strings.HasSuffix(name, "/Ctrl/SetRbh"),
		strings.HasSuffix(name, "/Ctrl/SetIceDet"):
		fmt.Sscanf(name, "Loc/Wec/Plant%d/Ctrl/", &plant)
		f.staged[name] = value
		f.advance(plant, SessionCtrl, SessionParameterInput)
	case strings.HasSuffix(name, "/Reset/SetReset"):
		fmt.Sscanf(name, "Loc/Wec/Plant%d/Reset/SetReset", &plant)
		f.staged[name] = value
		f.advance(plant, SessionReset, SessionParameterInput)
	case strings.HasSuffix(name, "/SessionSubmit"):
		kind := kindOf(name, "/SessionSubmit", &plant)
		if f.session[sessionKey(plant, kind)] != SessionParameterInput {
			return
		}
		if kind == SessionCtrl {
			if v, ok := f.staged[setCtrlItem(plant)]; ok && len(v) > 0 {
				f.Ctrl[plant] = uint64(v[0])
			}
			if v, ok := f.staged[setRbhItem(plant)]; ok && len(v) > 0 {
				f.Rbh[plant] = rbhStatusAfter(f.Rbh[plant], RbhValue(v[0]))
			}
			if v, ok := f.staged[setIceDetItem(plant)]; ok && len(v) > 0 {
				if IceDetValue(v[0]) == IceDetLampOn {
					f.IceDet[plant] |= IceDetExternalSCADA
				} else {
					f.IceDet[plant] &^= IceDetExternalSCADA
				}
			}
		}
		f.advance(plant, kind, SessionWaitEnd)
	}
}

func kindOf(name, suffix string, plant *uint8) SessionKind {
	trimmed := strings.TrimSuffix(name, suffix)
	var kind string
	fmt.Sscanf(trimmed, "Loc/Wec/Plant%d/%s", plant, &kind)
	return SessionKind(kind)
}

// advance moves a session forward unless the test pinned it.
func (f *fakeOPC) advance(plant uint8, kind SessionKind, to SessionState) {
	if stuck, ok := f.StuckAt[plant]; ok && to > stuck {
		return
	}
	f.session[sessionKey(plant, kind)] = to
}

// rbhStatusAfter models what a plant reports after a heating command, following
// the meaning Enercon gives each command value.
func rbhStatusAfter(before uint64, cmd RbhValue) uint64 {
	status := before | RbhInstalled
	switch cmd {
	case RbhSetStandard: // neither suppress the automatic system nor heat manually
		return status &^ (RbhAutoOffWEA | RbhManualOnSCADA)
	case RbhSetAutoOff: // suppress the automatic system
		return (status | RbhAutoOffWEA) &^ RbhManualOnSCADA
	case RbhSetManualOn: // suppress the automatic system and heat manually
		return status | RbhAutoOffWEA | RbhManualOnSCADA
	case RbhSetPresetDuration: // heat for the preset duration
		return status | RbhHeatingWhenStoppedSCADA
	}
	return status
}

func (f *fakeOPC) Browse(_ context.Context, itemPath string, handle *string, _ string,
	options gopcxmlda.TBrowseOptions) (gopcxmlda.TBrowse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if *handle == "" {
		*handle = "req"
	}
	var b gopcxmlda.TBrowse
	if itemPath == "Loc/Wec" {
		plants := make([]uint8, 0, len(f.Branches))
		for p := range f.Branches {
			plants = append(plants, p)
		}
		sort.Slice(plants, func(i, j int) bool { return plants[i] < plants[j] })
		for _, p := range plants {
			b.Response.Elements = append(b.Response.Elements, gopcxmlda.TBrowseElement{
				Name:        fmt.Sprintf("Plant%d", p),
				ItemName:    fmt.Sprintf("Loc/Wec/Plant%d", p),
				HasChildren: true,
			})
		}
		return b, nil
	}
	var plant uint8
	if _, err := fmt.Sscanf(itemPath, "Loc/Wec/Plant%d", &plant); err != nil {
		return b, nil
	}
	names := f.Branches[plant]
	if strings.HasSuffix(itemPath, "/Ctrl") || strings.HasSuffix(itemPath, "/Reset") {
		for _, n := range names {
			if !strings.HasPrefix(n, "Set") {
				continue
			}
			if options.ElementNameFilter == "SetReset" && n != "SetReset" {
				continue
			}
			b.Response.Elements = append(b.Response.Elements,
				gopcxmlda.TBrowseElement{Name: n, ItemName: itemPath + "/" + n, IsItem: true})
		}
		return b, nil
	}
	for _, n := range names {
		if strings.HasPrefix(n, "Set") {
			continue
		}
		b.Response.Elements = append(b.Response.Elements,
			gopcxmlda.TBrowseElement{Name: n, ItemName: itemPath + "/" + n, HasChildren: true})
	}
	return b, nil
}

// fillHandles mirrors how gopcxmlda generates and returns client handles.
func (f *fakeOPC) fillHandles(requestHandle *string, itemHandles *[]string, count int) {
	if *requestHandle == "" {
		*requestHandle = "req"
	}
	if len(*itemHandles) == 0 {
		h := make([]string, count)
		for i := range h {
			h[i] = fmt.Sprintf("handle_%d_%d", f.ReadCalls+f.WriteCalls, i)
		}
		*itemHandles = h
	}
}
