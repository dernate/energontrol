// Command scadaprobe is an interactive diagnostic and control client for an
// Enercon SCADA.
//
// It reads and reports on its own; nothing is ever written until an operator
// picks a control operation from the menu and then confirms it by typing that
// operation's name. Every step — what was offered, what was confirmed or
// aborted, every OPC request that went out, what came back, and what the
// outcome was — goes into a log file as JSON lines, so a run can be analysed
// afterwards. The user id is the one thing deliberately kept out of it: it is
// the operator's credential for the park, and a log file gets copied around.
//
// Configuration comes from the environment, the same variables the live test
// suite uses, so an existing .env works as is:
//
//	OPC_URL=http://scada.example:8080/DA \
//	USERID=1234 \
//	PARKNO=4242 \
//	ENERGONTROL_TEST_PLANTS=2,4 \
//	    scadaprobe
//
// An operation that needs values — the combined command — asks for them before
// the confirmation and lists them on the confirmation screen, so the operator
// confirms what will actually be sent rather than that something will be.
//
// PARKNO and ENERGONTROL_TEST_PLANTS are what make a command possible at all:
// the park number is verified against the server before the menu opens, and a
// command only ever goes to the plants named explicitly. Without them the tool
// still reads and diagnoses, it just refuses to command.
//
// # What it can send
//
// Every command the library can send has a menu entry, so each one can be tried
// against a real server: the nine documented control values (start, the two
// plain stops, the two gradient stops, the stops for ice detection and shadow
// flicker, and the two species-protection stops), the four heating values, the
// ice warning lamp, the fault acknowledgement, and the combined command that
// writes a control value, a heating value and the lamp in one session. The
// control block is listed in data-sheet order and every label names the value it
// writes, so a selection can be checked against the data sheet without reading
// this file. Values above 8 are absent on purpose: those are states a plant
// reports, not commands a client may send.
//
// forceExplicitCommand is a toggle in the menu rather than a per-operation
// question. With it off — the default — a stop request is satisfied by any state
// at least as stopped as the one asked for, including one Enercon made itself.
// With it on, the plant must reach exactly the state the command produces.
//
// # What it writes
//
// Reading writes nothing at all — GetStatus, Browse and Read only.
//
// A control operation writes exactly the three items the Enercon session schema
// defines, per plant, and nothing else — the combined command writes one
// parameter item per part it was given, still in the one session:
//
//	Loc/Wec/Plant<n>/{Ctrl,Reset}/SessionRequest    session id, user id, private key
//	Loc/Wec/Plant<n>/Ctrl/SetCtrl                   the value, private key, public key
//	                     /SetRbh                    (or SetRbh, or SetIceDet,
//	                     /SetIceDet                  or Reset/SetReset)
//	Loc/Wec/Plant<n>/{Ctrl,Reset}/SessionSubmit     private key, public key
//
// Everything else a command does is a read: the session state four times, the
// session id twice, the public key, the value read-back, and the remaining
// session timeout if a session had to be left open. The log lists every one of
// them with what it carried.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dernate/energontrol/v2"
	"github.com/dernate/energontrol/v2/opcxmlda"
	"github.com/dernate/gopcxmlda"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load()

	cfg := configFromEnv()
	flag.StringVar(&cfg.logPath, "log", cfg.logPath,
		"log file; it is appended to, never truncated")
	flag.DurationVar(&cfg.timeout, "timeout", cfg.timeout, "HTTP timeout per request")
	flag.DurationVar(&cfg.maxAge, "max-age", cfg.maxAge,
		"WithMaxStateAge for the reads; 0 leaves the freshness demand off")
	flag.IntVar(&cfg.samples, "samples", cfg.samples, "round trips to time in the diagnosis")
	flag.Parse()

	logFile, err := os.OpenFile(cfg.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scadaprobe: opening the log: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = logFile.Close() }()

	logger := slog.New(slog.NewJSONHandler(logFile, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if err := run(cfg, os.Stdin, os.Stdout, logger); err != nil {
		logger.Error("run failed", "err", err.Error())
		fmt.Fprintf(os.Stderr, "scadaprobe: %v\n", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// configuration
// ---------------------------------------------------------------------------

type config struct {
	url     string
	plants  string
	park    string
	user    string
	logPath string
	timeout time.Duration
	maxAge  time.Duration
	samples int

	// settleTimeout and settleInterval bound the wait for a commanded plant to
	// report the state it was asked for. Reaching the end of a session says the
	// command was accepted, not that the turbine moved.
	settleTimeout  time.Duration
	settleInterval time.Duration
}

func configFromEnv() config {
	return config{
		url:            os.Getenv("OPC_URL"),
		plants:         os.Getenv("ENERGONTROL_TEST_PLANTS"),
		park:           os.Getenv("PARKNO"),
		user:           os.Getenv("USERID"),
		logPath:        "scadaprobe.log",
		timeout:        10 * time.Second,
		samples:        20,
		settleTimeout:  60 * time.Second,
		settleInterval: 3 * time.Second,
	}
}

// ---------------------------------------------------------------------------
// the application
// ---------------------------------------------------------------------------

type app struct {
	cfg    config
	r      *report
	in     *bufio.Scanner
	log    *slog.Logger
	server *gopcxmlda.Server
	obs    *observer
	client *energontrol.Client

	plants []uint8 // the explicit selection; empty when none was given
	userID uint64
	parkNo uint64
	parkOK bool // the server confirmed the park number
	info   energontrol.TurbineInfo

	// force is the forceExplicitCommand argument the control operations pass.
	// It is off by default, which is the tolerant mode most callers want, and
	// the menu toggles it: with it on, a plant must reach exactly the state the
	// command produces, and a plant Enercon has stopped is reported as not
	// permitted instead of counting as stopped.
	force bool
	// expectOverride is the state to wait for when the operation itself cannot
	// know it in advance — the combined command, where the operator picks the
	// control value at the prompt.
	expectOverride *energontrol.CtrlValue
	// combined is what the combined command's prompts gathered, held between
	// prepare and send so the confirmation screen can show it.
	combined *energontrol.ControlAndRbhValue
}

// selection is the plants to act on: the explicit list where there is one, and
// otherwise every plant the park lists — which is fine for reading and never
// enough for a command.
func (a *app) selection() []uint8 {
	if len(a.plants) > 0 {
		return a.plants
	}
	return a.info.PlantNo
}

func run(cfg config, in io.Reader, out io.Writer, logger *slog.Logger) error {
	if cfg.url == "" {
		return errors.New("no endpoint: set OPC_URL")
	}
	endpoint, err := url.Parse(cfg.url)
	if err != nil {
		return fmt.Errorf("OPC_URL: %w", err)
	}
	plants, err := parsePlants(cfg.plants)
	if err != nil {
		return fmt.Errorf("ENERGONTROL_TEST_PLANTS: %w", err)
	}

	a := &app{
		cfg:    cfg,
		r:      &report{w: out},
		in:     bufio.NewScanner(in),
		log:    logger,
		plants: plants,
	}
	a.server = &gopcxmlda.Server{Url: endpoint, LocaleID: "en-us", Timeout: cfg.timeout}
	a.obs = &observer{inner: opcxmlda.New(a.server), log: logger}

	// The library logs into the same file, so its own warnings — a session id
	// the server does not report, a session left open, a read-back that could
	// not confirm a zero value — land next to the request trace.
	opts := []energontrol.Option{energontrol.WithLogger(logger)}
	if cfg.maxAge > 0 {
		opts = append(opts, energontrol.WithMaxStateAge(cfg.maxAge))
	}
	if a.client, err = energontrol.NewWithOptions(a.obs, opts...); err != nil {
		return err
	}

	if cfg.user != "" {
		if a.userID, err = strconv.ParseUint(cfg.user, 10, 32); err != nil {
			return fmt.Errorf("USERID: %q does not fit in a long word", cfg.user)
		}
	}
	if cfg.park != "" {
		if a.parkNo, err = strconv.ParseUint(cfg.park, 10, 64); err != nil {
			return fmt.Errorf("PARKNO: %q is not a park number", cfg.park)
		}
	}

	a.r.head("energontrol scadaprobe")
	a.r.kv("Endpoint", endpoint.String())
	a.r.kv("Park expected", orNone(cfg.park))
	a.r.kv("Plants selected", orNone(cfg.plants))
	a.r.kv("User id", orNone(cfg.user))
	a.r.kv("MaxAge", maxAgeLabel(cfg.maxAge))
	a.r.kv("Log", cfg.logPath+" (JSON lines; the user id is kept out of it)")
	a.log.Info("session started", "endpoint", endpoint.String(), "park", cfg.park,
		"plants", cfg.plants, "maxAge", cfg.maxAge.String(),
		"timeout", cfg.timeout.String())

	ctx := context.Background()
	if err := a.overview(ctx); err != nil {
		return err
	}
	a.verifyPark(ctx)
	return a.menu(ctx)
}

// verifyPark checks the park number once, before the menu opens. Without a
// confirmed park, no command is sent at all.
func (a *app) verifyPark(ctx context.Context) {
	a.r.section("Park number")
	if a.cfg.park == "" {
		a.r.warn("PARKNO is not set, so no command can be sent: a set of credentials " +
			"pointed at the wrong park would command somebody else's turbines")
		return
	}
	match, err := a.client.ParkNoMatch(ctx, a.parkNo, true)
	switch {
	case err != nil:
		a.r.fail("the park number could not be verified: " + err.Error())
	case !match:
		a.r.fail(fmt.Sprintf("the server does not serve park %d — no command will be sent",
			a.parkNo))
	default:
		a.parkOK = true
		a.r.pass(fmt.Sprintf("the server confirms park %d", a.parkNo))
	}
	a.log.Info("park verified", "expected", a.parkNo, "confirmed", a.parkOK,
		"err", errText(err))
}

// ---------------------------------------------------------------------------
// the control operations
// ---------------------------------------------------------------------------

// operation is one menu entry that writes something.
//
// name is what an operator has to type to confirm it. It is the operation's own
// word rather than a plain "yes", so that the habit of confirming cannot send a
// command nobody read.
type operation struct {
	key   string
	name  string
	label string
	note  string
	// group is the heading this operation is listed under, so the menu is
	// grouped the way the data sheet is: the Ctrl values, then the heating
	// values, then the lamp, then Reset.
	group string
	send  func(ctx context.Context, a *app, plants []uint8) (energontrol.Results, error)
	// expect is the state a plant reports once the command has taken effect,
	// for the commands where the data sheet documents one. Where it is nil,
	// nothing is waited for: stop for ice detection and stop for shadow flicker
	// have no documented resulting state.
	expect *energontrol.CtrlValue
	// usesForce marks the operations that pass forceExplicitCommand, so the
	// confirmation screen only mentions the toggle where it changes anything.
	usesForce bool
	// prepare asks for whatever the operation needs to know before the
	// confirmation, so that the confirmation screen can show what would actually
	// be sent. Without it the combined command would be confirmed before its
	// values were chosen, which is not a confirmation of anything. It returns
	// false if the operator backed out.
	prepare func(a *app) bool
	// value is the raw number this operation writes, for the groups where an
	// operation is exactly one value: the control values, the heating values and
	// the lamp. The combined command's prompts are built from it, so what they
	// offer and what the menu offers cannot drift apart. Nil for the fault
	// acknowledgement and for the combined command itself, which write no single
	// value of their own.
	value *uint64
}

const stopNote = "a plant that was just stopped refuses a new reservation for 360 s, " +
	"so it cannot be commanded again straight away"

const (
	groupCtrl    = "Control values (Ctrl/SetCtrl)"
	groupRbh     = "Rotor blade heating (Ctrl/SetRbh)"
	groupIceDet  = "Ice warning lamp (Ctrl/SetIceDet)"
	groupReset   = "Fault acknowledgement (Reset/SetReset)"
	groupCombine = "Several parameters in one session"
)

// operations is every command the library can send, one menu entry each.
//
// The control block is in data-sheet order, values 0 to 8, and each label names
// the value it writes so a selection can be checked against the data sheet
// without reading this file. The values above 8 are states a plant reports, not
// commands, so they are deliberately absent.
func operations() []operation {
	want := func(v energontrol.CtrlValue) *energontrol.CtrlValue { return &v }
	raw := func(v uint64) *uint64 { return &v }
	ctrl := func(key string, value energontrol.CtrlValue, name, label, note string) operation {
		// What to wait for afterwards is the command value: Ctrl carries the
		// value that was set, and the plant holds it. Waiting for the blade
		// angle that value produces — which this tool used to do — never
		// matched, because no blade angle is reported.
		expect := value
		return operation{
			key: key, name: name, group: groupCtrl, note: note,
			label:  fmt.Sprintf("%s (SetCtrl %d)", label, uint64(value)),
			expect: &expect, usesForce: true, value: raw(uint64(value)),
			send: func(ctx context.Context, a *app, p []uint8) (energontrol.Results, error) {
				return a.client.SetCtrl(ctx, a.userID, value, a.force, p...)
			},
		}
	}
	// The two stops whose name carries no blade angle. Nothing can be claimed to
	// satisfy them in advance, so they are always sent; that they took effect is
	// still observable, because the plant reports the value that was set.
	const undocumentedNote = "the data sheet names no blade angle for this stop, so no " +
		"state counts as already satisfying it and it is always sent"

	return []operation{
		// Start and the two plain stops keep their own wrappers: those are the
		// three calls most callers use, and exercising them is the point.
		{key: "1", name: "start", label: "start the plants (SetCtrl 0)", group: groupCtrl,
			expect: want(energontrol.CtrlStart), usesForce: true,
			value: raw(uint64(energontrol.CtrlStart)),
			send: func(ctx context.Context, a *app, p []uint8) (energontrol.Results, error) {
				return a.client.Start(ctx, a.userID, p...)
			}},
		{key: "2", name: "stop60", label: "stop at 60° (SetCtrl 1)", group: groupCtrl,
			note:   stopNote + "; not available on controller type EP5-CS-03",
			expect: want(energontrol.CtrlStop60), usesForce: true,
			value: raw(uint64(energontrol.CtrlStop60)),
			send: func(ctx context.Context, a *app, p []uint8) (energontrol.Results, error) {
				return a.client.Stop(ctx, a.userID, false, a.force, p...)
			}},
		{key: "3", name: "stop90", label: "stop at 90°, full stop (SetCtrl 2)", group: groupCtrl,
			note:   stopNote,
			expect: want(energontrol.CtrlStop90), usesForce: true,
			value: raw(uint64(energontrol.CtrlStop90)),
			send: func(ctx context.Context, a *app, p []uint8) (energontrol.Results, error) {
				return a.client.Stop(ctx, a.userID, true, a.force, p...)
			}},
		ctrl("4", energontrol.CtrlGradientStop60, "gradient60", "gradient stop at 60°",
			stopNote+"; not available on controller type EP5-CS-03"),
		ctrl("5", energontrol.CtrlGradientStop90, "gradient90", "gradient stop at 90°",
			stopNote),
		ctrl("6", energontrol.CtrlStopIceDetection, "stop-ice", "stop for ice detection",
			stopNote+"; "+undocumentedNote),
		ctrl("7", energontrol.CtrlStopShadowFlicker, "stop-shadow",
			"stop for shadow flicker mitigation", stopNote+"; "+undocumentedNote),
		ctrl("8", energontrol.CtrlStopSpeciesProtection60, "species60",
			"stop for species protection at 60°",
			stopNote+"; not available on controller type EP5-CS-03, except on the E-160 EP5 E3"),
		ctrl("9", energontrol.CtrlStopSpeciesProtection90, "species90",
			"stop for species protection at 90°", stopNote),

		{key: "10", name: "heating-auto", group: groupRbh,
			label: "heating back to automatic (SetRbh 0)",
			value: raw(uint64(energontrol.RbhSetStandard)),
			send: func(ctx context.Context, a *app, p []uint8) (energontrol.Results, error) {
				return a.client.RbhStandard(ctx, a.userID, p...)
			}},
		{key: "11", name: "heating-autooff", group: groupRbh,
			label: "suppress automatic heating (SetRbh 2)",
			value: raw(uint64(energontrol.RbhSetAutoOff)),
			send: func(ctx context.Context, a *app, p []uint8) (energontrol.Results, error) {
				return a.client.RbhAutoOff(ctx, a.userID, p...)
			}},
		{key: "12", name: "heating-on", group: groupRbh,
			label: "heating on (SetRbh 10)",
			value: raw(uint64(energontrol.RbhSetManualOn)),
			note: "this writes 10, which suppresses the automatic system and heats " +
				"manually; the bare 8 the data sheet also lists is rejected by the server",
			send: func(ctx context.Context, a *app, p []uint8) (energontrol.Results, error) {
				return a.client.RbhOn(ctx, a.userID, p...)
			}},
		{key: "13", name: "heating-preset", group: groupRbh,
			label: "heat for the preset duration (SetRbh 128)",
			value: raw(uint64(energontrol.RbhSetPresetDuration)),
			note: "Enercon: only possible if the plant has stopped and ice was detected. " +
				"It is a one-shot action with no status bit of its own, so it is always sent",
			send: func(ctx context.Context, a *app, p []uint8) (energontrol.Results, error) {
				return a.client.SetRbh(ctx, a.userID, energontrol.RbhSetPresetDuration, p...)
			}},

		{key: "14", name: "lamp-off", group: groupIceDet,
			label: "ice warning lamp off (SetIceDet 0)",
			value: raw(uint64(energontrol.IceDetLampOff)),
			send: func(ctx context.Context, a *app, p []uint8) (energontrol.Results, error) {
				return a.client.IceDetOff(ctx, a.userID, p...)
			}},
		{key: "15", name: "lamp-on", group: groupIceDet,
			label: "ice warning lamp on (SetIceDet 8)",
			value: raw(uint64(energontrol.IceDetLampOn)),
			send: func(ctx context.Context, a *app, p []uint8) (energontrol.Results, error) {
				return a.client.IceDetOn(ctx, a.userID, p...)
			}},

		{key: "16", name: "reset", group: groupReset,
			label: "acknowledge faults (SetReset)",
			note: "a reset has no target state to compare against, so every selected " +
				"plant goes through the session procedure",
			send: func(ctx context.Context, a *app, p []uint8) (energontrol.Results, error) {
				return a.client.Reset(ctx, a.userID, p...)
			}},

		{key: "17", name: "combined", group: groupCombine,
			label: "set a control value, a heating value and the lamp together (asks which)",
			note: "one session per plant writes all the chosen parameters, which is the " +
				"only way to change several at once; the server applies them on submit",
			usesForce: true,
			prepare:   func(a *app) bool { return a.askCombined() },
			send: func(ctx context.Context, a *app, p []uint8) (energontrol.Results, error) {
				if a.combined == nil {
					return nil, errAborted
				}
				return a.client.ControlAndRbh(ctx, a.userID, *a.combined, p...)
			}},
	}
}

// errAborted ends an operation the operator backed out of.
var errAborted = errors.New("aborted")

// ---------------------------------------------------------------------------
// the menu
// ---------------------------------------------------------------------------

func (a *app) menu(ctx context.Context) error {
	ops := operations()
	for {
		a.r.section("Menu")
		a.r.line("  s   read the state of the selected plants")
		a.r.line("  d   diagnose the server: behaviour and latency")
		a.r.line("  p   change the plant selection")
		a.r.printf("  f   forceExplicitCommand: %s\n", onOff(a.force))
		group := ""
		for _, op := range ops {
			if op.group != group {
				group = op.group
				a.r.printf("\n  %s\n", group)
			}
			a.r.printf("  %-3s %s\n", op.key, op.label)
		}
		a.r.line("")
		a.r.line("  q   quit")
		if !a.canCommand() {
			a.r.line("")
			a.r.note(a.whyNoCommand())
		}

		choice, ok := a.ask("\nchoice: ")
		if !ok {
			a.r.line("\nend of input — quitting.")
			a.log.Info("session ended", "reason", "end of input")
			return nil
		}
		a.log.Info("menu choice", "choice", choice)

		switch choice {
		case "q", "quit", "exit":
			a.r.line("quitting.")
			a.log.Info("session ended", "reason", "operator quit")
			return nil
		case "":
			continue
		case "s":
			a.states(ctx)
		case "d":
			a.diagnose(ctx)
		case "p":
			a.changeSelection()
		case "f":
			a.force = !a.force
			a.r.line("forceExplicitCommand is now " + onOff(a.force) + ".")
			a.log.Info("force toggled", "forceExplicitCommand", a.force)
		default:
			i := slices.IndexFunc(ops, func(op operation) bool {
				return op.key == choice || op.name == choice
			})
			if i < 0 {
				a.r.warn("no such choice: " + choice)
				continue
			}
			if err := a.runOperation(ctx, ops[i]); err != nil && !errors.Is(err, errAborted) {
				return err
			}
		}
	}
}

func (a *app) canCommand() bool {
	return a.parkOK && len(a.plants) > 0 && a.cfg.user != ""
}

func (a *app) whyNoCommand() string {
	var missing []string
	if !a.parkOK {
		missing = append(missing, "a park number the server confirms (PARKNO)")
	}
	if len(a.plants) == 0 {
		missing = append(missing,
			`an explicit plant selection (ENERGONTROL_TEST_PLANTS, or "p" above)`)
	}
	if a.cfg.user == "" {
		missing = append(missing, "a user id (USERID)")
	}
	return "No command can be sent: it needs " + strings.Join(missing, ", ") +
		". Reading and diagnosing work regardless."
}

func (a *app) changeSelection() {
	line, ok := a.ask("plants, comma separated (empty clears the selection): ")
	if !ok {
		return
	}
	plants, err := parsePlants(line)
	if err != nil {
		a.r.warn(err.Error())
		return
	}
	if missing := notListed(plants, a.info.PlantNo); len(missing) > 0 {
		a.r.warn(fmt.Sprintf("the park does not list %v — selection unchanged", missing))
		return
	}
	a.plants = plants
	a.cfg.plants = line
	a.r.pass(fmt.Sprintf("selection is now %v", a.selection()))
	a.log.Info("selection changed", "plants", plants)
}

// ---------------------------------------------------------------------------
// running one operation
// ---------------------------------------------------------------------------

func (a *app) runOperation(ctx context.Context, op operation) error {
	if !a.canCommand() {
		a.r.warn(a.whyNoCommand())
		a.log.Warn("command refused", "operation", op.name, "reason", a.whyNoCommand())
		return errAborted
	}

	a.expectOverride = nil
	a.combined = nil

	before, err := a.client.PlantCtrlState(ctx, a.plants...)
	if err != nil {
		a.r.fail("reading the state before the command: " + err.Error())
		return errAborted
	}

	// Whatever the operation needs to know is asked before the confirmation, not
	// after it: a confirmation of "something will be written here" is worthless.
	if op.prepare != nil && !op.prepare(a) {
		a.r.line("aborted — nothing was sent.")
		a.log.Info("command aborted", "operation", op.name, "reason", "nothing selected")
		return errAborted
	}

	a.r.section("Confirm")
	a.r.kv("Operation", op.label)
	a.r.kv("Plants", fmt.Sprintf("%v", a.plants))
	a.r.kv("Park", fmt.Sprintf("%d (confirmed by the server)", a.parkNo))
	a.r.kv("User id", a.cfg.user)
	a.r.kv("State now", statesLine(before))
	if v := a.combined; v != nil {
		a.r.kv("Control value (SetCtrl)", combinedPart(v.SetCtrlValue,
			uint64(v.CtrlValue), groupCtrl))
		a.r.kv("Heating value (SetRbh)", combinedPart(v.SetRbhValue,
			uint64(v.RbhValue), groupRbh))
		a.r.kv("Ice warning lamp (SetIceDet)", combinedPart(v.SetIceDetValue,
			uint64(v.IceDetValue), groupIceDet))
	}
	if op.usesForce {
		a.r.kv("forceExplicitCommand", onOff(a.force))
	}
	if op.note != "" {
		a.r.note(op.note)
	}
	a.r.note("This moves real machinery. The status resulting from a start or a stop has " +
		"to be monitored: reaching the end of a session is not evidence that the turbine " +
		"moved.")
	a.log.Info("command offered", "operation", op.name, "plants", a.plants,
		"forceExplicitCommand", op.usesForce && a.force,
		"stateBefore", statesLine(before))

	answer, ok := a.ask(fmt.Sprintf("\ntype %q to send it, anything else to abort: ", op.name))
	if !ok || answer != op.name {
		a.r.line("aborted — nothing was sent.")
		a.log.Info("command aborted", "operation", op.name, "answer", answer,
			"endOfInput", !ok)
		return errAborted
	}

	a.r.section("Result")
	writtenBefore := len(a.obs.writtenItems())
	start := time.Now()
	a.log.Info("command sending", "operation", op.name, "plants", a.plants)

	res, err := op.send(ctx, a, a.plants)
	elapsed := time.Since(start)
	written := a.obs.writtenItems()[writtenBefore:]

	switch {
	case errors.Is(err, errAborted):
		a.r.line("aborted — nothing was sent.")
		a.log.Info("command aborted", "operation", op.name, "reason", "no value chosen")
		return errAborted
	case err != nil:
		// A call-level error means the command could not be attempted at all.
		a.r.fail(fmt.Sprintf("%s could not be attempted: %v", op.name, err))
		a.log.Error("command could not be attempted", "operation", op.name,
			"err", err.Error(), "took", elapsed.String(), "written", written)
		return errAborted
	}

	a.r.kv("Took", elapsed.Round(time.Millisecond).String())
	a.r.kv("Items written", strconv.Itoa(len(written)))
	for _, name := range written {
		a.r.note("wrote " + name)
	}
	a.reportResults(res)
	a.log.Info("command finished", "operation", op.name, "plants", a.plants,
		"took", elapsed.String(), "written", written,
		"inRequestedState", res.InRequestedState(), "results", resultsTrace(res))

	// Only the plants something was written for. A plant reported as already in
	// state has nothing to monitor, and a live run showed what waiting for it
	// anyway looks like: a minute of polling a state that was never requested
	// of the plant.
	watch := worthWatching(res)
	if skipped := len(res) - len(watch); skipped > 0 {
		a.r.note(fmt.Sprintf("%d of %d plants had nothing written, so there is nothing "+
			"to wait for there", skipped, len(res)))
	}
	if expect := op.expect; expect != nil {
		a.settle(ctx, *expect, watch)
	} else if a.expectOverride != nil {
		a.settle(ctx, *a.expectOverride, watch)
	}
	a.r.note("Nothing was restored: what the plants should be set to is the operator's " +
		"call. The state before the command is printed above.")
	return nil
}

// reportResults spells out what happened per plant, and what it means for an
// operator who might be tempted to try again.
func (a *app) reportResults(res energontrol.Results) {
	for _, p := range res {
		if p.InRequestedState() {
			a.r.pass(p.String())
			continue
		}
		a.r.fail(p.String())
		switch {
		case errors.Is(p.Err, energontrol.ErrOutcomeUncertain):
			a.r.note("the submit was confirmed, so this command may well have taken " +
				"effect. Read the state before sending it again.")
		case errors.Is(p.Err, energontrol.ErrInsufficientRights),
			errors.Is(p.Err, energontrol.ErrIncorrectUserID):
			a.r.note("do not retry: three rejected keys lock the plant out for 180 s, " +
				"and a wrong user id for 300 s.")
		case errors.Is(p.Err, energontrol.ErrSessionOccupied):
			a.r.note("another client holds the session, or the plant is still inside the " +
				"delay that follows a stop. Retrying later can work.")
		case errors.Is(p.Err, energontrol.ErrPlantUnderEnerconControl):
			a.r.note("Enercon stopped this plant with higher rights; a client cannot " +
				"start it.")
		case errors.Is(p.Err, energontrol.ErrPlantStateUnknown):
			a.r.note("the plant reports a control state this library does not know, so it " +
				"is not commanded: the state says nothing about where the blades are or " +
				"who holds control. Retrying changes nothing. Look the raw value up in " +
				"the data sheet for this controller type — and note it, because the " +
				"library's value set may need extending.")
		case errors.Is(p.Err, energontrol.ErrSessionLeftOpen):
			a.r.note(`the session stays reserved until the server's 60 s timeout, during ` +
				`which this plant answers "occupied".`)
		}
	}
	if !res.InRequestedState() {
		a.r.note("Results.InRequestedState() is false — treat the setpoint as not reached.")
	}
}

// settle polls the control state until every plant reports what was asked for.
// This is the monitoring loop the package documentation prescribes.
// worthWatching is the plants a command may have changed: the ones it wrote to,
// and the ones where the outcome is uncertain because the submit was confirmed
// but the wind-down was not.
//
// A plant the library reported as already in state is not among them. Nothing
// was written for it, so there is nothing to wait for — and waiting would be
// waiting for a state it may legitimately never report: an unforced stop is also
// satisfied by a deeper stop, and by a stop Enercon holds the plant in.
func worthWatching(res energontrol.Results) []uint8 {
	out := make([]uint8, 0, len(res))
	for _, r := range res {
		if r.Outcome == energontrol.OutcomeCommanded ||
			errors.Is(r.Err, energontrol.ErrOutcomeUncertain) {
			out = append(out, r.PlantNo)
		}
	}
	return out
}

func (a *app) settle(ctx context.Context, want energontrol.CtrlValue, plants []uint8) {
	if len(plants) == 0 {
		return
	}
	a.r.section("Waiting for the plants to carry out " + want.String())
	deadline := time.Now().Add(a.cfg.settleTimeout)
	for {
		states, err := a.client.PlantCtrlState(ctx, plants...)
		if err != nil {
			a.r.fail("polling the state: " + err.Error())
			return
		}
		a.r.kv("state", statesLine(states))
		a.log.Info("settling", "want", want.String(), "states", statesLine(states))

		reached := true
		for _, s := range states {
			// A plant whose state is unknown has not reached the target:
			// unknown is never "yes". Ctrl carries the value that was set, so
			// Reached compares against the command itself.
			if s.Err != nil || !s.Ctrl.Reached(want) {
				reached = false
			}
		}
		if reached {
			a.r.pass("every plant has carried out " + want.String())
			a.log.Info("settled", "want", want.String())
			return
		}
		if time.Now().After(deadline) {
			a.r.warn(fmt.Sprintf("not every plant has carried out %s within %s; the "+
				"command may still be taking effect", want, a.cfg.settleTimeout))
			a.log.Warn("not settled", "want", want.String(),
				"after", a.cfg.settleTimeout.String())
			return
		}
		time.Sleep(a.cfg.settleInterval)
	}
}

// askCtrlValue asks which documented control value to send.
// choice is one value a setter accepts, for the prompts that ask which to send.
type choice struct {
	value uint64
	label string
}

// choicesFor is what the menu offers in one group, as selectable values. Built
// from operations() so the combined command offers exactly what the individual
// entries do, with the same wording — including the raw value in each label, so
// a selection can be checked against the data sheet.
func choicesFor(group string) []choice {
	var out []choice
	for _, op := range operations() {
		if op.group == group && op.value != nil {
			out = append(out, choice{*op.value, op.label})
		}
	}
	return out
}

// unchangedKey is what an operator picks to leave a part of a combined command
// alone. It is a listed option rather than "press enter", so that leaving
// something unchanged is a choice made deliberately and visible in the log.
const unchangedKey = "-"

// askChoice asks which state one setter should be put into. With optional true
// the list carries an entry for leaving it unchanged, and chosen comes back
// false when that is picked; ok is false when the operator aborted or input
// ended.
func (a *app) askChoice(question string, choices []choice, optional bool) (
	value uint64, chosen, ok bool) {

	a.r.line("")
	a.r.line("  " + question)
	if optional {
		a.r.printf("  %-5s leave unchanged, write nothing for this one\n", unchangedKey)
	}
	for _, c := range choices {
		a.r.printf("  %-5d %s\n", c.value, c.label)
	}
	prompt := "value: "
	if optional {
		prompt = fmt.Sprintf("value, or %q to leave it unchanged: ", unchangedKey)
	}
	line, ok := a.ask(prompt)
	if !ok {
		return 0, false, false
	}
	if optional && (line == unchangedKey || line == "") {
		a.log.Info("value left unchanged", "setter", question)
		a.r.note("left unchanged")
		return 0, false, true
	}
	n, err := strconv.ParseUint(line, 10, 64)
	if err != nil {
		a.r.warn(fmt.Sprintf("%q is neither one of the values listed nor %q", line, unchangedKey))
		return 0, false, false
	}
	i := slices.IndexFunc(choices, func(c choice) bool { return c.value == n })
	if i < 0 {
		// The library refuses an undocumented value too, but saying so here
		// names the value the operator typed instead of failing the command.
		a.r.warn(fmt.Sprintf("%d is not one of the values listed", n))
		return 0, false, false
	}
	a.r.note("chose " + choices[i].label)
	a.log.Info("value chosen", "setter", question, "value", n, "label", choices[i].label)
	return n, true, true
}

// askCombined builds the parameter set for one session that writes several
// items. Each of the three is asked for separately and each can be left
// unchanged, so "only Ctrl and Rbh" is a selection rather than something to be
// inferred from a blank line. A set that writes nothing is refused here, since
// the library refuses it too.
func (a *app) askCombined() bool {
	var values energontrol.ControlAndRbhValue
	values.ForceExplicitCommand = a.force

	a.r.section("Which parameters should this session write?")
	a.r.note(fmt.Sprintf("Each of the three is asked separately. %q leaves one unchanged, "+
		"and nothing is written for it. forceExplicitCommand is %s.",
		unchangedKey, onOff(a.force)))

	v, chosen, ok := a.askChoice("Set control value (Ctrl/SetCtrl) to which state?",
		choicesFor(groupCtrl), true)
	if !ok {
		return false
	}
	if chosen {
		values.SetCtrlValue, values.CtrlValue = true, energontrol.CtrlValue(v)
		// What to wait for afterwards is only known now, so the operation itself
		// cannot carry it.
		expect := values.CtrlValue
		a.expectOverride = &expect
	}

	v, chosen, ok = a.askChoice("Set heating value (Ctrl/SetRbh) to which state?",
		choicesFor(groupRbh), true)
	if !ok {
		return false
	}
	if chosen {
		values.SetRbhValue, values.RbhValue = true, energontrol.RbhValue(v)
	}

	v, chosen, ok = a.askChoice("Set the ice warning lamp (Ctrl/SetIceDet) to which state?",
		choicesFor(groupIceDet), true)
	if !ok {
		return false
	}
	if chosen {
		values.SetIceDetValue, values.IceDetValue = true, energontrol.IceDetValue(v)
	}

	if !values.SetCtrlValue && !values.SetRbhValue && !values.SetIceDetValue {
		a.r.warn("all three were left unchanged, so there is nothing to send")
		return false
	}

	// Held for the confirmation screen, which lists all three together.
	a.combined = &values
	a.log.Info("combined command built",
		"setCtrl", values.SetCtrlValue, "ctrl", uint64(values.CtrlValue),
		"setRbh", values.SetRbhValue, "rbh", uint64(values.RbhValue),
		"setIceDet", values.SetIceDetValue, "iceDet", uint64(values.IceDetValue),
		"forceExplicitCommand", values.ForceExplicitCommand)
	return true
}

// combinedPart renders one part of a combined command for the summary.
func combinedPart(set bool, value uint64, group string) string {
	if !set {
		return "left unchanged"
	}
	for _, c := range choicesFor(group) {
		if c.value == value {
			return c.label
		}
	}
	return strconv.FormatUint(value, 10)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// ask prints a prompt and reads one line. The second result is false at end of
// input, which is how a piped run ends.
func (a *app) ask(prompt string) (string, bool) {
	a.r.printf("%s", prompt)
	if !a.in.Scan() {
		return "", false
	}
	return strings.TrimSpace(a.in.Text()), true
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func parsePlants(s string) ([]uint8, error) {
	var out []uint8
	for _, field := range strings.Split(s, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		n, err := strconv.ParseUint(field, 10, 8)
		if err != nil {
			return nil, fmt.Errorf("%q is not a plant number in 0..255", field)
		}
		if slices.Contains(out, uint8(n)) {
			return nil, fmt.Errorf("plant %d is listed more than once", n)
		}
		out = append(out, uint8(n))
	}
	return out, nil
}

// resultsTrace renders the per-plant results for the log.
func resultsTrace(res energontrol.Results) []string {
	out := make([]string, 0, len(res))
	for _, p := range res {
		out = append(out, fmt.Sprintf("plant=%d outcome=%s inRequestedState=%t err=%s",
			p.PlantNo, p.Outcome, p.InRequestedState(), errText(p.Err)))
	}
	return out
}

func orNone(s string) string {
	if s == "" {
		return "(not set)"
	}
	return s
}

func maxAgeLabel(d time.Duration) string {
	if d <= 0 {
		return "off (-max-age enables it)"
	}
	return d.String() + " (freshness demanded and checked)"
}
