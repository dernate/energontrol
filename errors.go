package energontrol

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors. Every error returned by this package wraps one of these, so
// callers can classify a failure with errors.Is instead of matching on strings.
//
// The distinction that matters most for a control loop is between errors that
// describe the plant ("the state you asked for is not reachable") and errors
// that describe the connection ("I do not know the state of the plant").
var (
	// ErrNoPlants is returned when a command is issued without plant numbers.
	ErrNoPlants = errors.New("energontrol: no plant numbers provided")
	// ErrDuplicatePlant is returned when the same plant is listed twice. Two
	// concurrent sessions on one plant would race against each other.
	ErrDuplicatePlant = errors.New("energontrol: duplicate plant number")
	// ErrInvalidValue is returned when a Ctrl or Rbh value is outside the set of
	// values a client is allowed to write.
	ErrInvalidValue = errors.New("energontrol: value must not be written by a client")
	// ErrNothingRequested is returned by ControlAndRbh when none of
	// SetCtrlValue, SetRbhValue and SetIceDetValue is set, so the command would
	// write nothing at all.
	ErrNothingRequested = errors.New("energontrol: no Ctrl, Rbh or IceDet value was requested")
	// ErrInvalidUserID is returned when the user id does not fit into the long
	// word Enercon defines for it. It is an argument error and is reported
	// before anything is sent to the server.
	ErrInvalidUserID = errors.New("energontrol: user id does not fit in a long word")

	// ErrServerNotRunning is returned when the OPC server answers but reports a
	// ServerState other than "running".
	ErrServerNotRunning = errors.New("energontrol: OPC server is not in state \"running\"")

	// ErrItemMissing is returned when the server's response does not contain an
	// item that was requested. The state of that plant is unknown.
	ErrItemMissing = errors.New("energontrol: item missing from server response")
	// ErrItemFault is returned when the server reports a ResultID for an item.
	ErrItemFault = errors.New("energontrol: server reported an item fault")
	// ErrBadQuality is returned when an item's quality is not "good". Its value
	// must not be used as a process value.
	ErrBadQuality = errors.New("energontrol: item quality is not good")
	// ErrStaleValue is returned when an item's timestamp is older than the
	// configured maximum age. See WithMaxStateAge.
	ErrStaleValue = errors.New("energontrol: item value is stale")
	// ErrNoItemTime is returned when a maximum state age is configured but the
	// server reports no timestamp for an item, so its age cannot be
	// established. It wraps ErrStaleValue: an age that cannot be established is
	// not an age within the limit.
	ErrNoItemTime = fmt.Errorf("%w: the server reported no item timestamp", ErrStaleValue)
	// ErrUnexpectedType is returned when an item's value cannot be interpreted as
	// an unsigned integer.
	ErrUnexpectedType = errors.New("energontrol: unexpected OPC value type")
	// ErrUncorrelatable is returned when a response cannot be matched to the
	// request. Positional matching is deliberately not attempted, because a wrong
	// match would apply a command to the wrong turbine.
	ErrUncorrelatable = errors.New("energontrol: response item cannot be correlated with the request")

	// ErrPlantUnderEnerconControl is returned for plants in CtrlStop60Enercon or
	// CtrlStopEnercon. The plant was stopped with higher rights and cannot be
	// commanded by a client.
	ErrPlantUnderEnerconControl = errors.New("energontrol: plant is controlled by Enercon (higher rights)")
	// ErrPlantCommunication is returned for plants in CtrlCommError. The real
	// state of the plant is unknown; it is neither known to run nor known to be
	// stopped.
	ErrPlantCommunication = errors.New("energontrol: plant communication error, plant state unknown")

	// ErrSessionState is returned when a control session does not reach the state
	// the protocol requires. More specific session errors below wrap it.
	ErrSessionState = errors.New("energontrol: unexpected session state")
	// ErrSessionOccupied means another client holds the session.
	ErrSessionOccupied = errors.New("energontrol: control session is occupied")
	// ErrSessionBlocked means the session is blocked by a global reservation.
	ErrSessionBlocked = errors.New("energontrol: control session is blocked by a global reservation")
	// ErrAccessDenied means the server refused access to the session.
	ErrAccessDenied = errors.New("energontrol: access denied")
	// ErrInsufficientRights means the user id does not carry the rights for this
	// command. Retrying will not help.
	ErrInsufficientRights = errors.New("energontrol: insufficient rights")
	// ErrIncorrectUserID means the server rejected the user id.
	ErrIncorrectUserID = errors.New("energontrol: incorrect user id")
	// ErrSessionValue means the server rejected the written value.
	ErrSessionValue = errors.New("energontrol: server reported a value error")
	// ErrPublicKey is returned when the session public key reads as zero.
	ErrPublicKey = errors.New("energontrol: session public key is zero")
	// ErrSessionExpired is returned when the session lifetime Enercon documents
	// for control access to a single plant has run out before the procedure
	// completed. The remaining steps cannot achieve anything and are not
	// attempted. It wraps ErrSessionState so a caller that classifies session
	// problems still catches it.
	ErrSessionExpired = fmt.Errorf("%w: the control session lifetime has run out", ErrSessionState)

	// ErrCtrlValueRejected is returned for plants whose Ctrl item reports 121,
	// the feedback code Enercon uses for a rejected control value.
	ErrCtrlValueRejected = errors.New("energontrol: plant rejected the control value")

	// ErrSessionIDMismatch is returned when the session id read back from
	// SessionRequest is not the one this client wrote. The session belongs to
	// somebody else and must not be used.
	ErrSessionIDMismatch = errors.New("energontrol: the reserved session belongs to another client")

	// ErrSessionUnverified is returned when a check the Enercon session schema
	// requires could not be carried out at all — the session id or a written
	// value could not be read back, so neither ownership nor content of the
	// session is established.
	//
	// It is deliberately distinct from ErrSessionIDMismatch, which states that
	// the session provably belongs to somebody else, and from the one case that
	// stays tolerated: a session id read back as zero means the server does not
	// report the id, because this package never draws zero.
	ErrSessionUnverified = errors.New("energontrol: the control session could not be verified")

	// ErrParameterNotAccepted is returned when the value read back from a
	// Set… item is not the value that was written, so submitting the session
	// would commit something other than the requested command.
	ErrParameterNotAccepted = errors.New("energontrol: the server did not accept the written value")

	// ErrRbhUnavailable is returned for plants that report no rotor blade
	// heating, that is, whose heating status word has the "installed" bit clear.
	ErrRbhUnavailable = errors.New("energontrol: rotor blade heating is not available on this plant")

	// ErrSessionLeftOpen is attached to a plant result when a control session was
	// reserved but the procedure could not complete and no release mechanism is
	// configured. The session stays reserved until the server's own timeout
	// expires. See WithSessionRelease.
	ErrSessionLeftOpen = errors.New("energontrol: control session was left open")
)

// ItemError describes a problem with a single OPC item. It wraps one of the
// sentinel errors above, so errors.Is(err, ErrBadQuality) and friends work.
type ItemError struct {
	ItemName string
	Reason   error
	Detail   string
}

func (e *ItemError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("energontrol: item %q: %s", e.ItemName, reasonText(e.Reason))
	}
	return fmt.Sprintf("energontrol: item %q: %s (%s)", e.ItemName, reasonText(e.Reason), e.Detail)
}

func (e *ItemError) Unwrap() error { return e.Reason }

// SessionStateError reports that a control session did not reach the state the
// protocol requires at this step. It unwraps to a specific sentinel where the
// observed state identifies the cause, and to ErrSessionState otherwise.
type SessionStateError struct {
	PlantNo uint8
	Want    SessionState
	Got     SessionState
}

func (e *SessionStateError) Error() string {
	return fmt.Sprintf("energontrol: plant %d: session state is %s, expected %s",
		e.PlantNo, e.Got, e.Want)
}

func (e *SessionStateError) Unwrap() error {
	switch e.Got {
	case SessionBlocked:
		return ErrSessionBlocked
	case SessionOccupied:
		return ErrSessionOccupied
	case SessionAccessDenied:
		return ErrAccessDenied
	case SessionValueError:
		return ErrSessionValue
	case SessionIncorrectUserID:
		return ErrIncorrectUserID
	case SessionInsufficientRights:
		return ErrInsufficientRights
	default:
		return ErrSessionState
	}
}

// reasonText strips the package prefix from a sentinel so it reads well when
// embedded in a longer message.
func reasonText(err error) string {
	if err == nil {
		return "unknown reason"
	}
	return strings.TrimPrefix(err.Error(), "energontrol: ")
}
