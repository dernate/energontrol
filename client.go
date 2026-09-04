package energontrol

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
// installation whose SCADA needs longer can raise it with WithSessionPolling —
// up to, but well below, the 60 s session timeout Enercon documents for control
// access to a single plant.
const (
	defaultPollInterval = 100 * time.Millisecond
	defaultPollTimeout  = time.Second
)

// Client issues commands to the turbines of one park.
//
// A Client is safe for concurrent use by multiple goroutines as long as no two
// goroutines command the same plant at the same time. The Enercon control
// session is a single, plant-wide resource: two overlapping sessions on one
// plant overwrite each other's keys and the outcome is undefined. Serialise
// per plant in the caller, or route all commands for a park through one
// goroutine.
type Client struct {
	opc          OpcClient
	log          *slog.Logger
	pollInterval time.Duration
	pollTimeout  time.Duration
	maxStateAge  time.Duration
	release      SessionReleaseFunc
	now          func() time.Time
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
// session state transition. The default is 100 ms over one second.
//
// Keep the timeout below the session timeout Enercon documents for control
// access to a single plant (60 s), so the client does not still be waiting when
// the session it is waiting on has already expired.
func WithSessionPolling(interval, timeout time.Duration) Option {
	return func(c *Client) {
		if interval > 0 {
			c.pollInterval = interval
		}
		if timeout > 0 {
			c.pollTimeout = timeout
		}
	}
}

// WithMaxStateAge rejects plant states whose item timestamp is older than d,
// with ErrStaleValue. OPC XML-DA servers may answer a read from a cache, and a
// control decision taken on a stale state is a decision taken on the wrong
// state.
//
// The default is 0, which disables the check, because it depends on the server
// actually returning item timestamps and on both clocks agreeing. Enabling it
// with a value well above the SCADA's own update cycle (a few seconds) is
// recommended for production use.
func WithMaxStateAge(d time.Duration) Option {
	return func(c *Client) { c.maxStateAge = d }
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
		opc:          opc,
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		pollInterval: defaultPollInterval,
		pollTimeout:  defaultPollTimeout,
		now:          time.Now,
	}
	for _, o := range opts {
		o(c)
	}
	return c
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
	if s := status.Response.Result.ServerState; s != "running" {
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

func joinErrors(errs []error) error { return errors.Join(errs...) }
