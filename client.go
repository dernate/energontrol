package energontrol

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dernate/gopcxmlda"
)

// OpcClient is the subset of an OPC XML-DA client this package needs.
//
// *gopcxmlda.Server satisfies it as is. Taking an interface rather than a
// concrete struct has three effects: the protocol logic becomes testable without
// a network, an alternative transport can be substituted, and the client is no
// longer copied by value — gopcxmlda.Server contains a sync.Mutex from v1.2.0
// on, so passing it by value is a copylocks violation.
type OpcClient interface {
	GetStatus(ctx context.Context, clientRequestHandle *string, namespace string) (gopcxmlda.TGetStatus, error)
	Read(ctx context.Context, items []gopcxmlda.TItem, clientRequestHandle *string,
		clientItemHandles *[]string, namespace string, options map[string]interface{}) (gopcxmlda.TRead, error)
	Write(ctx context.Context, items []gopcxmlda.TItem, clientRequestHandle *string,
		clientItemHandles *[]string, namespace string, options map[string]interface{}) (gopcxmlda.TWrite, error)
	Browse(ctx context.Context, itemPath string, clientRequestHandle *string, namespace string,
		options gopcxmlda.TBrowseOptions) (gopcxmlda.TBrowse, error)
}

// Compile-time proof that the interface matches the client it was extracted
// from. Note the pointer: gopcxmlda.Server has pointer receivers and, from
// v1.2.0 on, contains a sync.Mutex, so it must not be copied.
var _ OpcClient = (*gopcxmlda.Server)(nil)

// SessionKind selects the branch a control session runs in.
type SessionKind string

const (
	// SessionCtrl is the Loc/Wec/Plant<n>/Ctrl branch, used for control and
	// heating commands.
	SessionCtrl SessionKind = "Ctrl"
	// SessionReset is the Loc/Wec/Plant<n>/Reset branch, used for resets.
	SessionReset SessionKind = "Reset"
)

// SessionReleaseFunc releases a control session that was reserved but could not
// be completed. See WithSessionRelease.
type SessionReleaseFunc func(ctx context.Context, opc OpcClient, plant uint8,
	kind SessionKind, privateKey uint16, publicKey uint64) error

// Default polling behaviour for session state transitions: ten attempts at
// 100 ms, the budget v1 used and that is proven in the field. The transitions
// are immediate on the server, so a longer budget mostly delays the report of a
// session that is not going to advance.
//
// Unlike in v1 the budget is configurable and the wait is cancellable, so an
// installation whose SCADA needs longer can raise it with WithSessionPolling.
const (
	defaultPollInterval = 100 * time.Millisecond
	defaultPollTimeout  = time.Second
)

// defaultSessionLifetime is the session timeout the ENERCON technical data
// sheet gives for control access to a single plant (Tab. 81).
//
// It is the hard ceiling on everything a command may wait for: once it has
// elapsed the server has dropped the session, so no further state transition
// can occur and no write can still be committed. Every wait inside a session is
// clamped to it, which is what keeps the per-transition polling budget from
// adding up past the point where the session still exists.
const defaultSessionLifetime = 60 * time.Second

// Client issues commands to the turbines of one park.
//
// A Client is safe for concurrent use by multiple goroutines. Commands are
// serialised per plant inside the Client: the Enercon control session is a
// single, plant-wide resource, and two overlapping sessions on one plant
// overwrite each other's keys. A command for a plant another goroutine is
// currently commanding waits for that command to finish, in plant-number
// order, so overlapping plant sets cannot deadlock. Read-only calls are never
// blocked.
//
// The serialisation is per Client, so it covers callers that share one — which
// is the intended way to use it, since a Client owns one park. It cannot cover
// a second process; that is what the session id verification is for.
type Client struct {
	opc                 OpcClient
	log                 *slog.Logger
	pollInterval        time.Duration
	pollTimeout         time.Duration
	sessionLifetime     time.Duration
	maxStateAge         time.Duration
	lenientVerification bool
	release             SessionReleaseFunc
	now                 func() time.Time

	// inFlight holds one channel per plant that a command currently owns. It is
	// closed when the command finishes, which wakes everyone waiting for that
	// plant.
	plantsMu sync.Mutex
	inFlight map[uint8]chan struct{}
}

// Option configures a Client.
type Option func(*Client)

// WithLogger attaches a logger. By default the package logs nothing: a library
// reports through its return values, and v1's LogLevel reconfigured the calling
// application's global logrus logger as a side effect.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) {
		if l != nil {
			c.log = l
		}
	}
}

// WithSessionPolling sets how often and for how long the client waits for a
// single session state transition. The default is 100 ms over one second.
//
// The timeout applies per transition, not per command: a command waits for four
// of them. What bounds the command as a whole is the session lifetime Enercon
// documents for control access to a single plant (60 s) — every wait inside a
// session is clamped to the time left of it, and a timeout larger than the
// lifetime is clamped to the lifetime, because waiting longer than that means
// waiting on a session the server has already dropped.
func WithSessionPolling(interval, timeout time.Duration) Option {
	return func(c *Client) {
		if interval > 0 {
			c.pollInterval = interval
		}
		if timeout > 0 {
			if timeout > c.sessionLifetime {
				timeout = c.sessionLifetime
			}
			c.pollTimeout = timeout
		}
	}
}

// WithMaxStateAge rejects item values whose timestamp is older than d, with
// ErrStaleValue. OPC XML-DA servers may answer a read from a cache, and a
// control decision taken on a stale state is a decision taken on the wrong
// state.
//
// The check is strict: an item for which the server reports no timestamp at all
// is rejected with ErrNoItemTime, which wraps ErrStaleValue. An age that cannot
// be established is not an age within the limit, and treating it as one would
// switch the check off exactly on the servers it is meant to guard against — a
// server that answers from a cache is more likely, not less, to be one that
// does not fill ItemTime. A server that never reports timestamps therefore
// cannot be used together with this option.
//
// The default is 0, which disables the check, because it depends on the server
// returning item timestamps and on both clocks agreeing. Enabling it with a
// value well above the SCADA's own update cycle (a few seconds) is recommended
// for production use.
func WithMaxStateAge(d time.Duration) Option {
	return func(c *Client) { c.maxStateAge = d }
}

// WithLenientVerification accepts a written item that the server did not
// confirm in its WriteResponse.
//
// By default an item missing from the response is reported as unconfirmed
// (ErrItemMissing) and the plant fails, because gopcxmlda sends
// ReturnValuesOnReply and the response is therefore expected to carry one item
// per written item. This option exists for a server that genuinely does not
// echo written items and would otherwise be unusable.
//
// It gives up the only evidence a Reset has that SetReset arrived, and for
// control commands it leaves the value read-back as the sole check. Turn it on
// only for an installation where the strict behaviour has been shown to reject
// writes the server did carry out.
func WithLenientVerification() Option {
	return func(c *Client) { c.lenientVerification = true }
}

// WithSessionLifetime overrides the session lifetime used to bound the waits
// inside a command. The default is 60 s, the value the ENERCON technical data
// sheet gives for control access to a single plant.
//
// It exists for an installation whose documentation states a different value.
// Raising it above what the server actually enforces reintroduces the case this
// bound exists to prevent: waiting on, and writing into, a session that has
// already expired.
func WithSessionLifetime(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.sessionLifetime = d
			if c.pollTimeout > d {
				c.pollTimeout = d
			}
		}
	}
}

// WithSessionRelease installs a function that releases a control session which
// was reserved but could not be completed.
//
// No default is provided, because the Enercon technical data sheet describes no
// way to abort a session: a session ends by running into its timeout, documented
// as 60 s for control access to a single plant. Without this option a session
// that cannot be completed is reported with ErrSessionLeftOpen and expires on
// that timeout, during which the plant answers "occupied".
//
// The option remains for installations whose Enercon documentation does define
// an abort telegram.
func WithSessionRelease(f SessionReleaseFunc) Option {
	return func(c *Client) { c.release = f }
}

// New returns a Client that talks to the park behind opc.
func New(opc OpcClient, opts ...Option) *Client {
	c := &Client{
		opc:             opc,
		log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		pollInterval:    defaultPollInterval,
		pollTimeout:     defaultPollTimeout,
		sessionLifetime: defaultSessionLifetime,
		now:             time.Now,
		inFlight:        make(map[uint8]chan struct{}),
	}
	for _, o := range opts {
		o(c)
	}
	// An option may have lowered the lifetime below the polling budget.
	if c.pollTimeout > c.sessionLifetime {
		c.pollTimeout = c.sessionLifetime
	}
	return c
}

// lockPlants takes the command lock for every plant in plants and returns the
// function that releases them again.
//
// Locks are taken in ascending plant number, so two commands with overlapping
// plant sets can never hold the halves each other needs. Waiting is bounded by
// ctx, and a context that is already cancelled is reported before any lock is
// taken, so a cancelled command does not start.
func (c *Client) lockPlants(ctx context.Context, plants []uint8) (func(), error) {
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	ordered := make([]uint8, len(plants))
	copy(ordered, plants)
	slices.Sort(ordered)

	held := make([]uint8, 0, len(ordered))
	release := func() {
		c.plantsMu.Lock()
		defer c.plantsMu.Unlock()
		for _, p := range held {
			if ch, ok := c.inFlight[p]; ok {
				delete(c.inFlight, p)
				close(ch)
			}
		}
	}
	for _, p := range ordered {
		for {
			c.plantsMu.Lock()
			busy, taken := c.inFlight[p]
			if !taken {
				c.inFlight[p] = make(chan struct{})
				c.plantsMu.Unlock()
				held = append(held, p)
				break
			}
			c.plantsMu.Unlock()
			select {
			case <-busy:
				// The other command finished; try to claim the plant again.
			case <-ctx.Done():
				release()
				return nil, ctx.Err()
			}
		}
	}
	return release, nil
}

// ServerAvailable reports whether the OPC server answers and is in state
// "running".
//
// Unlike v1 it returns an error whenever it returns false: v1 answered
// (false, nil) for a server that was reachable but suspended, and every caller
// then propagated a nil error, so the failure was invisible.
func (c *Client) ServerAvailable(ctx context.Context) error {
	var handle string
	status, err := c.opc.GetStatus(ctx, &handle, "")
	if err != nil {
		return wrapf(err, "GetStatus")
	}
	if s := strings.TrimSpace(status.Response.Result.ServerState); s != serverStateRunning {
		// Unlike the per-response check, an empty state is a failure here:
		// reporting the state is the whole purpose of GetStatus.
		return &serverStateError{state: s}
	}
	return nil
}

type serverStateError struct{ state string }

func (e *serverStateError) Error() string {
	if e.state == "" {
		return "energontrol: OPC server did not report a ServerState"
	}
	return "energontrol: OPC server is in state \"" + e.state + "\", expected \"running\""
}

func (e *serverStateError) Unwrap() error { return ErrServerNotRunning }
