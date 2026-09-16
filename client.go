package energontrol

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
)

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
type SessionReleaseFunc func(ctx context.Context, t Transport, plant uint8,
	kind SessionKind, privateKey uint16, publicKey uint64) error

// Default polling behaviour for session state transitions.
//
// The timeout is a wall-clock deadline and every attempt is a full SOAP round
// trip, which is why it is five seconds and not the one second an earlier
// draft used. That draft justified its value as "ten attempts at 100 ms, the
// budget v1 used and proven in the field" — but v1 polled a fixed eleven times
// with a sleep in between, independent of latency, whereas a wall-clock budget
// yields as many attempts as fit inside it. On a SCADA answering in 300 ms,
// one second bought two or three, and every wait that runs out of budget costs
// the plant 60 s of session occupancy.
//
// minPollAttempts is the other half of that fix: a server slower than the whole
// budget would otherwise get a single attempt.
const (
	defaultPollInterval = 100 * time.Millisecond
	defaultPollTimeout  = 5 * time.Second
	minPollAttempts     = 4
)

// defaultSessionLifetime is the session timeout the ENERCON technical data
// sheet gives for control access to a single plant (Tab. 81).
//
// It is the hard ceiling on everything a command may wait for once a session
// exists: after it has elapsed the server has dropped the session, so no
// further state transition can occur and no write can still be committed.
// Every wait inside a session is clamped to it, and it overrides
// minPollAttempts — a guaranteed attempt on a session that is gone is not worth
// guaranteeing.
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
	transport                Transport
	items                    itemNamer
	log                      *slog.Logger
	pollInterval             time.Duration
	pollTimeout              time.Duration
	sessionLifetime          time.Duration
	commandTimeout           time.Duration
	maxStateAge              time.Duration
	lenientWriteConfirmation bool
	lenientSessionVerify     bool
	release                  SessionReleaseFunc
	now                      func() time.Time

	// inFlight holds one channel per plant that a command currently owns. It is
	// closed when the command finishes, which wakes everyone waiting for that
	// plant.
	plantsMu sync.Mutex
	inFlight map[uint8]chan struct{}
}

// config is what the options collected. Options record what the caller asked
// for and never reconcile anything themselves, so that no option's effect can
// depend on the order the options were given in — an earlier draft clamped the
// polling budget inside the option, and WithSessionPolling followed by
// WithSessionLifetime therefore produced a different Client than the same two
// the other way round.
type config struct {
	itemRoot                 string
	log                      *slog.Logger
	pollInterval             time.Duration
	pollTimeout              time.Duration
	sessionLifetime          time.Duration
	commandTimeout           time.Duration
	maxStateAge              time.Duration
	lenientWriteConfirmation bool
	lenientSessionVerify     bool
	release                  SessionReleaseFunc
	now                      func() time.Time
	errs                     []error
}

func (cfg *config) reject(format string, args ...any) {
	cfg.errs = append(cfg.errs, fmt.Errorf("%w: %s", ErrInvalidOption, fmt.Sprintf(format, args...)))
}

// Option configures a Client.
type Option func(*config)

// WithLogger attaches a logger. By default the package logs nothing: a library
// reports through its return values, and v1's LogLevel reconfigured the calling
// application's global logrus logger as a side effect.
//
// A nil logger is ignored rather than rejected, so a caller may pass one
// straight from a configuration that has not set one up.
func WithLogger(l *slog.Logger) Option {
	return func(cfg *config) {
		if l != nil {
			cfg.log = l
		}
	}
}

// WithSessionPolling sets how often and for how long the client waits for a
// single session state transition. The default is 100 ms over five seconds.
//
// The timeout applies per transition, not per command: a command waits for four
// of them. It is a wall-clock deadline and every attempt is a round trip, so a
// slow SCADA gets fewer attempts out of the same budget — a minimum of four is
// made regardless. What bounds a command once a session exists is the session
// lifetime Enercon documents for control access to a single plant (60 s): every
// wait inside a session is clamped to the time left of it, and a timeout larger
// than the lifetime is clamped to the lifetime, because waiting longer than
// that means waiting on a session the server has already dropped. To bound a
// command as a whole, including the wait for a free session before any
// reservation exists, use WithCommandTimeout.
func WithSessionPolling(interval, timeout time.Duration) Option {
	return func(cfg *config) {
		if interval <= 0 {
			cfg.reject("WithSessionPolling needs a positive interval, got %s", interval)
			return
		}
		if timeout <= 0 {
			cfg.reject("WithSessionPolling needs a positive timeout, got %s", timeout)
			return
		}
		cfg.pollInterval, cfg.pollTimeout = interval, timeout
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
//
// It does two things, and the second is the one that matters more. Every read
// carries the age as the MaxAge attribute OPC XML-DA defines for it, which
// obliges the server to fetch a fresh value from the device rather than answer
// from its cache — that *prevents* a stale value. The timestamp check then
// *detects* one that arrives anyway, which is the part that depends on the
// server filling ItemTime and on both clocks agreeing. A server that ignores
// MaxAge is still caught by the second.
//
// MaxAge is an xs:int in milliseconds, so the largest value this option accepts
// is math.MaxInt32 milliseconds, about 24 days. Sub-millisecond durations round
// down to a device read, which is the strictest thing the attribute expresses.
func WithMaxStateAge(d time.Duration) Option {
	return func(cfg *config) {
		if d < 0 {
			cfg.reject("WithMaxStateAge cannot be negative, got %s", d)
			return
		}
		if d > maxStateAgeLimit {
			cfg.reject("WithMaxStateAge is at most %s (the MaxAge attribute is an xs:int "+
				"in milliseconds), got %s", maxStateAgeLimit, d)
			return
		}
		cfg.maxStateAge = d
	}
}

// maxStateAgeLimit is the largest age that fits the MaxAge attribute, which the
// specification types as an xs:int counting milliseconds.
const maxStateAgeLimit = time.Duration(math.MaxInt32) * time.Millisecond

// WithLenientWriteConfirmation accepts a written item that the server did not
// confirm in its WriteResponse.
//
// By default an item missing from the response is reported as unconfirmed
// (ErrItemMissing) and the plant fails, because gopcxmlda sends
// ReturnValuesOnReply and the response is therefore expected to carry one item
// per written item. This option exists for a server that genuinely does not
// echo written items and would otherwise be unusable.
//
// It gives up the only evidence a Reset has that SetReset arrived. For control
// commands the value read-back remains as a check — unlike with
// WithLenientSessionVerification, which gives that up too.
func WithLenientWriteConfirmation() Option {
	return func(cfg *config) { cfg.lenientWriteConfirmation = true }
}

// WithLenientSessionVerification accepts a control session whose id, or whose
// written value, could not be read back, logging a warning instead of failing
// the plant with ErrSessionUnverified.
//
// It gives up both checks the Enercon session schema requires: that the
// reserved session belongs to this client, and that it holds the requested
// command. What remains is the write confirmation. Because the default logger
// discards everything, a caller enabling this option should attach one with
// WithLogger, or the warnings go nowhere.
//
// A session id read back as zero stays tolerated with or without this option:
// this package never draws zero, so a zero read-back means the server does not
// report the id at all.
func WithLenientSessionVerification() Option {
	return func(cfg *config) { cfg.lenientSessionVerify = true }
}

// WithLenientVerification enables both WithLenientWriteConfirmation and
// WithLenientSessionVerification, for a server that answers none of these
// reads.
//
// It is the widest tolerance this package offers, and it is worth being precise
// about what is left: nothing confirms that the session commanded was this
// client's, nothing confirms that it held the requested value, and nothing
// confirms that the write arrived. Turn it on only for an installation where
// the strict behaviour has been shown to reject commands the server did carry
// out, and prefer whichever of the two narrower options is actually needed.
func WithLenientVerification() Option {
	return func(cfg *config) {
		cfg.lenientWriteConfirmation = true
		cfg.lenientSessionVerify = true
	}
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
	return func(cfg *config) {
		if d <= 0 {
			cfg.reject("WithSessionLifetime needs a positive duration, got %s", d)
			return
		}
		cfg.sessionLifetime = d
	}
}

// WithCommandTimeout bounds a whole command, including the wait for a free
// session before any reservation exists.
//
// The session lifetime bounds everything from the reservation onwards, but the
// wait for state "free" happens before there is a session whose lifetime could
// run out — so with a generous polling budget a command can spend that budget
// waiting to start and a full session lifetime afterwards. This option is the
// single figure a scheduler can reason about.
//
// The default is 0, which means no limit beyond the caller's own context. The
// cleanup of a session that was reserved but not completed is not subject to
// it: it runs on a context of its own, so a command that runs out of time still
// reports the session it left behind.
func WithCommandTimeout(d time.Duration) Option {
	return func(cfg *config) {
		if d <= 0 {
			cfg.reject("WithCommandTimeout needs a positive duration, got %s", d)
			return
		}
		cfg.commandTimeout = d
	}
}

// WithItemRoot points the client at a different root of the SCADA address
// space. The default is "Loc", which is what the ENERCON technical data sheet
// documents: the park number at Loc/LocNo and the plants below Loc/Wec.
//
// Everything below the root — the Wec branch, the Plant<n> nodes, the Ctrl and
// Reset branches, the item names, and the "/" between them — follows the data
// sheet and is not configurable. What varies between installations is which
// branch the park hangs off, and a package that assumes one answers nothing at
// all on an installation that chose the other: every item comes back missing,
// for every plant, with no hint as to why.
//
// Leading and trailing separators are trimmed; a root that is empty after that
// is rejected.
func WithItemRoot(root string) Option {
	return func(cfg *config) {
		trimmed := strings.Trim(strings.TrimSpace(root), "/")
		if trimmed == "" {
			cfg.reject("WithItemRoot needs a non-empty root, got %q", root)
			return
		}
		cfg.itemRoot = trimmed
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
	return func(cfg *config) { cfg.release = f }
}

// New returns a Client that talks to the park behind t. The transport for an
// Enercon SCADA is opcxmlda.New(server), from the subpackage of the same name.
//
// It panics if t is nil or if an option was given an unusable value. Both are
// programming errors that surface on the first run rather than in production:
// a Client without a transport used to panic with a nil pointer dereference
// somewhere inside a session, and an option value out of range used to be
// discarded without a word. Where the values come from a configuration file,
// use NewWithOptions and report the error.
func New(t Transport, opts ...Option) *Client {
	c, err := NewWithOptions(t, opts...)
	if err != nil {
		panic(err.Error())
	}
	return c
}

// NewWithOptions is New but reports an unusable transport or option value
// instead of panicking.
func NewWithOptions(t Transport, opts ...Option) (*Client, error) {
	if t == nil {
		return nil, errors.New("energontrol: New requires a Transport, got nil")
	}
	cfg := config{
		itemRoot: defaultItemRoot,
		// slog.DiscardHandler, not a text handler writing to io.Discard: the
		// latter reports itself as enabled, so a log line's arguments are still
		// evaluated — and one of them reads the remaining session timeout from
		// the server. A default that logs nothing should also cost nothing.
		log:             slog.New(slog.DiscardHandler),
		pollInterval:    defaultPollInterval,
		pollTimeout:     defaultPollTimeout,
		sessionLifetime: defaultSessionLifetime,
		now:             time.Now,
	}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	if err := errors.Join(cfg.errs...); err != nil {
		return nil, err
	}
	// Reconciled here, once, with every option already applied. Waiting longer
	// than the session lifetime means waiting on a session the server has
	// dropped, and which option happened to mention the lifetime last must not
	// decide the outcome.
	if cfg.pollTimeout > cfg.sessionLifetime {
		cfg.pollTimeout = cfg.sessionLifetime
	}
	if cfg.pollInterval > cfg.pollTimeout {
		cfg.pollInterval = cfg.pollTimeout
	}
	return &Client{
		transport:                t,
		items:                    itemNamer{root: cfg.itemRoot},
		log:                      cfg.log,
		pollInterval:             cfg.pollInterval,
		pollTimeout:              cfg.pollTimeout,
		sessionLifetime:          cfg.sessionLifetime,
		commandTimeout:           cfg.commandTimeout,
		maxStateAge:              cfg.maxStateAge,
		lenientWriteConfirmation: cfg.lenientWriteConfirmation,
		lenientSessionVerify:     cfg.lenientSessionVerify,
		release:                  cfg.release,
		now:                      cfg.now,
		inFlight:                 make(map[uint8]chan struct{}),
	}, nil
}

// withCommandTimeout applies WithCommandTimeout to a command's context. The
// returned cancel function is always non-nil.
func (c *Client) withCommandTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.commandTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.commandTimeout)
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
	ordered := slices.Clone(plants)
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
	state, err := c.transport.Status(ctx)
	if err != nil {
		return err
	}
	if s := strings.TrimSpace(state); s != serverStateRunning {
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
