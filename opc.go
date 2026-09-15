package energontrol

import (
	"context"
	"fmt"
	"math"
	"strings"
)

// defaultItemRoot is the root of the address space the ENERCON technical data
// sheet documents: the park number sits at Loc/LocNo, the plants below Loc/Wec.
const defaultItemRoot = "Loc"

// itemNamer builds the OPC item names of one address space.
//
// Keeping every name in one place makes the address space of the SCADA visible
// at a glance and keeps the format strings out of the logic. Carrying the root
// as a field rather than hardcoding it is what lets WithItemRoot exist: the
// separator and the branch names below the root are Enercon's own and follow
// the data sheet, but which branch a park hangs off is an installation's
// choice, and a package that assumes one answers nothing on an installation
// that made the other.
type itemNamer struct{ root string }

// plantElementName is the browse element name of a plant, and therefore also
// the last path segment of every item name below it.
func plantElementName(plant uint8) string { return fmt.Sprintf("Plant%d", plant) }

func (n itemNamer) parkNo() string     { return n.root + "/LocNo" }
func (n itemNamer) parkBranch() string { return n.root + "/Wec" }

func (n itemNamer) plantBranch(plant uint8) string {
	return n.parkBranch() + "/" + plantElementName(plant)
}

func (n itemNamer) plantSubBranch(plant uint8, kind SessionKind) string {
	return fmt.Sprintf("%s/%s", n.plantBranch(plant), kind)
}

func (n itemNamer) ctrl(plant uint8) string {
	return n.plantSubBranch(plant, SessionCtrl) + "/Ctrl"
}
func (n itemNamer) rbh(plant uint8) string {
	return n.plantSubBranch(plant, SessionCtrl) + "/Rbh"
}
func (n itemNamer) iceDet(plant uint8) string {
	return n.plantSubBranch(plant, SessionCtrl) + "/IceDet"
}
func (n itemNamer) setCtrl(plant uint8) string {
	return n.plantSubBranch(plant, SessionCtrl) + "/SetCtrl"
}
func (n itemNamer) setRbh(plant uint8) string {
	return n.plantSubBranch(plant, SessionCtrl) + "/SetRbh"
}
func (n itemNamer) setIceDet(plant uint8) string {
	return n.plantSubBranch(plant, SessionCtrl) + "/SetIceDet"
}
func (n itemNamer) setReset(plant uint8) string {
	return n.plantSubBranch(plant, SessionReset) + "/SetReset"
}

func (n itemNamer) sessionState(plant uint8, kind SessionKind) string {
	return n.plantSubBranch(plant, kind) + "/SessionState"
}
func (n itemNamer) sessionRequest(plant uint8, kind SessionKind) string {
	return n.plantSubBranch(plant, kind) + "/SessionRequest"
}
func (n itemNamer) sessionPubKey(plant uint8, kind SessionKind) string {
	return n.plantSubBranch(plant, kind) + "/SessionPubKey"
}
func (n itemNamer) sessionSubmit(plant uint8, kind SessionKind) string {
	return n.plantSubBranch(plant, kind) + "/SessionSubmit"
}
func (n itemNamer) sessionTimeout(plant uint8, kind SessionKind) string {
	return n.plantSubBranch(plant, kind) + "/SessionTimeOut"
}

// itemValue is the per-item outcome of a batched read: either a value or the
// reason this particular item is unusable.
type itemValue struct {
	Value uint64
	Err   error
}

// readValues reads the named items in a single request and returns one entry
// per requested name.
//
// The returned error is non-nil only for failures that affect the whole request
// — transport, SOAP fault, a server that is not running, or a response that
// cannot be correlated. A problem with a single item (missing, faulted, bad
// quality, unexpected type) is reported in that item's entry, so one broken
// plant does not fail a command for a whole park.
func (c *Client) readValues(ctx context.Context, names []string) (map[string]itemValue, error) {
	results, err := c.readItems(ctx, names)
	if err != nil {
		return nil, err
	}
	out := make(map[string]itemValue, len(names))
	for _, n := range names {
		r, ok := results[n]
		if !ok {
			out[n] = itemValue{Err: &ItemError{ItemName: n, Reason: ErrItemMissing}}
			continue
		}
		v, err := c.scalarOf(r)
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

// readArrays reads array items in a single request, with the same validation
// and correlation rules as readValues.
func (c *Client) readArrays(ctx context.Context, names []string) (map[string]arrayValue, error) {
	results, err := c.readItems(ctx, names)
	if err != nil {
		return nil, err
	}
	out := make(map[string]arrayValue, len(names))
	for _, n := range names {
		r, ok := results[n]
		if !ok {
			out[n] = arrayValue{Err: &ItemError{ItemName: n, Reason: ErrItemMissing}}
			continue
		}
		if err := c.usable(r); err != nil {
			out[n] = arrayValue{Err: err}
			continue
		}
		out[n] = arrayValue{Values: r.Values}
	}
	return out, nil
}

// readItems performs one batched read, honours the ServerState the response
// carries, and indexes the results by item name.
func (c *Client) readItems(ctx context.Context, names []string) (map[string]ItemResult, error) {
	if len(names) == 0 {
		return map[string]ItemResult{}, nil
	}
	// Every read this package makes carries the same freshness requirement, the
	// session items included — a stale SessionState is the most dangerous of
	// them all.
	resp, err := c.transport.Read(ctx, names, ReadOptions{MaxAge: c.maxStateAge})
	if err != nil {
		return nil, err
	}
	if err := requireRunning(resp.ServerState); err != nil {
		return nil, err
	}
	return resultsByName(names, resp.Items)
}

// resultsByName indexes a transport's results by item name.
//
// It is this package's own check on the Transport contract: at most one result
// per requested name, and nothing that was not requested. The adapter shipped
// here correlates responses properly, but a Transport is part of the public API
// and a caller may supply one — and a transport that gets this wrong would
// otherwise attribute one plant's value to another, which is the failure this
// package refuses to be capable of.
func resultsByName(names []string, items []ItemResult) (map[string]ItemResult, error) {
	requested := make(map[string]struct{}, len(names))
	for _, n := range names {
		requested[n] = struct{}{}
	}
	out := make(map[string]ItemResult, len(items))
	for _, it := range items {
		if _, ok := requested[it.Name]; !ok {
			return nil, fmt.Errorf("%w: the transport returned item %q, which was not requested",
				ErrUncorrelatable, it.Name)
		}
		if _, dup := out[it.Name]; dup {
			return nil, fmt.Errorf("%w: item %q returned more than once", ErrUncorrelatable, it.Name)
		}
		out[it.Name] = it
	}
	return out, nil
}

// scalarOf validates one result and returns its value as a single number.
//
// An item that carries no value, or more than one, is rejected rather than
// read leniently: every item read this way is a scalar on an Enercon SCADA, and
// a value of an unexpected shape means the item is not what this package
// thinks it is.
func (c *Client) scalarOf(r ItemResult) (uint64, error) {
	if err := c.usable(r); err != nil {
		return 0, err
	}
	if len(r.Values) != 1 {
		return 0, &ItemError{ItemName: r.Name, Reason: ErrUnexpectedType,
			Detail: fmt.Sprintf("expected a single value, got %d", len(r.Values))}
	}
	return r.Values[0], nil
}

// usable applies the checks that must pass before an item's value may be used
// at all: the transport could decode it, the server reported no fault for it,
// its quality permits use, and it is not stale.
//
// v1 read a value straight through an unchecked type assertion and ignored both
// the item's ResultID and its quality, so a value the server had explicitly
// marked as bad was used as a process value — and a value of an unexpected OPC
// type panicked the calling application.
func (c *Client) usable(r ItemResult) error {
	if r.Err != nil {
		return r.Err
	}
	if r.ResultID != "" {
		return &ItemError{ItemName: r.Name, Reason: ErrItemFault, Detail: r.ResultID}
	}
	if !qualityUsable(r.Quality) {
		return &ItemError{ItemName: r.Name, Reason: ErrBadQuality, Detail: "quality=" + r.Quality}
	}
	if c.maxStateAge > 0 {
		// A maximum age is an explicit requirement, and it cannot be met by an
		// item whose age is unknown. Skipping the check here would disable the
		// caller's safety net precisely on the servers it was asked for: one
		// that answers from a cache is more likely, not less, to be one that
		// does not support ReturnItemTime.
		if r.Timestamp.IsZero() {
			return &ItemError{ItemName: r.Name, Reason: ErrNoItemTime}
		}
		if age := c.now().Sub(r.Timestamp); age > c.maxStateAge {
			return &ItemError{ItemName: r.Name, Reason: ErrStaleValue,
				Detail: fmt.Sprintf("age %s exceeds %s", age.Round(0), c.maxStateAge)}
		}
	}
	return nil
}

// qualityUsable reports whether an OPC quality field permits using the value.
// An empty field means the server did not report quality, which the
// specification defines as good. Everything in the "good…" family is usable;
// "uncertain…" and "bad…" are not.
func qualityUsable(q string) bool {
	return q == "" || strings.HasPrefix(q, "good")
}

// requireRunning checks the ServerState that OPC XML-DA carries in the reply
// base of every response.
//
// Checking it once with GetStatus before a command is a time-of-check to
// time-of-use test: a command takes several requests, and a server that goes to
// "failed" or "suspended" in between would otherwise keep receiving control
// commands and keep having its values used as process values. An empty state is
// tolerated, because not every server fills the attribute — that is the absence
// of a statement, not a statement of failure.
func requireRunning(state string) error {
	if s := strings.TrimSpace(state); s != "" && s != serverStateRunning {
		return &serverStateError{state: s}
	}
	return nil
}

// serverStateRunning is the only ServerState in which a server may be given a
// control command.
const serverStateRunning = "running"

// longWord narrows a value to the 32 bits an Enercon long word holds.
func longWord(v uint64, what string) (uint32, error) {
	if v > math.MaxUint32 {
		return 0, fmt.Errorf("%w: %s %d does not fit in a long word", ErrInvalidValue, what, v)
	}
	return uint32(v), nil
}

// writeValues writes the given items in a single request and returns one entry
// per item name: nil if the server confirmed it, otherwise the reason.
//
// An item the server reports on is checked for its ResultID. An item the server
// does not mention at all is *not* confirmed, and an unconfirmed write is not a
// write: gopcxmlda sends ReturnValuesOnReply, so the response is expected to
// carry one item per written item, and a missing one is a gap in the evidence
// rather than silent consent. This mirrors the rule reads follow — a missing
// item is an error, not a value.
//
// The earlier behaviour treated a missing item as accepted, on the grounds that
// a top-level fault would have surfaced as the returned error. That argument
// does not cover the case it needs to: a server that answers successfully but
// omits one item from the list. WithLenientWriteConfirmation restores it for
// servers that genuinely do not echo written items.
func (c *Client) writeValues(ctx context.Context, items []ItemWrite) (map[string]error, error) {
	if len(items) == 0 {
		return map[string]error{}, nil
	}
	names := make([]string, len(items))
	for i, it := range items {
		names[i] = it.Name
	}
	resp, err := c.transport.Write(ctx, items)
	if err != nil {
		return nil, err
	}
	if err := requireRunning(resp.ServerState); err != nil {
		return nil, err
	}
	results, err := resultsByName(names, resp.Items)
	if err != nil {
		return nil, err
	}
	out := make(map[string]error, len(names))
	for _, n := range names {
		r, ok := results[n]
		switch {
		case !ok && c.lenientWriteConfirmation:
			out[n] = nil
		case !ok:
			out[n] = &ItemError{ItemName: n, Reason: ErrItemMissing,
				Detail: "the server did not confirm the write"}
		case r.Err != nil:
			out[n] = r.Err
		case r.ResultID != "":
			out[n] = &ItemError{ItemName: n, Reason: ErrItemFault, Detail: r.ResultID}
		default:
			out[n] = nil
		}
	}
	return out, nil
}

// ctxErr reports a cancelled context. It is checked when a command is entered
// and once per polling round, so a cancelled context stops a command promptly
// instead of running to the end of its polling budget. The requests themselves
// carry the context and are cancelled by the transport.
func ctxErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}
