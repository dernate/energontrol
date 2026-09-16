package energontrol

import (
	"context"
	"time"
)

// Transport is the OPC XML-DA layer this package sits on. It speaks item names
// and unsigned integers; nothing about SOAP, XML or a particular OPC client
// library reaches past it.
//
// The transport for an Enercon SCADA is in the opcxmlda subpackage:
// energontrol.New(opcxmlda.New(server)). It lives there rather than here so
// that this package does not import the OPC client library at all, which is
// what keeps the port honest by construction: an interface cannot leak types it
// cannot name. Its predecessor was built from those types and did leak.
//
// The port also makes the protocol logic above it testable without a network,
// lets a different OPC client be substituted without changing this package's
// API, and puts the rules for turning one server response into per-item facts
// in exactly one place.
//
// An implementation owns three responsibilities the layers above it cannot
// discharge on its behalf:
//
//   - Correlating a response with its request. Every ItemResult carries the
//     requested name it belongs to, and that name must never be derived from
//     the position of an item in the response: OPC XML-DA does not guarantee
//     that a response lists items in request order, and a positional mismatch
//     would apply a command to the wrong turbine.
//   - Following a paged browse to its end. A server may answer a browse with a
//     part of the listing and a continuation point. A partial listing must
//     never be returned without an error, because a plant missing from a park
//     listing is never commanded and never monitored.
//   - Separating what the server said about the request from what it said about
//     one item. An item-level fault belongs in ItemResult.ResultID and must not
//     fail the whole call; one unreadable plant does not blind a caller to the
//     rest of the park.
type Transport interface {
	// Status returns the ServerState the server reports for itself. An empty
	// string means the server answered without reporting one, which is the
	// absence of a statement rather than a statement of failure.
	Status(ctx context.Context) (string, error)

	// Read reads the named items in a single request. It returns one
	// ItemResult per requested name the server answered for, in any order; a
	// name the server did not answer for is left out, and the layers above
	// report it as missing. The error is reserved for failures of the whole
	// request.
	Read(ctx context.Context, names []string, opts ReadOptions) (Response, error)

	// Write writes the given items in a single request, under the same
	// contract as Read. A server is expected to confirm every written item,
	// and an item left out of the response is treated as unconfirmed rather
	// than as silent consent.
	Write(ctx context.Context, items []ItemWrite) (Response, error)

	// Browse lists the child nodes of one path, following the server's
	// continuation points until the listing is complete.
	Browse(ctx context.Context, path string, filter BrowseFilter) ([]Node, error)
}

// ReadOptions asks for a property of a read that the layers above the Transport
// cannot enforce for themselves, because only the server can.
type ReadOptions struct {
	// MaxAge bounds how old a value out of the server's cache may be before the
	// server has to fetch a fresh one from the device. It is the mechanism
	// OPC XML-DA defines for this, as the MaxAge attribute of a read request,
	// and it is the difference between *detecting* a stale value and
	// *preventing* one: the item timestamp only says how old the answer was.
	//
	// Zero means the caller states no requirement, and a Transport must then
	// omit the attribute entirely rather than send zero — the specification
	// gives the value 0 its own meaning, namely the most accurate data
	// available, which is not the same statement as "no requirement".
	//
	// A Transport that cannot express this must still honour everything else it
	// is asked; the layers above keep checking the item timestamps regardless,
	// so a transport without MaxAge degrades to detection rather than to
	// nothing.
	MaxAge time.Duration
}

// Response is the outcome of one batched read or write.
type Response struct {
	// ServerState is what the response's reply base reported. Empty means the
	// server did not report it. It is checked on every response, not only on
	// the GetStatus before a command: a command takes several requests, and a
	// server that degrades in between must stop receiving them.
	ServerState string

	// Items holds one entry per requested item the server answered for.
	Items []ItemResult
}

// ItemResult is what a server answered for one item.
//
// The fields are facts about the item, not judgements about it: whether a
// quality is good enough, whether a timestamp is too old and whether a
// ResultID is fatal are decisions of the layers above, which know what the
// value is going to be used for.
type ItemResult struct {
	// Name is the requested item name this result belongs to.
	Name string

	// Values carries the item's value. A scalar is a one-element slice, so a
	// server that types a single value as a scalar and one that types it as a
	// one-element array are not distinguishable here — the read-back checks
	// only ever look at the leading elements, and turning a server's encoding
	// choice into an unverifiable session would be the wrong trade. An empty
	// slice means the server sent no value at all.
	Values []uint64

	// Quality is the OPC quality field, for example "good" or
	// "badDeviceFailure". Empty means the server did not report quality, which
	// the specification defines as good.
	Quality string

	// Timestamp is the item's timestamp, zero if the server reported none.
	Timestamp time.Time

	// ResultID is the item-level fault code the server reported for this item,
	// empty if the server reported none. It is a statement about this item
	// alone.
	ResultID string

	// Err is set when the transport could not turn the server's answer for
	// this item into an ItemResult — a value of a type that is not a number,
	// say. Like ResultID it concerns this item only, and it wraps one of this
	// package's sentinel errors.
	Err error
}

// ItemWrite is one item of a batched write.
//
// Enercon types every writable item this package touches as an array of long
// words, so the elements are 32 bit. That is why the value is a []uint32 and
// not a []uint64: the width of the elements is what decides whether a
// conforming server sees ArrayOfUnsignedInt or ArrayOfUnsignedLong, and only
// the former matches the item.
type ItemWrite struct {
	Name  string
	Value []uint32
}

// BrowseFilter narrows what a browse returns. The zero value asks for
// everything.
type BrowseFilter struct {
	// BranchesOnly restricts the listing to nodes that have children.
	BranchesOnly bool

	// NamePattern restricts the listing to element names matching this
	// pattern, in which "*" stands for any sequence of characters and "?" for
	// any single one. Empty means no name filter.
	NamePattern string
}

// Node is one element of a browse listing.
type Node struct {
	// Name is the element's name within its parent, for example "SetCtrl".
	Name string

	// ItemName is the element's full item name, for example
	// "Loc/Wec/Plant2/Ctrl/SetCtrl". OPC XML-DA requires it only for items, so
	// a server may leave it empty for a branch — a plant node that is named
	// only in Name still exists and must not be dropped because of it.
	ItemName string

	// HasChildren reports whether the node has children.
	HasChildren bool

	// IsItem reports whether the node is a readable or writable item.
	IsItem bool
}
