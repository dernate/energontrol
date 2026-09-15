// Package opcxmlda is the OPC XML-DA transport for
// github.com/dernate/energontrol/v2, built on github.com/dernate/gopcxmlda.
//
//	server := &gopcxmlda.Server{Url: u, LocaleID: "en-us", Timeout: 10 * time.Second}
//	client := energontrol.New(opcxmlda.New(server))
//
// It is a package of its own so that energontrol itself does not import
// gopcxmlda. That keeps the port honest by construction rather than by
// discipline: the interface this package satisfies cannot grow a field or a
// parameter of the client library's own types, because the core cannot name
// them. The predecessor of that port was built from exactly those types, and a
// pre-release audit found it leaking — a package boundary turns that mistake
// into a compile error.
//
// Three responsibilities live here, and nowhere else:
//
//   - Correlating a response with its request, by ClientItemHandle and by
//     ItemName, never by position.
//   - Following a paged browse to its end.
//   - Separating what a server said about a request from what it said about one
//     item, so a single faulted item does not fail a read for a whole park.
package opcxmlda

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/dernate/energontrol/v2"
	"github.com/dernate/gopcxmlda"
)

// OPC XML-DA request option names. These are XML attribute names on the
// RequestOptions element and therefore case sensitive; v1 sent
// "returnItemName", which a conformant server ignores as an unknown attribute
// — which in turn made ItemName unavailable for correlating responses.
const (
	optReturnItemName  = "ReturnItemName"
	optReturnItemTime  = "ReturnItemTime"
	optReturnItemPath  = "ReturnItemPath"
	optReturnErrorText = "ReturnErrorText"
)

// readOptions builds the RequestOptions of a read, plus the list-level MaxAge.
//
// MaxAge is not a RequestOptions attribute — it belongs on the
// ReadRequestItemList — and gopcxmlda takes it out of this map and renders it
// there. It goes on the list rather than on each item because every item of one
// request carries the same freshness requirement, so one attribute says it
// once.
//
// A maxAge of zero omits the attribute: the specification reads 0 as "give me
// the most accurate data available", which is a demand, not the absence of one.
// Sending it for a caller who never asked would turn every read into a device
// read.
func readOptions(maxAge time.Duration) map[string]interface{} {
	opts := map[string]interface{}{
		optReturnItemName:  true,
		optReturnItemTime:  true,
		optReturnErrorText: true,
	}
	if maxAge > 0 {
		// Truncating division rounds towards the stricter requirement, and an
		// age below a millisecond becomes 0 — a device read, which is the
		// strictest thing the attribute can express and the honest reading of
		// "fresher than a millisecond".
		opts[gopcxmlda.MaxAgeOption] = int(maxAge / time.Millisecond)
	}
	return opts
}

func writeOptions() map[string]interface{} {
	return map[string]interface{}{
		optReturnItemName:  true,
		optReturnItemPath:  true,
		optReturnErrorText: true,
	}
}

// serverStateRunning is the only ServerState in which a server may be given a
// control command. The core checks it on every read and write reply; the
// adapter checks it on a browse, which does not pass through the core's item
// layer.
const serverStateRunning = "running"

// wrapf prefixes an error from the OPC layer with what the adapter was doing.
func wrapf(err error, format string, args ...any) error {
	return fmt.Errorf("energontrol: %s: %w", fmt.Sprintf(format, args...), err)
}

// maxBrowsePages bounds how many continuation points a single browse follows.
// A park holds 20 to 30 turbines, so any server needing more than this many
// pages is not paging, it is looping.
const maxBrowsePages = 64

// transport implements energontrol.Transport on top of a *gopcxmlda.Server.
//
// Everything that is specific to that library lives here: the request option
// maps, the ClientItemHandle bookkeeping, the conversion of an OPC value of
// whatever type the server chose into a number, the browse continuation loop,
// and the distinction between an error about the request and an error about a
// single item.
type transport struct {
	server *gopcxmlda.Server
}

// New adapts a *gopcxmlda.Server to an energontrol.Transport.
//
// Note the pointer: gopcxmlda.Server carries an http.Client and, from v1.2.0
// on, a sync.Mutex, so it must not be copied.
func New(server *gopcxmlda.Server) energontrol.Transport {
	return &transport{server: server}
}

// Compile-time proof that the adapter satisfies the port.
var _ energontrol.Transport = (*transport)(nil)

func (t *transport) Status(ctx context.Context) (string, error) {
	var handle string
	status, err := t.server.GetStatus(ctx, &handle, "")
	if err != nil {
		return "", wrapf(err, "GetStatus")
	}
	return strings.TrimSpace(status.Response.Result.ServerState), nil
}

func (t *transport) Read(ctx context.Context, names []string,
	opts energontrol.ReadOptions) (energontrol.Response, error) {

	if len(names) == 0 {
		return energontrol.Response{}, nil
	}
	items := make([]gopcxmlda.TItem, len(names))
	for i, n := range names {
		items[i] = gopcxmlda.TItem{ItemName: n}
	}
	var requestHandle string
	var itemHandles []string
	resp, err := t.server.Read(ctx, items, &requestHandle, &itemHandles, "", readOptions(opts.MaxAge))
	what := fmt.Sprintf("read %d item(s)", len(names))
	return t.response(resp.Response.Result.ServerState, resp.Response.Errors,
		names, itemHandles, resp.Response.ItemList.Items, err, what, true)
}

func (t *transport) Write(ctx context.Context, items []energontrol.ItemWrite) (energontrol.Response, error) {
	if len(items) == 0 {
		return energontrol.Response{}, nil
	}
	names := make([]string, len(items))
	opcItems := make([]gopcxmlda.TItem, len(items))
	for i, it := range items {
		names[i] = it.Name
		opcItems[i] = gopcxmlda.TItem{
			ItemName: it.Name,
			Value:    gopcxmlda.TValue{Value: it.Value},
		}
	}
	var requestHandle string
	var itemHandles []string
	resp, err := t.server.Write(ctx, opcItems, &requestHandle, &itemHandles, "", writeOptions())
	what := fmt.Sprintf("write %d item(s)", len(items))
	// A write reply confirms items; it is not obliged to echo their values, and
	// an item echoed without one is a confirmation rather than a value that
	// could not be decoded. Decoding it as one failed every command against a
	// server that confirms without echoing.
	return t.response(resp.Response.Result.ServerState, resp.Response.Errors,
		names, itemHandles, resp.Response.ItemList.Items, err, what, false)
}

// response turns one read or write reply into a Response, and decides whether
// the error gopcxmlda returned is a statement about the request or about the
// individual items.
//
// OPC XML-DA reports a problem with one item in that item's ResultID attribute
// and, when ReturnErrorText is requested — which this package does, and which
// is the specification's own default — adds an <Errors> element per distinct
// ResultID carrying its localised text. gopcxmlda surfaces that element as an
// *OpcResponseError joined into the returned error, so a response that is
// complete and perfectly usable arrives with a non-nil error as soon as a
// single item is faulted.
//
// Treating that as a request failure is what made this package's whole
// per-item error path unreachable against a conformant server: one unknown
// item name failed a read for an entire park, and inside a command it failed
// every plant of the batch and left their reserved sessions open for the
// server's 60 s timeout. So an error consisting of nothing but
// *OpcResponseError is the error-text table belonging to the items, and the
// items themselves are the authoritative statement.
//
// The tolerance is deliberately narrow. A transport failure, a SOAP fault and
// a response that could not be unmarshalled all remain request failures. And
// an <Errors> element that no returned item accounts for is not explained by
// the items, so it is not swallowed either.
//
// decodeValues says whether the items are expected to carry values. A read
// answers with them, and one that carries none is a read that failed. A write
// answers with confirmations: ReturnValuesOnReply invites a server to echo the
// value, but it does not oblige it to, and nothing above the port looks at a
// write reply's values. Decoding them anyway turned every such confirmation
// into ErrUnexpectedType, which failed every command against a server that
// confirms without echoing.
func (t *transport) response(serverState string, opcErrors gopcxmlda.OpcErrors,
	names, handles []string, items []gopcxmlda.TItem, err error, what string,
	decodeValues bool) (energontrol.Response, error) {

	tolerated := false
	if err != nil {
		if !itemLevelOnly(err) {
			return energontrol.Response{}, wrapf(err, "%s", what)
		}
		tolerated = true
	}

	byName, cerr := correlate(names, handles, items)
	if cerr != nil {
		return energontrol.Response{}, cerr
	}

	// The <Errors> element is keyed by ResultID rather than by item, and
	// gopcxmlda keeps one of them, so this maps back the text for the common
	// case of a single distinct fault code.
	text := ""
	if len(opcErrors.Text) > 0 {
		text = strings.TrimSpace(strings.Join(opcErrors.Text, "; "))
	}

	out := energontrol.Response{ServerState: strings.TrimSpace(serverState), Items: make([]energontrol.ItemResult, 0, len(byName))}
	faulted := 0
	for _, name := range names {
		it, ok := byName[name]
		if !ok {
			// The server did not answer for this item. Left out on purpose:
			// the layers above report it as missing, which is not the same
			// statement as an item the server answered for with a fault.
			continue
		}
		r := energontrol.ItemResult{
			Name:      name,
			Quality:   it.Quality.QualityField,
			Timestamp: it.Timestamp,
			ResultID:  strings.TrimSpace(it.Error),
		}
		if r.ResultID != "" {
			faulted++
			if text != "" && (opcErrors.Id == "" || strings.EqualFold(opcErrors.Id, r.ResultID)) {
				r.ResultID += " (" + text + ")"
			}
			out.Items = append(out.Items, r)
			continue
		}
		if decodeValues {
			values, verr := toUint64Slice(it.Value.Value)
			if verr != nil {
				r.Err = &energontrol.ItemError{ItemName: name,
					Reason: energontrol.ErrUnexpectedType, Detail: verr.Error()}
			}
			r.Values = values
		}
		out.Items = append(out.Items, r)
	}

	if tolerated && faulted == 0 {
		// The server reported an error, no item carries a ResultID, and there
		// is therefore nothing in the response that accounts for it. An
		// unexplained error is not an item-level one.
		return energontrol.Response{}, wrapf(err, "%s", what)
	}
	return out, nil
}

// itemLevelOnly reports whether err says nothing about the request as a whole
// — that is, whether it consists of nothing but the *OpcResponseError that
// gopcxmlda builds from a response's <Errors> element.
func itemLevelOnly(err error) bool {
	leaves := flattenErr(err)
	if len(leaves) == 0 {
		return false
	}
	for _, e := range leaves {
		var opcErr *gopcxmlda.OpcResponseError
		if !errors.As(e, &opcErr) {
			return false
		}
	}
	return true
}

// flattenErr walks an error tree built by errors.Join and returns its leaves,
// so each one can be classified on its own. errors.As alone would report "yes,
// an *OpcResponseError is in there" for a join that also carries a SOAP fault.
func flattenErr(err error) []error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, e := range joined.Unwrap() {
			out = append(out, flattenErr(e)...)
		}
		return out
	}
	return []error{err}
}

func (t *transport) Browse(ctx context.Context, path string,
	filter energontrol.BrowseFilter) ([]energontrol.Node, error) {

	opts := gopcxmlda.TBrowseOptions{ElementNameFilter: filter.NamePattern}
	if filter.BranchesOnly {
		opts.BrowseFilter = "branch"
	}

	var out []energontrol.Node
	for page := 0; ; page++ {
		if page >= maxBrowsePages {
			return nil, fmt.Errorf("%w: browse of %q did not terminate after %d pages",
				energontrol.ErrBrowseIncomplete, path, maxBrowsePages)
		}
		// Every request gets its own request handle: reusing one across calls
		// made the request count and the handle count drift apart, which
		// gopcxmlda rejects.
		var handle string
		resp, err := t.server.Browse(ctx, path, &handle, "", opts)
		if err != nil {
			return nil, wrapf(err, "browse %s", path)
		}
		if state := strings.TrimSpace(resp.Response.Result.ServerState); state != "" &&
			state != serverStateRunning {
			return nil, fmt.Errorf("%w: the server is in state %q, expected %q",
				energontrol.ErrServerNotRunning, state, serverStateRunning)
		}
		for _, e := range resp.Response.Elements {
			out = append(out, energontrol.Node{
				Name:        e.Name,
				ItemName:    e.ItemName,
				HasChildren: e.HasChildren,
				IsItem:      e.IsItem,
			})
		}
		// MoreElements is an XML attribute that gopcxmlda keeps as a string,
		// so it cannot be tested as a bool.
		if !strings.EqualFold(strings.TrimSpace(resp.Response.MoreElements), "true") {
			return out, nil
		}
		point := strings.TrimSpace(resp.Response.ContinuationPoint)
		if point == "" {
			// More elements announced and no way to fetch them. An incomplete
			// park listing must not pass for a complete one: a plant that is
			// not in the listing is never commanded and never monitored.
			return nil, fmt.Errorf("%w: %q reports more elements without a continuation point",
				energontrol.ErrBrowseIncomplete, path)
		}
		if point == opts.ContinuationPoint {
			return nil, fmt.Errorf("%w: %q repeated its continuation point",
				energontrol.ErrBrowseIncomplete, path)
		}
		opts.ContinuationPoint = point
	}
}

// correlate matches response items to the requested item names.
//
// Matching is by ClientItemHandle first — the mechanism OPC XML-DA defines for
// exactly this purpose — and by ItemName second. Position is never used: the
// specification does not guarantee that a response lists items in request
// order, and a positional mismatch would apply a command to the wrong turbine.
// v1 matched by position throughout, so a response that omitted one item
// silently produced CtrlState 0 ("running") for a plant whose state was in
// fact unknown.
//
// Where the server supplies both a handle and an item name, the two must name
// the same item. The handle is authoritative per specification, so a
// contradiction is a server fault — but filing the item under the handle
// anyway is the very mistake positional matching was rejected for, only
// harder to see. ReturnItemName is requested precisely so that the name is
// available, which makes the cross-check free.
func correlate(names, handles []string, items []gopcxmlda.TItem) (map[string]gopcxmlda.TItem, error) {
	handleToName := make(map[string]string, len(handles))
	for i, h := range handles {
		if h != "" && i < len(names) {
			handleToName[h] = names[i]
		}
	}
	requested := make(map[string]struct{}, len(names))
	for _, n := range names {
		requested[n] = struct{}{}
	}
	out := make(map[string]gopcxmlda.TItem, len(items))
	for _, it := range items {
		name, ok := handleToName[it.ClientItemHandle]
		if ok {
			if it.ItemName != "" && it.ItemName != name {
				return nil, fmt.Errorf(
					"%w: ClientItemHandle=%q belongs to %q but the server named it %q",
					energontrol.ErrUncorrelatable, it.ClientItemHandle, name, it.ItemName)
			}
		} else {
			if _, isRequested := requested[it.ItemName]; !isRequested {
				return nil, fmt.Errorf("%w: ClientItemHandle=%q ItemName=%q",
					energontrol.ErrUncorrelatable, it.ClientItemHandle, it.ItemName)
			}
			name = it.ItemName
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("%w: item %q returned more than once", energontrol.ErrUncorrelatable, name)
		}
		out[name] = it
	}
	return out, nil
}

// toUint64Slice accepts the shapes gopcxmlda produces for an item value and
// normalises them to a slice of unsigned integers.
//
// A bare scalar becomes a one-element slice: a server is free to type a single
// value as a scalar rather than as an array, and the read-back checks only
// look at the leading elements. Rejecting that shape would turn a server's
// encoding choice into an unverifiable session.
func toUint64Slice(v any) ([]uint64, error) {
	switch a := v.(type) {
	case []interface{}:
		out := make([]uint64, 0, len(a))
		for i, e := range a {
			n, err := toUint64(e)
			if err != nil {
				return nil, fmt.Errorf("element %d: %w", i, err)
			}
			out = append(out, n)
		}
		return out, nil
	case []uint64:
		return a, nil
	case nil:
		return nil, fmt.Errorf("item carries no value")
	default:
		n, err := toUint64(v)
		if err != nil {
			return nil, fmt.Errorf("cannot use %T as an array of unsigned integers", v)
		}
		return []uint64{n}, nil
	}
}

// toUint64 accepts every numeric type gopcxmlda can decode from an OPC value.
// Which Go type an item's value carries depends on the xsi:type the server
// chose, not on what the client expects, so pinning it to one type — as v1 did
// with .(uint64) and .(uint16) — makes the caller's process depend on a server
// implementation detail.
func toUint64(v any) (uint64, error) {
	switch n := v.(type) {
	case uint64:
		return n, nil
	case uint32:
		return uint64(n), nil
	case uint16:
		return uint64(n), nil
	case uint8:
		return uint64(n), nil
	case uint:
		return uint64(n), nil
	case int:
		return fromInt64(int64(n))
	case int64:
		return fromInt64(n)
	case int32:
		return fromInt64(int64(n))
	case int16:
		return fromInt64(int64(n))
	case int8:
		return fromInt64(int64(n))
	case float64:
		return fromFloat64(n)
	case float32:
		return fromFloat64(float64(n))
	case nil:
		return 0, fmt.Errorf("item carries no value")
	default:
		return 0, fmt.Errorf("cannot use %T as an unsigned integer", v)
	}
}

func fromInt64(n int64) (uint64, error) {
	if n < 0 {
		return 0, fmt.Errorf("negative value %d", n)
	}
	return uint64(n), nil
}

func fromFloat64(f float64) (uint64, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("value %v is not a number", f)
	}
	if f < 0 || f > math.MaxUint64 || f != math.Trunc(f) {
		return 0, fmt.Errorf("value %v is not a non-negative integer", f)
	}
	return uint64(f), nil
}
