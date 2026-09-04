package energontrol

import (
	"context"
	"fmt"
	"sort"
	"strconv"
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
	StatusErr  error                  // fail every GetStatus
	Reverse    bool                   // return response items in reverse order
	NoHandles  bool                   // do not echo ClientItemHandle
	StripNames bool                   // do not echo ItemName either
	Timestamps bool                   // stamp response items with the current time

	OmitWrite  map[string]bool  // apply the write but drop the item from the response
	ReadErrOn  map[string]error // fail a Read that requests an item with this name
	WriteErrOn map[string]error // fail a Write that carries an item with this name

	// ResponseServerState is the ServerState reported on Read and Write
	// responses. Empty means "the same as ServerState", which is what a healthy
	// server does; setting it models a server that degrades after GetStatus.
	ResponseServerState string

	// ScalarArrays returns the array items as a bare scalar instead of a
	// one-element array, which some servers do for a single value.
	ScalarArrays bool

	// LoopModeAfterSubmit routes the session through SessionWaitLoop (3) after
	// the submit instead of going straight to SessionWaitEnd (4).
	LoopModeAfterSubmit bool

	// AutoFreeSessions returns a session to "free" once its final state has been
	// observed, the way the session end timeout does on a real plant. A test
	// that issues several commands to one plant needs it.
	AutoFreeSessions bool

	// OnWrite is called for every item name a Write carries, before the fake
	// takes its own lock, so a test can observe overlapping requests.
	OnWrite func(itemName string)

	// ExtraBranches are additional item names a browse of Loc/Wec reports, for
	// nodes that are not plants this package can address.
	ExtraBranches []string
	// BrowseErrOn fails a browse of exactly this item path.
	BrowseErrOn map[string]error
	// OnBrowse is called for every browsed path, before the fake takes its own
	// lock, so a test can order concurrent browses against each other.
	OnBrowse func(itemPath string)

	// Observations.
	Writes      []WriteRecord
	ReadCalls   int
	WriteCalls  int
	BrowseCalls int

	session     map[string]SessionState // "<plant>/<kind>"
	sessionID   map[uint8]uint64        // session id the client reserved with
	staged      map[string][]uint32     // value staged in a session, by item name
	loopPending []string                // sessions in loop mode, promoted on the next read
	freePending []string                // finished sessions, released on the next read
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
		OmitWrite:   map[string]bool{},
		ReadErrOn:   map[string]error{},
		WriteErrOn:  map[string]error{},
		BrowseErrOn: map[string]error{},
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

func (f *fakeOPC) GetStatus(ctx context.Context, handle *string, _ string) (gopcxmlda.TGetStatus, error) {
	if err := ctx.Err(); err != nil {
		return gopcxmlda.TGetStatus{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.StatusErr != nil {
		return gopcxmlda.TGetStatus{}, f.StatusErr
	}
	if *handle == "" {
		*handle = "req"
	}
	var st gopcxmlda.TGetStatus
	st.Response.Result.ServerState = f.ServerState
	return st, nil
}

func (f *fakeOPC) Read(ctx context.Context, items []gopcxmlda.TItem, requestHandle *string,
	itemHandles *[]string, _ string, _ map[string]interface{}) (gopcxmlda.TRead, error) {
	// A real transport fails a cancelled request rather than serving it, and
	// the cleanup path depends on that being true.
	if err := ctx.Err(); err != nil {
		return gopcxmlda.TRead{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ReadCalls++
	if f.ReadErr != nil {
		return gopcxmlda.TRead{}, f.ReadErr
	}
	for _, req := range items {
		if err, ok := f.ReadErrOn[req.ItemName]; ok {
			return gopcxmlda.TRead{}, err
		}
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
			if f.ScalarArrays && len(arr) > 0 {
				item.Value.Value = arr[0]
			} else {
				boxed := make([]interface{}, 0, len(arr))
				for _, v := range arr {
					boxed = append(boxed, v)
				}
				item.Value.Value = boxed
			}
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
	r.Response.Result.ServerState = f.responseServerState()
	r.Response.ItemList.Items = out
	return r, nil
}

// responseServerState is the ServerState reported on Read and Write responses.
func (f *fakeOPC) responseServerState() string {
	if f.ResponseServerState != "" {
		return f.ResponseServerState
	}
	return f.ServerState
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
	switch {
	case strings.HasSuffix(name, "/SessionRequest"):
		plant, _, _ := parseItem(name)
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
	plant, kind, leaf := parseItem(name)
	switch leaf {
	case "SessionState":
		if f.Occupied[plant] {
			return uint64(SessionOccupied)
		}
		key := sessionKey(plant, kind)
		state := f.session[key]
		// A session parked in loop mode moves on to "waiting time session end"
		// once it has been observed there, the way a real waiting time elapses.
		for i, pending := range f.loopPending {
			if pending == key {
				f.session[key] = SessionWaitEnd
				f.loopPending = append(f.loopPending[:i], f.loopPending[i+1:]...)
				break
			}
		}
		// Likewise a finished session returns to "free" once its final state
		// has been observed, which is what the session end timeout does.
		for i, pending := range f.freePending {
			if pending == key {
				f.session[key] = SessionFree
				f.freePending = append(f.freePending[:i], f.freePending[i+1:]...)
				break
			}
		}
		return uint64(state)
	case "SessionTimeOut":
		return f.SessionTO
	case "SessionPubKey":
		return f.PubKey
	case "Rbh":
		return f.Rbh[plant]
	case "IceDet":
		return f.IceDet[plant]
	case "Ctrl":
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

func (f *fakeOPC) Write(ctx context.Context, items []gopcxmlda.TItem, requestHandle *string,
	itemHandles *[]string, _ string, _ map[string]interface{}) (gopcxmlda.TWrite, error) {
	if err := ctx.Err(); err != nil {
		return gopcxmlda.TWrite{}, err
	}
	if f.OnWrite != nil {
		for _, req := range items {
			f.OnWrite(req.ItemName)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.WriteCalls++
	if f.WriteErr != nil {
		return gopcxmlda.TWrite{}, f.WriteErr
	}
	for _, req := range items {
		if err, ok := f.WriteErrOn[req.ItemName]; ok {
			return gopcxmlda.TWrite{}, err
		}
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
		if f.OmitWrite[name] {
			// The write was applied, but the server does not echo the item —
			// the response carries no confirmation for it either way.
			continue
		}
		out = append(out, item)
	}
	var w gopcxmlda.TWrite
	w.Response.Result.ServerState = f.responseServerState()
	w.Response.ItemList.Items = out
	return w, nil
}

// applyWrite advances the simulated session state machine:
// free -> reserved on SessionRequest, -> parameter input on a value write,
// -> waiting time session end on SessionSubmit, at which point the staged value
// becomes the plant's new state.
func (f *fakeOPC) applyWrite(name string, value []uint32) {
	plant, kind, leaf := parseItem(name)
	switch leaf {
	case "SessionRequest":
		if len(value) > 0 {
			f.sessionID[plant] = uint64(value[0])
		}
		f.advance(plant, kind, SessionReserved)
	case "SetCtrl", "SetRbh", "SetIceDet", "SetReset":
		f.staged[name] = value
		f.advance(plant, kind, SessionParameterInput)
	case "SessionSubmit":
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
		if f.LoopModeAfterSubmit {
			f.advance(plant, kind, SessionWaitLoop)
			f.loopPending = append(f.loopPending, sessionKey(plant, kind))
			return
		}
		f.advance(plant, kind, SessionWaitEnd)
		if f.AutoFreeSessions {
			f.freePending = append(f.freePending, sessionKey(plant, kind))
		}
	}
}

// parseItem splits a plant item name into the plant it addresses, the branch it
// sits in and its leaf name.
//
// It panics on a name it cannot parse. The fake used to pick these apart with
// four separate unchecked fmt.Sscanf calls, which left the plant number at 0 for
// anything unexpected — so the fake would happily answer for plant 0 and the
// test would pass for the wrong reason. In a test double a name that does not
// parse is a bug in the test, not a condition worth modelling.
func parseItem(name string) (plant uint8, kind SessionKind, leaf string) {
	rest, ok := strings.CutPrefix(name, "Loc/Wec/Plant")
	if !ok {
		panic("fakeOPC: not a plant item: " + name)
	}
	number, rest, ok := strings.Cut(rest, "/")
	if !ok {
		panic("fakeOPC: item name has no branch: " + name)
	}
	n, err := strconv.ParseUint(number, 10, 8)
	if err != nil {
		panic("fakeOPC: unparseable plant number in " + name + ": " + err.Error())
	}
	branch, leaf, ok := strings.Cut(rest, "/")
	if !ok {
		panic("fakeOPC: item name has no leaf: " + name)
	}
	return uint8(n), SessionKind(branch), leaf
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

func (f *fakeOPC) Browse(ctx context.Context, itemPath string, handle *string, _ string,
	options gopcxmlda.TBrowseOptions) (gopcxmlda.TBrowse, error) {
	if f.OnBrowse != nil {
		f.OnBrowse(itemPath)
	}
	if err := ctx.Err(); err != nil {
		return gopcxmlda.TBrowse{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.BrowseCalls++
	if err, ok := f.BrowseErrOn[itemPath]; ok {
		return gopcxmlda.TBrowse{}, err
	}
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
		for _, name := range f.ExtraBranches {
			b.Response.Elements = append(b.Response.Elements, gopcxmlda.TBrowseElement{
				Name:        strings.TrimPrefix(name, "Loc/Wec/"),
				ItemName:    name,
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
