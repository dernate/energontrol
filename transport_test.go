package energontrol

// The in-memory Transport the protocol tests run against, and the item names
// of the default address space.
//
// It models the Enercon session state machine faithfully enough to drive the
// whole package, and exposes knobs for the failure modes a real SCADA can
// produce. Being able to write it at all is the point of the port: in v1 none
// of the protocol logic could be exercised without a live SCADA, so the tests
// commanded real turbines.
//
// It sits above the OPC XML-DA wire format on purpose. Everything below the
// port belongs to the adapter and is tested against a real HTTP server in
// opcxmlda/transport_test.go.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fakeOPC is an in-memory Transport. It models the Enercon session state
// machine faithfully enough to drive the whole package, and exposes knobs for
// the failure modes a real SCADA can produce: items with a fault or bad
// quality, items missing from a response, values of an unusable shape, and
// sessions that refuse to advance.
//
// Being able to write this at all is the point of the Transport port: in v1
// none of the protocol logic could be exercised without a live SCADA, so the
// tests commanded real turbines.
//
// It sits above the OPC XML-DA wire format on purpose. Everything below the
// port — response correlation, the distinction between an error about the
// request and an error about one item, value decoding, browse paging — belongs
// to the gopcxmlda adapter and is tested against a real HTTP server in
// opcxmlda/transport_test.go. Testing only at this level is what let the
// item-level error escalation survive 130 tests.
type fakeOPC struct {
	mu sync.Mutex

	// Root is the address-space root the fake serves under, "Loc" by default.
	// Its internal bookkeeping stays in default-rooted names, so a test can go
	// on using the package's own item-name builders whatever root is set.
	Root string

	ServerState string           // default "running"
	Ctrl        map[uint8]uint64 // Loc/Wec/Plant<n>/Ctrl/Ctrl
	Rbh         map[uint8]uint64 // Loc/Wec/Plant<n>/Ctrl/Rbh
	IceDet      map[uint8]uint64 // Loc/Wec/Plant<n>/Ctrl/IceDet
	ParkNo      uint64           //
	PubKey      uint64           // default 4711

	// Branches is the browse result per plant: the branch and Set… element
	// names the plant offers.
	Branches map[uint8][]string

	// Failure injection, keyed by item name unless noted otherwise.
	ItemFault  map[string]string      // ResultID on a read item
	Quality    map[string]string      // e.g. "bad", "uncertain"
	ValueErr   map[string]error       // the transport could not decode the value
	RawValues  map[string][]uint64    // override the values an item reports
	Omit       map[string]bool        // drop the item from the response
	WriteBad   map[string]string      // ResultID on a written item
	StuckAt    map[uint8]SessionState // session never advances past this state
	Occupied   map[uint8]bool         // session reads as "occupied"
	SessionID  map[uint8]uint64       // session id the server reports back
	SessionTO  uint64                 // seconds left, reported by SessionTimeOut
	ReadErr    error                  // fail every Read
	WriteErr   error                  // fail every Write
	StatusErr  error                  // fail every Status
	Timestamps bool                   // stamp response items with the current time

	// Reverse returns the response items in reverse order. A Transport makes no
	// promise about order, so nothing above it may depend on one.
	Reverse bool
	// ExtraItem is an item name a Read answers for although it was not
	// requested, and DuplicateItem one it answers for twice. Both are breaches
	// of the Transport contract that must be caught rather than attributed to
	// whichever plant happens to match.
	ExtraItem     string
	DuplicateItem string

	OmitWrite  map[string]bool  // apply the write but drop the item from the response
	ReadErrOn  map[string]error // fail a Read that requests an item with this name
	WriteErrOn map[string]error // fail a Write that carries an item with this name

	// ResponseServerState is the ServerState reported on Read and Write
	// responses. Empty means "the same as ServerState", which is what a healthy
	// server does; setting it models a server that degrades after GetStatus.
	ResponseServerState string

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
	// ExtraNodes are additional browse elements of Loc/Wec, given in full, for
	// the cases where the Name and the ItemName have to differ.
	ExtraNodes []Node
	// BrowseErrOn fails a browse of exactly this item path.
	BrowseErrOn map[string]error
	// OnBrowse is called for every browsed path, before the fake takes its own
	// lock, so a test can order concurrent browses against each other.
	OnBrowse func(itemPath string)

	// Observations.
	Writes      []WriteRecord
	ReadNames   []string
	ReadMaxAge  []time.Duration
	ReadCalls   int
	WriteCalls  int
	BrowseCalls int

	session     map[string]SessionState // "<plant>/<kind>"
	sessionID   map[uint8]uint64        // session id the client reserved with
	staged      map[string][]uint32     // value staged in a session, by item name
	loopPending []string                // sessions in loop mode, promoted on the next read
	freePending []string                // finished sessions, released on the next read
}

// Compile-time proof that the fake is a Transport.
var _ Transport = (*fakeOPC)(nil)

// WriteRecord is one item written by the package under test.
type WriteRecord struct {
	ItemName string
	Value    []uint32
}

func newFakeOPC() *fakeOPC {
	return &fakeOPC{
		Root:        fakeDefaultRoot,
		ServerState: "running",
		Ctrl:        map[uint8]uint64{},
		Rbh:         map[uint8]uint64{},
		IceDet:      map[uint8]uint64{},
		PubKey:      4711,
		ParkNo:      1234,
		ItemFault:   map[string]string{},
		Quality:     map[string]string{},
		ValueErr:    map[string]error{},
		RawValues:   map[string][]uint64{},
		Omit:        map[string]bool{},
		WriteBad:    map[string]string{},
		StuckAt:     map[uint8]SessionState{},
		Occupied:    map[uint8]bool{},
		Branches:    map[uint8][]string{},
		SessionID:   map[uint8]uint64{},
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

// fakeDefaultRoot is the root the package addresses by default.
const fakeDefaultRoot = defaultItemRoot

// The item names of the default address space, as free functions, so a test can
// name an item without holding a Client. Production code reaches them through
// Client.items, which carries the configured root.
var defaultNamer = itemNamer{root: defaultItemRoot}

var parkNoItem = defaultNamer.parkNo()

func plantBranchPath(plant uint8) string { return defaultNamer.plantBranch(plant) }

func ctrlItem(plant uint8) string { return defaultNamer.ctrl(plant) }

func rbhItem(plant uint8) string { return defaultNamer.rbh(plant) }

func setCtrlItem(plant uint8) string { return defaultNamer.setCtrl(plant) }

func setRbhItem(plant uint8) string { return defaultNamer.setRbh(plant) }

func setIceDetItem(plant uint8) string { return defaultNamer.setIceDet(plant) }

func setResetItem(plant uint8) string { return defaultNamer.setReset(plant) }

func sessionStateItem(plant uint8, kind SessionKind) string {
	return defaultNamer.sessionState(plant, kind)
}

func sessionRequestItem(plant uint8, kind SessionKind) string {
	return defaultNamer.sessionRequest(plant, kind)
}

func sessionPubKeyItem(plant uint8, kind SessionKind) string {
	return defaultNamer.sessionPubKey(plant, kind)
}

func sessionSubmitItem(plant uint8, kind SessionKind) string {
	return defaultNamer.sessionSubmit(plant, kind)
}

func sessionTimeoutItem(plant uint8, kind SessionKind) string {
	return defaultNamer.sessionTimeout(plant, kind)
}

// item maps a default-rooted item name or browse path onto the fake's own root,
// so a test can keep using the package's name builders whatever root it serves.
func (f *fakeOPC) item(name string) string {
	if f.Root == "" || f.Root == fakeDefaultRoot {
		return name
	}
	return f.Root + strings.TrimPrefix(name, fakeDefaultRoot)
}

// strip is the inverse of item: everything the fake stores internally is keyed
// by the default-rooted name. An item name outside the served root is reported
// as not served, the way a real server has no answer for it — answering it
// anyway would hide exactly the mistake a configurable root exists to prevent.
func (f *fakeOPC) strip(name string) (string, bool) {
	root := f.Root
	if root == "" {
		root = fakeDefaultRoot
	}
	rest, ok := strings.CutPrefix(name, root+"/")
	if !ok {
		return name, false
	}
	return fakeDefaultRoot + "/" + rest, true
}

// stripped is strip for the lookups where a name outside the root simply finds
// nothing.
func (f *fakeOPC) stripped(name string) string {
	out, _ := f.strip(name)
	return out
}

// TimesRead reports how often an item name was requested by a read.
func (f *fakeOPC) TimesRead(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, got := range f.ReadNames {
		if got == name {
			n++
		}
	}
	return n
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

func (f *fakeOPC) Status(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.StatusErr != nil {
		return "", f.StatusErr
	}
	return f.ServerState, nil
}

func (f *fakeOPC) Read(ctx context.Context, names []string, opts ReadOptions) (Response, error) {
	// A real transport fails a cancelled request rather than serving it, and
	// the cleanup path depends on that being true.
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ReadCalls++
	f.ReadMaxAge = append(f.ReadMaxAge, opts.MaxAge)
	if f.ReadErr != nil {
		return Response{}, f.ReadErr
	}
	for _, name := range names {
		if err, ok := f.ReadErrOn[f.stripped(name)]; ok {
			return Response{}, err
		}
	}

	f.ReadNames = append(f.ReadNames, names...)

	resp := Response{ServerState: f.responseServerState()}
	for _, requested := range names {
		name, served := f.strip(requested)
		if !served || f.Omit[name] {
			continue
		}
		item := ItemResult{Name: requested}
		if f.Timestamps {
			item.Timestamp = time.Now()
		}
		if rid, ok := f.ItemFault[name]; ok {
			item.ResultID = rid
			resp.Items = append(resp.Items, item)
			continue
		}
		item.Quality = f.qualityOf(name)
		if err, ok := f.ValueErr[name]; ok {
			item.Err = err
			resp.Items = append(resp.Items, item)
			continue
		}
		item.Values = f.valuesOf(name)
		resp.Items = append(resp.Items, item)
		if f.DuplicateItem == name {
			resp.Items = append(resp.Items, item)
		}
	}
	if f.ExtraItem != "" {
		resp.Items = append(resp.Items, ItemResult{
			Name: f.ExtraItem, Quality: "good", Values: []uint64{0},
		})
	}
	if f.Reverse {
		for i, j := 0, len(resp.Items)-1; i < j; i, j = i+1, j-1 {
			resp.Items[i], resp.Items[j] = resp.Items[j], resp.Items[i]
		}
	}
	return resp, nil
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

// valuesOf answers a read for one item from the simulated plant state. A
// scalar comes back as a one-element slice, which is how a Transport reports
// one.
func (f *fakeOPC) valuesOf(name string) []uint64 {
	if v, ok := f.RawValues[name]; ok {
		return v
	}
	if arr, ok := f.arrayValueOf(name); ok {
		return arr
	}
	return []uint64{f.scalarValueOf(name)}
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

// scalarValueOf answers a read for one scalar item.
func (f *fakeOPC) scalarValueOf(name string) uint64 {
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

func (f *fakeOPC) Write(ctx context.Context, items []ItemWrite) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if f.OnWrite != nil {
		for _, it := range items {
			f.OnWrite(it.Name)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.WriteCalls++
	if f.WriteErr != nil {
		return Response{}, f.WriteErr
	}
	for _, it := range items {
		if err, ok := f.WriteErrOn[f.stripped(it.Name)]; ok {
			return Response{}, err
		}
	}

	resp := Response{ServerState: f.responseServerState()}
	for _, it := range items {
		name, served := f.strip(it.Name)
		item := ItemResult{Name: it.Name}
		if rid, ok := f.WriteBad[name]; ok {
			item.ResultID = rid
			resp.Items = append(resp.Items, item)
			continue
		}
		if !served {
			continue
		}
		f.Writes = append(f.Writes, WriteRecord{ItemName: name, Value: it.Value})
		f.applyWrite(name, it.Value)
		if f.OmitWrite[name] {
			// The write was applied, but the server does not echo the item —
			// the response carries no confirmation for it either way.
			continue
		}
		resp.Items = append(resp.Items, item)
	}
	return resp, nil
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
	rest, ok := strings.CutPrefix(name, fakeDefaultRoot+"/Wec/Plant")
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

func (f *fakeOPC) Browse(ctx context.Context, path string, filter BrowseFilter) ([]Node, error) {
	if f.OnBrowse != nil {
		f.OnBrowse(path)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.BrowseCalls++
	if err, ok := f.BrowseErrOn[f.stripped(path)]; ok {
		return nil, err
	}
	path, served := f.strip(path)
	if !served {
		return nil, nil
	}

	if path == fakeDefaultRoot+"/Wec" {
		plants := make([]uint8, 0, len(f.Branches))
		for p := range f.Branches {
			plants = append(plants, p)
		}
		sort.Slice(plants, func(i, j int) bool { return plants[i] < plants[j] })
		var out []Node
		for _, p := range plants {
			out = append(out, Node{
				Name:        fmt.Sprintf("Plant%d", p),
				ItemName:    f.item(plantBranchPath(p)),
				HasChildren: true,
			})
		}
		for _, name := range f.ExtraBranches {
			out = append(out, Node{
				Name:        strings.TrimPrefix(name, fakeDefaultRoot+"/Wec/"),
				ItemName:    f.item(name),
				HasChildren: true,
			})
		}
		out = append(out, f.ExtraNodes...)
		return out, nil
	}

	var plant uint8
	if _, err := fmt.Sscanf(path, fakeDefaultRoot+"/Wec/Plant%d", &plant); err != nil {
		return nil, nil
	}
	names := f.Branches[plant]
	if strings.HasSuffix(path, "/"+string(SessionCtrl)) ||
		strings.HasSuffix(path, "/"+string(SessionReset)) {
		var out []Node
		for _, n := range names {
			if !strings.HasPrefix(n, "Set") {
				continue
			}
			if filter.NamePattern == "SetReset" && n != "SetReset" {
				continue
			}
			out = append(out, Node{Name: n, ItemName: f.item(path + "/" + n), IsItem: true})
		}
		return out, nil
	}
	var out []Node
	for _, n := range names {
		if strings.HasPrefix(n, "Set") {
			continue
		}
		out = append(out, Node{Name: n, ItemName: f.item(path + "/" + n), HasChildren: true})
	}
	return out, nil
}

var errTransport = errors.New("connection reset by peer")

const testUser = uint64(1234)
