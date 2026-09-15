package main

// The tool driven end to end against a SOAP server that answers like an Enercon
// SCADA, with the menu fed from a string.
//
// A tool whose whole purpose is to be pointed at real turbines has to be known
// to work before it is. So the read-only run, the confirmation gate and a
// completed command all run here against a simulated park — and the gate is
// checked from both sides: a refused confirmation must leave the server
// untouched, and an accepted one must write exactly the three items the session
// schema defines.

import (
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"net/http"

	"github.com/dernate/energontrol/v2"
)

// ---------------------------------------------------------------------------
// a SOAP server that answers like an Enercon SCADA
// ---------------------------------------------------------------------------

type fakeSCADA struct {
	mu sync.Mutex

	ctrl     map[uint8]uint64
	params   map[string][]uint64 // the last value written to each item
	sessions map[string]int      // "<plant>/<branch>" -> session state
	writes   []string
	reads    int
	paged    bool // page the Loc/Wec browse, to exercise the continuation path
}

func newFakeSCADA() *fakeSCADA {
	return &fakeSCADA{
		ctrl:     map[uint8]uint64{2: 0, 5: 2}, // plant 2 runs, plant 5 is stopped at 90°
		params:   map[string][]uint64{},
		sessions: map[string]int{},
	}
}

var (
	itemNameRE = regexp.MustCompile(`:Items ItemName="([^"]*)" ClientItemHandle="([^"]*)"`)
	itemPathRE = regexp.MustCompile(`ItemPath="([^"]*)"`)
	valueRE    = regexp.MustCompile(`<[^>]*:?unsignedInt>(\d+)<`)
	plantRE    = regexp.MustCompile(`Plant(\d+)`)
)

func (f *fakeSCADA) start(t *testing.T) *url.URL {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (f *fakeSCADA) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	payload := string(body)
	action := r.Header.Get("SOAPAction")
	w.Header().Set("Content-Type", "text/xml")

	switch {
	case strings.Contains(action, "GetStatus"):
		_, _ = fmt.Fprint(w, envelope(f.statusReply()))
	case strings.Contains(action, "Browse"):
		_, _ = fmt.Fprint(w, envelope(f.browseReply(payload)))
	case strings.Contains(action, "Write"):
		_, _ = fmt.Fprint(w, envelope(f.writeReply(payload)))
	case strings.Contains(action, "Read"):
		_, _ = fmt.Fprint(w, envelope(f.readReply(payload)))
	default:
		http.Error(w, "unexpected SOAPAction "+action, http.StatusInternalServerError)
	}
}

func (f *fakeSCADA) statusReply() string {
	now := time.Now().Format(time.RFC3339)
	return fmt.Sprintf(`<GetStatusResponse xmlns="http://opcfoundation.org/webservices/XMLDA/1.0/">`+
		`<GetStatusResult RcvTime="%s" ReplyTime="%s" ServerState="running"/><Status/>`+
		`</GetStatusResponse>`, now, now)
}

func (f *fakeSCADA) browseReply(payload string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := ""
	if m := itemPathRE.FindStringSubmatch(payload); m != nil {
		path = m[1]
	}
	now := time.Now().Format(time.RFC3339)
	elems, more, point := "", "false", ""

	switch {
	case path == "Loc/Wec":
		if f.paged {
			// One plant per page, keyed on the continuation point the way a real
			// server does, so the answer does not depend on how often the path
			// has been asked for.
			if strings.Contains(payload, `ContinuationPoint="p2"`) {
				elems = element("Plant5", "Loc/Wec/Plant5", true, false)
			} else {
				elems = element("Plant2", "Loc/Wec/Plant2", true, false)
				more, point = "true", ` ContinuationPoint="p2"`
			}
		} else {
			elems = element("Plant2", "Loc/Wec/Plant2", true, false) +
				element("Plant5", "Loc/Wec/Plant5", true, false)
		}
	case strings.HasSuffix(path, "/Ctrl"):
		elems = element("SetCtrl", path+"/SetCtrl", false, true)
	case strings.HasSuffix(path, "/Reset"):
		elems = element("SetReset", path+"/SetReset", false, true)
	case plantRE.MatchString(path):
		elems = element("Ctrl", path+"/Ctrl", true, false) +
			element("Reset", path+"/Reset", true, false)
	}
	return fmt.Sprintf(`<BrowseResponse xmlns="http://opcfoundation.org/webservices/XMLDA/1.0/" `+
		`MoreElements="%s"%s>`+
		`<BrowseResult RcvTime="%s" ReplyTime="%s" ServerState="running"/>%s</BrowseResponse>`,
		more, point, now, now, elems)
}

func element(name, itemName string, hasChildren, isItem bool) string {
	return fmt.Sprintf(`<Elements Name="%s" ItemName="%s" HasChildren="%t" IsItem="%t"/>`,
		name, itemName, hasChildren, isItem)
}

func (f *fakeSCADA) readReply(payload string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	now := time.Now().UTC().Format(time.RFC3339)

	var items strings.Builder
	faulted := false
	for _, m := range itemNameRE.FindAllStringSubmatch(payload, -1) {
		name, handle := m[1], m[2]
		plant := plantOf(name)
		if plant != 0 && f.ctrl[plant] == 0 && plant != 2 {
			// A plant the park does not list: the server has no such item, and
			// says so about the item rather than about the request.
			fmt.Fprintf(&items,
				`<Items ItemName="%s" ClientItemHandle="%s" ResultID="E_UNKNOWNITEMNAME"/>`,
				name, handle)
			faulted = true
			continue
		}
		if values, ok := f.arrayOf(name); ok {
			var elems strings.Builder
			for _, v := range values {
				fmt.Fprintf(&elems, `<xsd:unsignedInt>%d</xsd:unsignedInt>`, v)
			}
			fmt.Fprintf(&items,
				`<Items ItemName="%s" ClientItemHandle="%s" Timestamp="%s">`+
					`<Value xsi:type="xsd:ArrayOfUnsignedInt">%s</Value>`+
					`<Quality QualityField="good"/></Items>`, name, handle, now, elems.String())
			continue
		}
		fmt.Fprintf(&items,
			`<Items ItemName="%s" ClientItemHandle="%s" Timestamp="%s">`+
				`<Value xsi:type="xsd:unsignedLong">%d</Value>`+
				`<Quality QualityField="good"/></Items>`, name, handle, now, f.scalarOf(name))
	}
	extra := ""
	if faulted {
		// What a conformant server adds for the result id it used.
		extra = `<Errors ID="E_UNKNOWNITEMNAME"><Text>the item does not exist</Text></Errors>`
	}
	return fmt.Sprintf(`<ReadResponse xmlns="http://opcfoundation.org/webservices/XMLDA/1.0/">`+
		`<ReadResult RcvTime="%s" ReplyTime="%s" ServerState="running"/>`+
		`<RItemList>%s</RItemList>%s</ReadResponse>`, now, now, items.String(), extra)
}

// arrayOf answers the items Enercon types as arrays of long words.
func (f *fakeSCADA) arrayOf(name string) ([]uint64, bool) {
	switch {
	case strings.HasSuffix(name, "/SessionRequest"):
		// Zero means "this server does not report the session id", which the
		// library tolerates and logs.
		return []uint64{0, 0, 0}, true
	case strings.HasSuffix(name, "/SetCtrl"), strings.HasSuffix(name, "/SetReset"),
		strings.HasSuffix(name, "/SetRbh"), strings.HasSuffix(name, "/SetIceDet"):
		// The read-back of a written parameter. Before anything was written the
		// item reads as zeroes; afterwards it has to carry what was written, or
		// the library's verification of the write cannot pass.
		if v, ok := f.params[name]; ok {
			return v, true
		}
		return []uint64{0, 0, 0}, true
	}
	return nil, false
}

func (f *fakeSCADA) scalarOf(name string) uint64 {
	plant := plantOf(name)
	switch {
	case name == "Loc/LocNo":
		return 4242
	case strings.HasSuffix(name, "/SessionState"):
		key := sessionKey(name)
		state := f.sessions[key]
		if state == 4 {
			// The end of the procedure is reported once, and then the session is
			// free again — a server does not hold a finished session forever.
			// Without this the fake refuses every command after the first.
			f.sessions[key] = 0
		}
		return uint64(state)
	case strings.HasSuffix(name, "/SessionPubKey"):
		return 4711
	case strings.HasSuffix(name, "/SessionTimeOut"):
		return 60
	case strings.HasSuffix(name, "/Ctrl/Rbh"):
		return 1 << 15 // heating installed, nothing running
	case strings.HasSuffix(name, "/Ctrl/IceDet"):
		return 0
	case strings.HasSuffix(name, "/Ctrl/Ctrl"):
		return f.ctrl[plant]
	}
	return 0
}

func (f *fakeSCADA) writeReply(payload string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().Format(time.RFC3339)

	var items strings.Builder
	bounds := itemNameRE.FindAllStringSubmatchIndex(payload, -1)
	for i, m := range bounds {
		name, handle := payload[m[2]:m[3]], payload[m[4]:m[5]]
		end := len(payload)
		if i+1 < len(bounds) {
			end = bounds[i+1][0]
		}
		f.writes = append(f.writes, name)
		f.params[name] = valuesIn(payload[m[1]:end])
		key := sessionKey(name)
		switch {
		case strings.HasSuffix(name, "/SessionRequest"):
			f.sessions[key] = 1
		case strings.HasSuffix(name, "/SetReset"), strings.HasSuffix(name, "/SetCtrl"),
			strings.HasSuffix(name, "/SetRbh"), strings.HasSuffix(name, "/SetIceDet"):
			f.sessions[key] = 2
		case strings.HasSuffix(name, "/SessionSubmit"):
			f.sessions[key] = 4
			// Submitting is when the plant acts on the parameter, so this is
			// where the reported state changes.
			f.applyOnSubmit(key)
		}
		fmt.Fprintf(&items, `<Items ItemName="%s" ClientItemHandle="%s"/>`, name, handle)
	}
	return fmt.Sprintf(`<WriteResponse xmlns="http://opcfoundation.org/webservices/XMLDA/1.0/">`+
		`<WriteResult RcvTime="%s" ReplyTime="%s" ServerState="running"/>`+
		`<RItemList>%s</RItemList></WriteResponse>`, now, now, items.String())
}

// valuesIn pulls the elements of an ArrayOfUnsignedInt out of one item of a
// write payload.
func valuesIn(chunk string) []uint64 {
	out := []uint64{}
	for _, m := range valueRE.FindAllStringSubmatch(chunk, -1) {
		v, err := strconv.ParseUint(m[1], 10, 32)
		if err != nil {
			continue
		}
		out = append(out, v)
	}
	return out
}

// applyOnSubmit moves the plant into the state the submitted parameter asks for.
// Called with f.mu held.
func (f *fakeSCADA) applyOnSubmit(key string) {
	name := "Loc/Wec/" + key + "/SetCtrl"
	v, ok := f.params[name]
	if !ok || len(v) == 0 {
		return
	}
	// Ctrl carries the value that was set: the plant reports the command it was
	// given and holds it, which is what a real park does.
	f.ctrl[plantOf(name)] = v[0]
}

// sessionKey is the plant and branch an item belongs to, so the session state
// of Ctrl and Reset are tracked apart.
func sessionKey(name string) string {
	parts := strings.Split(name, "/")
	if len(parts) < 4 {
		return name
	}
	return parts[2] + "/" + parts[3]
}

func plantOf(name string) uint8 {
	m := plantRE.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	var n int
	if _, err := fmt.Sscanf(m[1], "%d", &n); err != nil || n < 0 || n > 255 {
		return 0
	}
	return uint8(n)
}

func envelope(inner string) string {
	return `<?xml version="1.0" encoding="utf-8"?>` +
		`<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://schemas.xmlsoap.org/soap/envelope/" ` +
		`xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" ` +
		`xmlns:xsd="http://www.w3.org/2001/XMLSchema">` +
		`<SOAP-ENV:Body>` + inner + `</SOAP-ENV:Body></SOAP-ENV:Envelope>`
}

func (f *fakeSCADA) wrote() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...)
}

// ---------------------------------------------------------------------------
// driving the tool
// ---------------------------------------------------------------------------

// probeRun feeds the menu from a string and returns what the operator saw and
// what went into the log.
func probeRun(t *testing.T, scada *fakeSCADA, cfg config, input string) (
	out string, logged string, err error) {

	t.Helper()
	var screen, logbuf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg.url = scada.start(t).String()
	if cfg.timeout == 0 {
		cfg.timeout = 5 * time.Second
	}
	if cfg.settleInterval == 0 {
		cfg.settleInterval = time.Millisecond
	}
	if cfg.settleTimeout == 0 {
		cfg.settleTimeout = 50 * time.Millisecond
	}
	err = run(cfg, strings.NewReader(input), &screen, logger)
	return screen.String(), logbuf.String(), err
}

// A run with nothing selected reads, reports and refuses to command.
func TestReadOnlyRunReportsTheParkAndRefusesToCommand(t *testing.T) {
	scada := newFakeSCADA()
	out, logged, err := probeRun(t, scada, config{samples: 3}, "q\n")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	t.Log("\n" + out)

	for _, want := range []string{
		"energontrol scadaprobe",
		`the server answers and reports state "running"`,
		"Park number", "4242",
		"Plants", "[2 5]",
		"Plant states",
		"Menu",
		"No command can be sent",
		"PARKNO",
		"quitting",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not mention %q", want)
		}
	}
	if w := scada.wrote(); len(w) != 0 {
		t.Errorf("a read-only run wrote %v", w)
	}
	if !strings.Contains(logged, `"msg":"session started"`) {
		t.Error("the log does not record the session start")
	}
	if !strings.Contains(logged, `"msg":"park listed"`) {
		t.Error("the log does not record the park listing")
	}
}

// Nothing is sent until the operation's own name is typed back.
func TestConfirmationGateRefusesAnythingElse(t *testing.T) {
	for name, answer := range map[string]string{
		"a plain yes":    "yes",
		"an empty line":  "",
		"the wrong name": "start",
		"the menu key":   "2",
		"end of input":   "",
	} {
		t.Run(name, func(t *testing.T) {
			scada := newFakeSCADA()
			input := "2\n" + answer + "\nq\n"
			if name == "end of input" {
				input = "2\n" // the confirmation prompt hits EOF
			}
			out, logged, err := probeRun(t, scada, config{
				samples: 0, park: "4242", user: "1234", plants: "2",
			}, input)
			if err != nil {
				t.Fatalf("run: %v\n%s", err, out)
			}
			if !strings.Contains(out, "aborted — nothing was sent") {
				t.Errorf("the run did not report the abort:\n%s", out)
			}
			if w := scada.wrote(); len(w) != 0 {
				t.Errorf("an unconfirmed command wrote %v", w)
			}
			if !strings.Contains(logged, `"msg":"command aborted"`) {
				t.Error("the log does not record the abort")
			}
		})
	}
}

// A confirmed reset runs the session to completion and writes exactly the three
// items the schema defines. Reset is the command to drive here: it has no
// documented resulting state, so it reports without waiting for one.
func TestConfirmedResetRunsTheSession(t *testing.T) {
	scada := newFakeSCADA()
	out, logged, err := probeRun(t, scada, config{
		samples: 0, park: "4242", user: "1234", plants: "2",
	}, "16\nreset\nq\n")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	t.Log("\n" + out)

	for _, want := range []string{
		"the server confirms park 4242",
		"Confirm",
		"acknowledge faults (SetReset)",
		`type "reset" to send it`,
		"Result",
		"plant 2: commanded",
		"Nothing was restored",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not mention %q", want)
		}
	}

	wrote := scada.wrote()
	want := []string{
		"Loc/Wec/Plant2/Reset/SessionRequest",
		"Loc/Wec/Plant2/Reset/SetReset",
		"Loc/Wec/Plant2/Reset/SessionSubmit",
	}
	if len(wrote) != len(want) {
		t.Fatalf("wrote %v, want exactly the three session items %v", wrote, want)
	}
	for i, w := range want {
		if wrote[i] != w {
			t.Errorf("write %d was %s, want %s", i, wrote[i], w)
		}
	}

	// The log has to hold the whole story: what was offered, what was
	// confirmed, every write, and the outcome.
	for _, want := range []string{
		`"msg":"command offered"`,
		`"msg":"command sending"`,
		`"msg":"opc Write"`,
		`"msg":"command finished"`,
		`"inRequestedState":true`,
		"Loc/Wec/Plant2/Reset/SetReset",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("the log does not contain %q", want)
		}
	}
}

// A command is refused unless all three of park, plants and user id are there,
// whatever the operator types.
func TestCommandNeedsParkPlantsAndUser(t *testing.T) {
	for name, cfg := range map[string]config{
		"no park":   {samples: 0, user: "1234", plants: "2"},
		"no plants": {samples: 0, park: "4242", user: "1234"},
		"no user":   {samples: 0, park: "4242", plants: "2"},
	} {
		t.Run(name, func(t *testing.T) {
			scada := newFakeSCADA()
			out, _, err := probeRun(t, scada, cfg, "16\nreset\nq\n")
			if err != nil {
				t.Fatalf("run: %v\n%s", err, out)
			}
			if !strings.Contains(out, "No command can be sent") {
				t.Errorf("the run did not refuse the command:\n%s", out)
			}
			if w := scada.wrote(); len(w) != 0 {
				t.Errorf("wrote %v despite a missing guard", w)
			}
		})
	}
}

// The park number is verified before the menu opens, and a mismatch takes every
// command off the table without ending the run.
func TestWrongParkDisablesCommands(t *testing.T) {
	scada := newFakeSCADA()
	out, logged, err := probeRun(t, scada, config{
		samples: 0, park: "9999", user: "1234", plants: "2",
	}, "16\nreset\nq\n")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "does not serve park 9999") {
		t.Errorf("the run did not report the park mismatch:\n%s", out)
	}
	if w := scada.wrote(); len(w) != 0 {
		t.Errorf("wrote %v against the wrong park", w)
	}
	if !strings.Contains(logged, `"confirmed":false`) {
		t.Error("the log does not record that the park was not confirmed")
	}
}

// The diagnosis writes nothing and answers the questions the library's
// contracts rest on.
func TestDiagnoseWritesNothing(t *testing.T) {
	scada := newFakeSCADA()
	out, logged, err := probeRun(t, scada, config{samples: 3}, "d\nq\n")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	for _, want := range []string{
		"Server behaviour",
		"every item carried a timestamp",
		"every reply carried a ServerState",
		"it does not page",
		"reported per item",
		"Latency (3 round trips",
		"Recommended WithSessionPolling budget",
		"Requests so far",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the diagnosis does not mention %q", want)
		}
	}
	if w := scada.wrote(); len(w) != 0 {
		t.Errorf("the diagnosis wrote %v", w)
	}
	if !strings.Contains(logged, `"msg":"latency measured"`) {
		t.Error("the log does not record the latency measurement")
	}
}

// A server that pages its park listing is followed to the end, and the
// diagnosis says so rather than quietly listing half a park.
func TestPagedParkListingIsComplete(t *testing.T) {
	scada := newFakeSCADA()
	scada.paged = true
	out, _, err := probeRun(t, scada, config{samples: 0}, "d\nq\n")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "[2 5]") {
		t.Errorf("the paged listing lost a plant:\n%s", out)
	}
	if !strings.Contains(out, "pages its park listing") {
		t.Errorf("the diagnosis does not mention the paging:\n%s", out)
	}
}

// The plant selection can be changed from the menu, and a plant the park does
// not list is refused.
func TestSelectionCanBeChanged(t *testing.T) {
	scada := newFakeSCADA()
	out, _, err := probeRun(t, scada, config{samples: 0, park: "4242", user: "1234"},
		"p\n5\np\n99\nq\n")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "selection is now [5]") {
		t.Errorf("the selection was not changed:\n%s", out)
	}
	if !strings.Contains(out, "the park does not list [99]") {
		t.Errorf("an unlisted plant was accepted:\n%s", out)
	}
}

func TestUnknownChoiceIsReportedAndTheMenuStays(t *testing.T) {
	scada := newFakeSCADA()
	out, _, err := probeRun(t, scada, config{samples: 0}, "zz\nq\n")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no such choice: zz") {
		t.Errorf("an unknown choice was not reported:\n%s", out)
	}
	if !strings.Contains(out, "quitting") {
		t.Errorf("the menu did not survive an unknown choice:\n%s", out)
	}
}

func TestRunNeedsAnEndpoint(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	var out strings.Builder
	if err := run(config{}, strings.NewReader(""), &out, logger); err == nil {
		t.Fatal("a run without an endpoint was not refused")
	}
}

func TestBadEnvironmentIsReported(t *testing.T) {
	scada := newFakeSCADA()
	for name, cfg := range map[string]config{
		"plants":  {plants: "two"},
		"user id": {user: "99999999999", plants: "2"},
		"park":    {park: "the big one", plants: "2"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := probeRun(t, scada, cfg, "q\n"); err == nil {
				t.Fatalf("accepted: %+v", cfg)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// the operation table
// ---------------------------------------------------------------------------

// Every operation needs a unique key and a unique confirmation word, or the
// menu would send something other than what was chosen.
func TestOperationsAreUnambiguous(t *testing.T) {
	keys, names := map[string]bool{}, map[string]bool{}
	for _, op := range operations() {
		if op.key == "" || op.name == "" || op.label == "" {
			t.Errorf("incomplete operation: %+v", op)
		}
		if keys[op.key] {
			t.Errorf("duplicate key %q", op.key)
		}
		if names[op.name] {
			t.Errorf("duplicate confirmation word %q", op.name)
		}
		keys[op.key], names[op.name] = true, true
		if op.send == nil {
			t.Errorf("%s sends nothing", op.name)
		}
		// A confirmation word that is also a menu key would let a stray keypress
		// confirm an operation.
		if keys[op.name] && op.name != op.key {
			t.Errorf("%q is both a confirmation word and a menu key", op.name)
		}
		for _, reserved := range []string{"s", "d", "p", "q", "f"} {
			if op.key == reserved || op.name == reserved {
				t.Errorf("%q collides with the reserved choice %q", op.name, reserved)
			}
		}
	}
	// Every stop locks a new reservation for 360 s, so every one of them has to
	// say so. And every control operation waits for the value it sent, because
	// Ctrl carries the value that was set. Waiting for the blade angle that value
	// produces — which this tool used to do — never matched.
	for _, op := range operations() {
		if op.group != groupCtrl {
			continue
		}
		if op.name != "start" && !strings.Contains(op.note, "360") {
			t.Errorf("%s does not warn about the re-reservation delay", op.name)
		}
		if op.expect == nil {
			t.Errorf("%s has nothing to wait for", op.name)
			continue
		}
		if !strings.Contains(op.label, fmt.Sprintf("(SetCtrl %d)", uint64(*op.expect))) {
			t.Errorf("%s waits for %s, which is not the value its label says it sends: %s",
				op.name, *op.expect, op.label)
		}
	}
	// Only the control operations take forceExplicitCommand, plus the combined
	// one, which can carry a control value.
	for _, op := range operations() {
		if op.usesForce && op.group != groupCtrl && op.group != groupCombine {
			t.Errorf("%s claims to use forceExplicitCommand but sends no control value",
				op.name)
		}
	}
}

// The menu has to offer every command the library can send, or an operator
// cannot test one of them against a real server. The documented value sets are
// the yardstick: every value Enercon defines for SetCtrl, SetRbh and SetIceDet
// is reachable, and nothing above them — those are states a plant reports.
func TestEveryDocumentedValueIsOnTheMenu(t *testing.T) {
	ops := operations()
	labels := func(group string) string {
		var b strings.Builder
		for _, op := range ops {
			if op.group == group {
				b.WriteString(op.label + "\n")
			}
		}
		return b.String()
	}

	ctrl := labels(groupCtrl)
	for v := energontrol.CtrlStart; v.Writable(); v++ {
		if !strings.Contains(ctrl, fmt.Sprintf("(SetCtrl %d)", uint64(v))) {
			t.Errorf("no menu entry writes SetCtrl %d (%s)", uint64(v), v)
		}
	}
	if n := strings.Count(ctrl, "(SetCtrl "); n != 9 {
		t.Errorf("the control group has %d entries, want the 9 documented values:\n%s", n, ctrl)
	}
	// The control group is listed in data-sheet order, so a selection can be
	// checked against the data sheet by reading down the menu.
	for i, want := range []string{"0", "1", "2", "3", "4", "5", "6", "7", "8"} {
		marker := fmt.Sprintf("(SetCtrl %s)", want)
		if got := strings.Index(ctrl, marker); got < 0 {
			t.Errorf("SetCtrl %s missing", want)
		} else if i > 0 {
			prev := fmt.Sprintf("(SetCtrl %d)", i-1)
			if strings.Index(ctrl, prev) > got {
				t.Errorf("SetCtrl %s is listed before %s", want, prev)
			}
		}
	}

	rbh := labels(groupRbh)
	for _, c := range rbhChoices() {
		if !strings.Contains(rbh, fmt.Sprintf("(SetRbh %d)", c.value)) {
			t.Errorf("no menu entry writes SetRbh %d (%s)", c.value, c.name)
		}
	}
	lamp := labels(groupIceDet)
	for _, c := range iceDetChoices() {
		if !strings.Contains(lamp, fmt.Sprintf("(SetIceDet %d)", c.value)) {
			t.Errorf("no menu entry writes SetIceDet %d (%s)", c.value, c.name)
		}
	}

	// Reset and the combined command have no value of their own, so they are
	// checked by being present at all.
	for _, group := range []string{groupReset, groupCombine} {
		if labels(group) == "" {
			t.Errorf("no menu entry for %q", group)
		}
	}
}

func TestParsePlants(t *testing.T) {
	got, err := parsePlants(" 2, 5 ,11")
	if err != nil {
		t.Fatalf("parsePlants: %v", err)
	}
	if len(got) != 3 || got[0] != 2 || got[1] != 5 || got[2] != 11 {
		t.Errorf("plants = %v, want [2 5 11]", got)
	}
	if got, err := parsePlants(""); err != nil || len(got) != 0 {
		t.Errorf("empty = %v, %v; want no plants and no error", got, err)
	}
	for _, bad := range []string{"300", "two", "2,2"} {
		if _, err := parsePlants(bad); err == nil {
			t.Errorf("parsePlants(%q) was accepted", bad)
		}
	}
}

// The user id is the operator's credential for the park. A log file gets copied
// into tickets and mails, so it must not be in there — not as a field of its
// own, and not buried in the SessionRequest array, where the Enercon schema
// puts it between the session id and the private key.
func TestTheLogNeverCarriesTheUserID(t *testing.T) {
	const userID = "987654321"

	scada := newFakeSCADA()
	out, logged, err := probeRun(t, scada, config{
		samples: 0, park: "4242", user: userID, plants: "2",
	}, "16\nreset\nq\n")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "plant 2: commanded") {
		t.Fatalf("the command did not run, so the test proves nothing:\n%s", out)
	}

	if strings.Contains(logged, userID) {
		for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
			if strings.Contains(line, userID) {
				t.Errorf("the log carries the user id: %s", line)
			}
		}
	}
	if strings.Contains(logged, `"userID"`) {
		t.Error("the log still has a userID field")
	}

	// It is redacted in place rather than dropped, so the array still reads as
	// the data sheet defines it and the other two elements stay checkable.
	if !strings.Contains(logged, "<userid redacted>") {
		t.Error("the SessionRequest write trace does not mark where the user id was")
	}
	if !strings.Contains(logged, "Loc/Wec/Plant2/Reset/SessionRequest") {
		t.Error("the log lost the SessionRequest write altogether")
	}
}

// The scenario the first live run surfaced, through the tool that surfaced it:
// a plant standing at 60° and a full stop asked for. It has to send the command
// and report the plant commanded, not report it as already stopped.
func TestFullStopReachesAPlantStandingAt60(t *testing.T) {
	scada := newFakeSCADA()
	scada.ctrl[2] = 1 // stopped at 60°

	out, logged, err := probeRun(t, scada, config{
		samples: 0, park: "4242", user: "1234", plants: "2",
	}, "3\nstop90\nq\n")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	t.Log("\n" + out)

	if !strings.Contains(out, "plant 2: commanded") {
		t.Errorf("a plant at 60° was not commanded to 90°:\n%s", out)
	}
	if strings.Contains(out, "already in state") {
		t.Errorf("a plant at 60° was reported as already fully stopped:\n%s", out)
	}
	if !slices.Contains(scada.wrote(), "Loc/Wec/Plant2/Ctrl/SetCtrl") {
		t.Errorf("SetCtrl was never written, so the blades stay at 60°; writes: %v", scada.wrote())
	}
	if got := energontrol.CtrlValue(scada.ctrl[2]); !got.Reached(energontrol.CtrlStop90) {
		t.Errorf("plant ends at %s, want the 90° full stop", got)
	}
	if !strings.Contains(logged, "Loc/Wec/Plant2/Ctrl/SetCtrl") {
		t.Error("the log does not hold the write, so the run cannot be analysed")
	}
}

// A stop the library only reaches through SetCtrl, picked straight from the
// menu: it has to open a session, write the documented value, and end at the
// blade angle the value names.
func TestSpeciesProtectionStopRunsFromTheMenu(t *testing.T) {
	scada := newFakeSCADA()

	out, logged, err := probeRun(t, scada, config{
		samples: 0, park: "4242", user: "1234", plants: "2",
	}, "9\nspecies90\nq\n")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	t.Log("\n" + out)

	if !strings.Contains(out, "stop for species protection at 90° (SetCtrl 8)") {
		t.Errorf("the menu does not offer the species-protection stop at 90°:\n%s", out)
	}
	if !strings.Contains(out, "plant 2: commanded") {
		t.Errorf("the command did not go through:\n%s", out)
	}
	if !slices.Contains(scada.wrote(), "Loc/Wec/Plant2/Ctrl/SetCtrl") {
		t.Errorf("SetCtrl was never written; writes: %v", scada.wrote())
	}
	if got := scada.params["Loc/Wec/Plant2/Ctrl/SetCtrl"]; len(got) == 0 || got[0] != 8 {
		t.Errorf("SetCtrl carried %v, want the documented value 8 first", got)
	}
	// Ctrl carries the value that was set, so the plant reports 8 — not the 90°
	// blade angle that stop produces.
	if got := energontrol.CtrlValue(scada.ctrl[2]); !got.Reached(energontrol.CtrlStopSpeciesProtection90) {
		t.Errorf("plant ends at %s, which is not the species-protection stop at 90°", got)
	}
	if !strings.Contains(logged, `"value":8`) && !strings.Contains(logged, "SetCtrl") {
		t.Error("the log does not record the value that was sent")
	}
}

// The force toggle has to reach the library, not just the screen. A plant
// Enercon stopped at 60° satisfies an unforced request for a 60° stop and can
// never satisfy a forced one, so the two runs must differ.
func TestForceToggleReachesTheCommand(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  string
		deny  string
	}{
		{"unforced", "2\nstop60\nq\n", "already in state", "not permitted"},
		{"forced", "f\n2\nstop60\nq\n", "not permitted", "already in state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scada := newFakeSCADA()
			scada.ctrl[2] = 129 // stopped at 60° by Enercon

			out, _, err := probeRun(t, scada, config{
				samples: 0, park: "4242", user: "1234", plants: "2",
			}, tc.input)
			if err != nil {
				t.Fatalf("run: %v\n%s", err, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("the result does not say %q:\n%s", tc.want, out)
			}
			if strings.Contains(out, tc.deny) {
				t.Errorf("the result still says %q:\n%s", tc.deny, out)
			}
			if len(scada.wrote()) != 0 {
				t.Errorf("a plant under Enercon control must not be commanded; writes: %v",
					scada.wrote())
			}
		})
	}
}

// The combined command is the only way to change several parameters in one
// session. Each part is optional, and the parts that were chosen have to end up
// in the same session as one another.
func TestCombinedCommandWritesTheChosenParametersInOneSession(t *testing.T) {
	scada := newFakeSCADA()

	// Confirmation first, then the three value prompts: a control value, a
	// heating value, and the lamp left alone.
	out, logged, err := probeRun(t, scada, config{
		samples: 0, park: "4242", user: "1234", plants: "2",
	}, "17\ncombined\n2\n10\n\nq\n")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	t.Log("\n" + out)

	wrote := scada.wrote()
	for _, want := range []string{
		"Loc/Wec/Plant2/Ctrl/SessionRequest",
		"Loc/Wec/Plant2/Ctrl/SetCtrl",
		"Loc/Wec/Plant2/Ctrl/SetRbh",
		"Loc/Wec/Plant2/Ctrl/SessionSubmit",
	} {
		if !slices.Contains(wrote, want) {
			t.Errorf("%s was not written; writes: %v", want, wrote)
		}
	}
	if slices.Contains(wrote, "Loc/Wec/Plant2/Ctrl/SetIceDet") {
		t.Errorf("the lamp was left alone, so SetIceDet must not be written; writes: %v", wrote)
	}
	// One session, not one per parameter.
	if n := strings.Count(strings.Join(wrote, " "), "SessionRequest"); n != 1 {
		t.Errorf("%d sessions were opened, want exactly one; writes: %v", n, wrote)
	}
	if got := scada.params["Loc/Wec/Plant2/Ctrl/SetCtrl"]; len(got) == 0 || got[0] != 2 {
		t.Errorf("SetCtrl carried %v, want 2 first", got)
	}
	if got := scada.params["Loc/Wec/Plant2/Ctrl/SetRbh"]; len(got) == 0 || got[0] != 10 {
		t.Errorf("SetRbh carried %v, want 10 first", got)
	}
	if !strings.Contains(logged, "combined command built") {
		t.Error("the log does not record what the combined command was built from")
	}
}

// A combined command that selects nothing writes nothing: the library refuses an
// empty parameter set, and the tool says so before getting there.
func TestCombinedCommandWithNothingChosenSendsNothing(t *testing.T) {
	scada := newFakeSCADA()

	out, _, err := probeRun(t, scada, config{
		samples: 0, park: "4242", user: "1234", plants: "2",
	}, "17\ncombined\n\n\n\nq\n")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "nothing was chosen") {
		t.Errorf("the tool does not say that nothing was chosen:\n%s", out)
	}
	if len(scada.wrote()) != 0 {
		t.Errorf("nothing may be written; writes: %v", scada.wrote())
	}
}

// A value outside the documented set is refused at the prompt, naming what was
// typed, rather than being handed to the library to fail on.
func TestAValueOutsideTheDocumentedSetIsRefusedAtThePrompt(t *testing.T) {
	scada := newFakeSCADA()

	out, _, err := probeRun(t, scada, config{
		samples: 0, park: "4242", user: "1234", plants: "2",
	}, "17\ncombined\n130\nq\n")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "130 is not one of the values listed") {
		t.Errorf("the prompt does not refuse 130 by name:\n%s", out)
	}
	if len(scada.wrote()) != 0 {
		t.Errorf("nothing may be written; writes: %v", scada.wrote())
	}
}

// The live run this was written from. Ctrl carries the value that was set: the
// species-protection stop went through, the plant read 7 for a minute, and the
// tool kept waiting for 1 and then warned. It has to recognise the stop instead
// — and the plant has to be startable afterwards.
func TestAParkReportingTheValueThatWasSetIsUnderstood(t *testing.T) {
	scada := newFakeSCADA() // reports the value that was set, as a real park does

	out, logged, err := probeRun(t, scada, config{
		samples: 0, park: "4242", user: "1234", plants: "2",
	}, "8\nspecies60\nq\n")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	t.Log("\n" + out)

	if got := scada.ctrl[2]; got != 7 {
		t.Fatalf("the park reports Ctrl=%d, want the echoed command value 7", got)
	}
	// Anchored on the pass marker: a bare substring check would also match the
	// warning, which reads "not every plant has carried out ...".
	if !strings.Contains(out, "[ ok ]   every plant has carried out StopSpeciesProtection60") {
		t.Errorf("the stop was not recognised as carried out:\n%s", out)
	}
	if strings.Contains(out, "not every plant has carried out") {
		t.Errorf("the tool warned although the plant reported the stop:\n%s", out)
	}
	if !strings.Contains(logged, "settled") {
		t.Error("the log does not record that the command settled")
	}

	// And the plant can be started again from that state. This is the second
	// symptom of the same wrong assumption: a state the package believed
	// impossible was refused as unknown.
	out, _, err = probeRun(t, scada, config{
		samples: 0, park: "4242", user: "1234", plants: "2",
	}, "1\nstart\nq\n")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "plant 2: commanded") {
		t.Errorf("a plant reporting StopSpeciesProtection60 could not be started:\n%s", out)
	}
	if strings.Contains(out, "does not know") {
		t.Errorf("the state was treated as unknown:\n%s", out)
	}
	if got := scada.ctrl[2]; got != 0 {
		t.Errorf("plant ends at Ctrl=%d, want 0 (running)", got)
	}
}
