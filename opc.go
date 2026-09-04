package energontrol

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/dernate/gopcxmlda"
)

// OPC XML-DA request option names. These are XML attribute names and therefore
// case sensitive; v1 sent "returnItemName", which a conformant server ignores as
// an unknown attribute — which in turn made ItemName unavailable for correlating
// responses.
const (
	optReturnItemName  = "ReturnItemName"
	optReturnItemTime  = "ReturnItemTime"
	optReturnItemPath  = "ReturnItemPath"
	optReturnErrorText = "ReturnErrorText"
)

func readOptions() map[string]interface{} {
	return map[string]interface{}{
		optReturnItemName:  true,
		optReturnItemTime:  true,
		optReturnErrorText: true,
	}
}

func writeOptions() map[string]interface{} {
	return map[string]interface{}{
		optReturnItemName:  true,
		optReturnItemPath:  true,
		optReturnErrorText: true,
	}
}

// Item name builders. Keeping them in one place makes the address space of the
// SCADA visible at a glance and keeps the format strings out of the logic.

func ctrlItem(plant uint8) string { return fmt.Sprintf("Loc/Wec/Plant%d/Ctrl/Ctrl", plant) }
func rbhItem(plant uint8) string  { return fmt.Sprintf("Loc/Wec/Plant%d/Ctrl/Rbh", plant) }
func setCtrlItem(plant uint8) string {
	return fmt.Sprintf("Loc/Wec/Plant%d/Ctrl/SetCtrl", plant)
}
func setRbhItem(plant uint8) string { return fmt.Sprintf("Loc/Wec/Plant%d/Ctrl/SetRbh", plant) }
func iceDetItem(plant uint8) string { return fmt.Sprintf("Loc/Wec/Plant%d/Ctrl/IceDet", plant) }
func setIceDetItem(plant uint8) string {
	return fmt.Sprintf("Loc/Wec/Plant%d/Ctrl/SetIceDet", plant)
}
func setResetItem(plant uint8) string {
	return fmt.Sprintf("Loc/Wec/Plant%d/Reset/SetReset", plant)
}
func sessionStateItem(plant uint8, kind SessionKind) string {
	return fmt.Sprintf("Loc/Wec/Plant%d/%s/SessionState", plant, kind)
}
func sessionRequestItem(plant uint8, kind SessionKind) string {
	return fmt.Sprintf("Loc/Wec/Plant%d/%s/SessionRequest", plant, kind)
}
func sessionPubKeyItem(plant uint8, kind SessionKind) string {
	return fmt.Sprintf("Loc/Wec/Plant%d/%s/SessionPubKey", plant, kind)
}
func sessionSubmitItem(plant uint8, kind SessionKind) string {
	return fmt.Sprintf("Loc/Wec/Plant%d/%s/SessionSubmit", plant, kind)
}
func sessionTimeoutItem(plant uint8, kind SessionKind) string {
	return fmt.Sprintf("Loc/Wec/Plant%d/%s/SessionTimeOut", plant, kind)
}

const parkNoItem = "Loc/LocNo"

// itemValue is the per-item outcome of a batched read: either a value or the
// reason this particular item is unusable.
type itemValue struct {
	Value uint64
	Err   error
}

// readValues reads the named items in a single request and returns one entry per
// requested name.
//
// The returned error is non-nil only for failures that affect the whole request
// — transport, SOAP fault, or a response that cannot be correlated. A problem
// with a single item (missing, faulted, bad quality, unexpected type) is
// reported in that item's entry, so one broken plant does not fail a command for
// a whole park.
func (c *Client) readValues(ctx context.Context, names []string) (map[string]itemValue, error) {
	byName, err := c.readItems(ctx, names)
	if err != nil {
		return nil, err
	}
	out := make(map[string]itemValue, len(names))
	for _, n := range names {
		it, ok := byName[n]
		if !ok {
			out[n] = itemValue{Err: &ItemError{ItemName: n, Reason: ErrItemMissing}}
			continue
		}
		v, err := c.itemUint64(it, n)
		out[n] = itemValue{Value: v, Err: err}
	}
	return out, nil
}

// arrayValue is the per-item outcome of a batched read of array items such as
// SessionRequest, SetCtrl and SetRbh, which Enercon defines as arrays of long
// words.
type arrayValue struct {
	Values []uint64
	Err    error
}

// readArrays reads array items in a single request, with the same validation and
// correlation rules as readValues.
func (c *Client) readArrays(ctx context.Context, names []string) (map[string]arrayValue, error) {
	byName, err := c.readItems(ctx, names)
	if err != nil {
		return nil, err
	}
	out := make(map[string]arrayValue, len(names))
	for _, n := range names {
		it, ok := byName[n]
		if !ok {
			out[n] = arrayValue{Err: &ItemError{ItemName: n, Reason: ErrItemMissing}}
			continue
		}
		v, err := c.itemUint64Slice(it, n)
		out[n] = arrayValue{Values: v, Err: err}
	}
	return out, nil
}

// readItems performs one batched read and correlates the response.
func (c *Client) readItems(ctx context.Context, names []string) (map[string]gopcxmlda.TItem, error) {
	if len(names) == 0 {
		return map[string]gopcxmlda.TItem{}, nil
	}
	items := make([]gopcxmlda.TItem, len(names))
	for i, n := range names {
		items[i] = gopcxmlda.TItem{ItemName: n}
	}
	var requestHandle string
	var itemHandles []string
	resp, err := c.opc.Read(ctx, items, &requestHandle, &itemHandles, "", readOptions())
	if err != nil {
		return nil, wrapf(err, "read %d item(s)", len(names))
	}
	return correlate(names, itemHandles, resp.Response.ItemList.Items)
}

// writeItem is one item of a batched write.
//
// Enercon types every writable array item as an array of long words, so the
// elements are 32 bit. Writing them as []uint32 makes gopcxmlda emit
// ArrayOfUnsignedInt (xsd:unsignedInt, 32 bit); v1 wrote []uint64, which emits
// ArrayOfUnsignedLong (xsd:unsignedLong, 64 bit) and does not match the item.
type writeItem struct {
	Name  string
	Value []uint32
}

// longWord narrows a value to the 32 bits an Enercon long word holds.
func longWord(v uint64, what string) (uint32, error) {
	if v > math.MaxUint32 {
		return 0, fmt.Errorf("%w: %s %d does not fit in a long word", ErrInvalidValue, what, v)
	}
	return uint32(v), nil
}

// writeValues writes the given items in a single request and returns one entry
// per item name: nil if the server accepted it, otherwise the reason.
//
// Servers differ in how much they echo in a WriteResponse. An item the server
// reports on is checked for its ResultID; items the server does not mention are
// treated as accepted, because a top-level fault would already have surfaced as
// the returned error.
func (c *Client) writeValues(ctx context.Context, items []writeItem) (map[string]error, error) {
	if len(items) == 0 {
		return map[string]error{}, nil
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
	resp, err := c.opc.Write(ctx, opcItems, &requestHandle, &itemHandles, "", writeOptions())
	if err != nil {
		return nil, wrapf(err, "write %d item(s)", len(items))
	}
	byName, err := correlate(names, itemHandles, resp.Response.ItemList.Items)
	if err != nil {
		return nil, err
	}
	out := make(map[string]error, len(names))
	for _, n := range names {
		it, ok := byName[n]
		if !ok {
			out[n] = nil
			continue
		}
		if it.Error != "" {
			out[n] = &ItemError{ItemName: n, Reason: ErrItemFault, Detail: it.Error}
			continue
		}
		out[n] = nil
	}
	return out, nil
}

// correlate matches response items to the requested item names.
//
// Matching is by ClientItemHandle first — the mechanism OPC XML-DA defines for
// exactly this purpose — and by ItemName second. Position is never used: the
// specification does not guarantee that a response lists items in request order,
// and a positional mismatch would apply a command to the wrong turbine. v1
// matched by position throughout, so a response that omitted one item silently
// produced CtrlState 0 ("running") for a plant whose state was in fact unknown.
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
		if !ok {
			if _, isRequested := requested[it.ItemName]; isRequested {
				name = it.ItemName
			} else {
				return nil, fmt.Errorf("%w: ClientItemHandle=%q ItemName=%q",
					ErrUncorrelatable, it.ClientItemHandle, it.ItemName)
			}
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("%w: item %q returned more than once", ErrUncorrelatable, name)
		}
		out[name] = it
	}
	return out, nil
}

// itemUint64 validates a single response item and converts its value.
//
// v1 read the value straight through an unchecked type assertion and ignored
// both the item's ResultID and its quality, so a value the server had explicitly
// marked as bad was used as a process value — and a value of an unexpected OPC
// type panicked the calling application.
func (c *Client) itemUint64(it gopcxmlda.TItem, name string) (uint64, error) {
	if err := c.itemUsable(it, name); err != nil {
		return 0, err
	}
	v, err := toUint64(it.Value.Value)
	if err != nil {
		return 0, &ItemError{ItemName: name, Reason: ErrUnexpectedType, Detail: err.Error()}
	}
	return v, nil
}

// itemUsable applies the checks that must pass before an item's value may be
// used at all: no fault code, usable quality, and not stale.
func (c *Client) itemUsable(it gopcxmlda.TItem, name string) error {
	if it.Error != "" {
		return &ItemError{ItemName: name, Reason: ErrItemFault, Detail: it.Error}
	}
	if q := it.Quality.QualityField; !qualityUsable(q) {
		return &ItemError{ItemName: name, Reason: ErrBadQuality, Detail: "quality=" + q}
	}
	if c.maxStateAge > 0 && !it.Timestamp.IsZero() {
		if age := c.now().Sub(it.Timestamp); age > c.maxStateAge {
			return &ItemError{ItemName: name, Reason: ErrStaleValue,
				Detail: fmt.Sprintf("age %s exceeds %s", age.Round(0), c.maxStateAge)}
		}
	}
	return nil
}

// itemUint64Slice validates a response item and converts its array value.
func (c *Client) itemUint64Slice(it gopcxmlda.TItem, name string) ([]uint64, error) {
	if err := c.itemUsable(it, name); err != nil {
		return nil, err
	}
	v, err := toUint64Slice(it.Value.Value)
	if err != nil {
		return nil, &ItemError{ItemName: name, Reason: ErrUnexpectedType, Detail: err.Error()}
	}
	return v, nil
}

// toUint64Slice accepts the shapes gopcxmlda produces for an ArrayOf… value.
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
		return nil, fmt.Errorf("cannot use %T as an array of unsigned integers", v)
	}
}

// qualityUsable reports whether an OPC quality field permits using the value.
// An empty field means the server did not report quality, which the
// specification defines as good. Everything in the "good…" family is usable;
// "uncertain…" and "bad…" are not.
func qualityUsable(q string) bool {
	return q == "" || strings.HasPrefix(q, "good")
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

// wrapf prefixes an error from the OPC layer with what this package was doing.
func wrapf(err error, format string, args ...any) error {
	return fmt.Errorf("energontrol: %s: %w", fmt.Sprintf(format, args...), err)
}

// ctx is checked before every request so a cancelled context stops a command
// promptly instead of running to the end of a retry budget.
func ctxErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}
