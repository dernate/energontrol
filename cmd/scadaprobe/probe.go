package main

// The transport observer and the diagnostic probes.
//
// The observer is a decorator on energontrol.Transport, which is what the port
// is for: every request the library makes is recorded and logged without the
// library knowing it is being watched. That recording is the protocol trace in
// the log file — for a command, it is the complete list of what went to the
// server and what came back.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/dernate/energontrol/v2"
	"github.com/dernate/gopcxmlda"
)

// parkBranchPath is where an Enercon SCADA lists its plants.
const parkBranchPath = "Loc/Wec"

// ---------------------------------------------------------------------------
// the observer
// ---------------------------------------------------------------------------

// observer wraps a Transport, logs every request and remembers what the server
// answered, so the report can say how it behaves and the log holds the trace.
type observer struct {
	inner energontrol.Transport
	log   *slog.Logger

	mu           sync.Mutex
	reads        int
	writes       int
	browses      int
	written      []string
	itemsSeen    int
	itemsStamped int
	qualities    map[string]int
	repliesNoSt  int
}

var _ energontrol.Transport = (*observer)(nil)

func (o *observer) Status(ctx context.Context) (string, error) {
	start := time.Now()
	state, err := o.inner.Status(ctx)
	o.log.Debug("opc GetStatus", "serverState", state, "took", took(start), "err", errText(err))
	return state, err
}

func (o *observer) Read(ctx context.Context, names []string,
	opts energontrol.ReadOptions) (energontrol.Response, error) {

	start := time.Now()
	resp, err := o.inner.Read(ctx, names, opts)

	o.mu.Lock()
	o.reads++
	o.recordReply(resp)
	o.mu.Unlock()

	o.log.Debug("opc Read", "items", names, "maxAge", opts.MaxAge.String(),
		"took", took(start), "serverState", resp.ServerState,
		"answered", itemTrace(resp.Items), "err", errText(err))
	return resp, err
}

func (o *observer) Write(ctx context.Context, items []energontrol.ItemWrite) (
	energontrol.Response, error) {

	// A write is the only thing that can change a turbine, so it is logged at
	// info level: the trace of a command has to be legible without turning the
	// whole log up to debug.
	names := make([]string, 0, len(items))
	values := make([]string, 0, len(items))
	for _, it := range items {
		names = append(names, it.Name)
		values = append(values, loggableValue(it))
	}
	start := time.Now()
	resp, err := o.inner.Write(ctx, items)

	o.mu.Lock()
	o.writes++
	o.written = append(o.written, names...)
	o.recordReply(resp)
	o.mu.Unlock()

	o.log.Info("opc Write", "items", names, "values", values, "took", took(start),
		"serverState", resp.ServerState, "confirmed", itemTrace(resp.Items),
		"err", errText(err))
	return resp, err
}

func (o *observer) Browse(ctx context.Context, path string,
	filter energontrol.BrowseFilter) ([]energontrol.Node, error) {

	start := time.Now()
	nodes, err := o.inner.Browse(ctx, path, filter)

	o.mu.Lock()
	o.browses++
	o.mu.Unlock()

	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, n.Name)
	}
	o.log.Debug("opc Browse", "path", path, "branchesOnly", filter.BranchesOnly,
		"namePattern", filter.NamePattern, "took", took(start),
		"elements", names, "err", errText(err))
	return nodes, err
}

// recordReply is called with o.mu held.
func (o *observer) recordReply(resp energontrol.Response) {
	if strings.TrimSpace(resp.ServerState) == "" {
		o.repliesNoSt++
	}
	if o.qualities == nil {
		o.qualities = map[string]int{}
	}
	for _, it := range resp.Items {
		if it.ResultID != "" {
			continue
		}
		o.itemsSeen++
		if !it.Timestamp.IsZero() {
			o.itemsStamped++
		}
		q := it.Quality
		if q == "" {
			q = "(none reported, which the specification reads as good)"
		}
		o.qualities[q]++
	}
}

func (o *observer) counts() (reads, writes, browses int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.reads, o.writes, o.browses
}

func (o *observer) writtenItems() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.written)
}

func (o *observer) timestamps() (stamped, total int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.itemsStamped, o.itemsSeen
}

func (o *observer) repliesWithoutServerState() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.repliesNoSt
}

func (o *observer) qualitySummary() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	keys := make([]string, 0, len(o.qualities))
	for q := range o.qualities {
		keys = append(keys, q)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, q := range keys {
		parts = append(parts, fmt.Sprintf("%s (%d)", q, o.qualities[q]))
	}
	return strings.Join(parts, ", ")
}

// itemTrace renders what the server answered for each item, for the log.
func itemTrace(items []energontrol.ItemResult) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		switch {
		case it.Err != nil:
			out = append(out, fmt.Sprintf("%s=<undecodable: %v>", it.Name, it.Err))
		case it.ResultID != "":
			out = append(out, fmt.Sprintf("%s=<fault %s>", it.Name, it.ResultID))
		default:
			out = append(out, fmt.Sprintf("%s=%v q=%s", it.Name, it.Values, it.Quality))
		}
	}
	return out
}

// loggableValue renders a written value for the log with the user id removed.
//
// Enercon lays SessionRequest out as [session id, user id, private key], so the
// user id travels in the middle of the one array this tool writes that carries
// it. It is the operator's credential for the park, and a log file gets copied
// around and pasted into tickets — so it does not go in, and the position is
// kept rather than dropped so the array still reads as the schema defines it.
func loggableValue(it energontrol.ItemWrite) string {
	if !strings.HasSuffix(it.Name, "/SessionRequest") || len(it.Value) < 2 {
		return fmt.Sprintf("%v", it.Value)
	}
	parts := make([]string, 0, len(it.Value))
	for i, v := range it.Value {
		if i == userIDElementOfSessionRequest {
			parts = append(parts, "<userid redacted>")
			continue
		}
		parts = append(parts, strconv.FormatUint(uint64(v), 10))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// userIDElementOfSessionRequest is where the user id sits in a SessionRequest
// array, per the ENERCON technical data sheet.
const userIDElementOfSessionRequest = 1

func took(start time.Time) string { return time.Since(start).Round(time.Millisecond).String() }

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---------------------------------------------------------------------------
// the probes
// ---------------------------------------------------------------------------

// overview reads the park and the plant states. It writes nothing.
func (a *app) overview(ctx context.Context) error {
	a.r.section("Server")
	start := time.Now()
	if err := a.client.ServerAvailable(ctx); err != nil {
		a.r.fail("ServerAvailable: " + err.Error())
		return err
	}
	a.r.kv("GetStatus", took(start))
	a.r.pass(`the server answers and reports state "running"`)

	a.r.section("Park")
	info, err := a.client.Turbines(ctx)
	if err != nil {
		a.r.fail("Turbines: " + err.Error())
		if errors.Is(err, energontrol.ErrBrowseIncomplete) {
			a.r.note("the server paged its browse answer and the paging could not be " +
				"followed, so the park listing would have been incomplete")
		}
		return err
	}
	a.info = info
	a.r.kv("Park number", strconv.FormatUint(info.ParkNo, 10))
	a.r.kv("Plants", fmt.Sprintf("%d %v", len(info.PlantNo), info.PlantNo))
	a.r.table([]string{"plant", "ctrl", "rbh", "reset", "para", "icedet"},
		func(add func(...string)) {
			for _, p := range info.PlantNo {
				add(strconv.Itoa(int(p)), yesNo(info.Ctrl[p]), yesNo(info.Rbh[p]),
					yesNo(info.Reset[p]), yesNo(info.Para[p]), yesNo(info.IceDet[p]))
			}
		})
	for _, u := range info.Unsupported {
		a.r.warn("node not in the listing: " + u)
	}
	a.log.Info("park listed", "parkNo", info.ParkNo, "plants", info.PlantNo,
		"unsupported", info.Unsupported)

	if len(a.plants) == 0 {
		a.r.note("ENERGONTROL_TEST_PLANTS is not set, so reads cover the whole park and " +
			"no command can be sent: a command never defaults to every plant.")
	} else if missing := notListed(a.plants, info.PlantNo); len(missing) > 0 {
		a.r.warn(fmt.Sprintf("ENERGONTROL_TEST_PLANTS names %v, which the park does not list",
			missing))
	}
	a.states(ctx)
	return nil
}

// states reports the control, heating and ice detection state of the selected
// plants, or of the whole park when nothing is selected.
func (a *app) states(ctx context.Context) {
	plants := a.selection()
	if len(plants) == 0 {
		a.r.note("no plants to read")
		return
	}
	a.r.section("Plant states")

	ctrl, ctrlErr := a.client.PlantCtrlState(ctx, plants...)
	rbh, rbhErr := a.client.PlantRbhState(ctx, plants...)
	ice, iceErr := a.client.PlantIceDetState(ctx, plants...)
	for _, e := range []struct {
		what string
		err  error
	}{{"PlantCtrlState", ctrlErr}, {"PlantRbhState", rbhErr}, {"PlantIceDetState", iceErr}} {
		if e.err != nil {
			a.r.fail(e.what + ": " + e.err.Error())
		}
	}
	a.r.table([]string{"plant", "control", "heating", "ice detection"},
		func(add func(...string)) {
			for _, p := range plants {
				add(strconv.Itoa(int(p)), stateCell(ctrl, p), rbhCell(rbh, p), iceCell(ice, p))
			}
		})
	a.log.Info("plant states read", "plants", plants, "control", statesLine(ctrl))

	var unreadable int
	for _, s := range ctrl {
		if s.Err != nil {
			unreadable++
		}
	}
	if ctrlErr == nil && unreadable > 0 {
		a.r.warn(fmt.Sprintf("%d of %d plants had no usable control state; their Ctrl "+
			`value must not be read as "running"`, unreadable, len(ctrl)))
	}
}

// diagnose asks the questions the library's own contracts rest on. It writes
// nothing.
func (a *app) diagnose(ctx context.Context) {
	a.r.section("Server behaviour")

	stamped, total := a.obs.timestamps()
	switch {
	case total == 0:
		a.r.note("no item values seen yet, so nothing can be said about timestamps")
	case stamped == total:
		a.r.pass(fmt.Sprintf("every item carried a timestamp (%d/%d), so WithMaxStateAge "+
			"can detect a stale value as well as demand a fresh one", stamped, total))
	case stamped == 0:
		a.r.warn(fmt.Sprintf("no item carried a timestamp (0/%d): WithMaxStateAge would "+
			"reject every read with ErrNoItemTime, because an age that cannot be "+
			"established is not an age within the limit", total))
	default:
		a.r.warn(fmt.Sprintf("only %d of %d items carried a timestamp; WithMaxStateAge "+
			"would reject the rest", stamped, total))
	}
	if qs := a.obs.qualitySummary(); qs != "" {
		a.r.kv("Qualities seen", qs)
	}
	if missing := a.obs.repliesWithoutServerState(); missing == 0 {
		a.r.pass("every reply carried a ServerState, so a server that degrades " +
			"mid-command is noticed")
	} else {
		a.r.warn(fmt.Sprintf("%d replies carried no ServerState; the check is skipped "+
			"for those, since an absent attribute is not a statement of failure", missing))
	}

	// Whether the server pages a listing cannot be seen through the port:
	// following the pages is the transport's job, so a complete listing is all
	// the layers above ever get. That is the right contract — and the reason
	// this one question is asked underneath it.
	more, point, err := rawBrowsePaging(ctx, a.server, parkBranchPath)
	switch {
	case err != nil:
		a.r.warn("a raw browse of the park branch failed, so paging could not be " +
			"checked: " + err.Error())
	case !more:
		a.r.pass("the server returns its park listing in one answer; it does not page")
	case point == "":
		a.r.fail("the server announces more elements but hands out no continuation " +
			"point, so a listing cannot be completed — this is what ErrBrowseIncomplete " +
			"reports")
	default:
		a.r.note("the server pages its park listing (continuation point " + point +
			"); the listing followed the pages to the end, which is what the transport " +
			"is required to do")
	}

	// How does the server answer for an item that does not exist? The whole
	// per-plant error contract of this library depends on that being a
	// statement about the item and not about the request.
	if spare, ok := unlistedPlant(a.info.PlantNo); ok {
		states, err := a.client.PlantCtrlState(ctx, spare)
		switch {
		case err != nil:
			a.r.warn(fmt.Sprintf("a read for the unused plant %d failed as a whole "+
				"request (%v) instead of reporting the item; one bad item name would "+
				"then fail a read for the entire park", spare, err))
		case len(states) == 1 && states[0].Err != nil:
			a.r.pass(fmt.Sprintf("an item that does not exist is reported per item (%v), "+
				"so one unreadable plant does not blind the caller to the others",
				firstLine(states[0].Err)))
		default:
			a.r.note(fmt.Sprintf("the server answered for the unused plant %d, so this "+
				"check says nothing", spare))
		}
	}

	a.latency(ctx)

	reads, writes, browses := a.obs.counts()
	a.r.kv("Requests so far", fmt.Sprintf("%d read, %d write, %d browse", reads, writes, browses))
	a.log.Info("diagnosis complete", "itemsStamped", stamped, "itemsSeen", total,
		"reads", reads, "writes", writes, "browses", browses)
}

func (a *app) latency(ctx context.Context) {
	plants := a.selection()
	if len(plants) == 0 || a.cfg.samples <= 0 {
		return
	}
	a.r.section(fmt.Sprintf("Latency (%d round trips, one item each)", a.cfg.samples))
	shots := make([]time.Duration, 0, a.cfg.samples)
	for range a.cfg.samples {
		start := time.Now()
		if _, err := a.client.PlantCtrlState(ctx, plants[0]); err != nil {
			a.r.fail("read while timing: " + err.Error())
			return
		}
		shots = append(shots, time.Since(start))
	}
	s := summarise(shots)
	a.r.kv("min / p50 / p95 / max", fmt.Sprintf("%s / %s / %s / %s",
		round(s.min), round(s.p50), round(s.p95), round(s.max)))

	budget := recommendedPollTimeout(s.p95)
	a.r.kv("Recommended WithSessionPolling budget", budget.String())
	a.r.note(fmt.Sprintf("the budget is a wall-clock deadline and every attempt is one of "+
		"these round trips, so it has to hold several: %s is p95 × %d, floored at %s. "+
		"The library's default is %s.",
		budget, pollAttemptsWanted, minRecommendedPollTimeout, defaultPollTimeoutForReport))
	if budget > defaultPollTimeoutForReport {
		a.r.warn(fmt.Sprintf("this server is slow enough that the default %s leaves room "+
			"for few attempts; pass WithSessionPolling(100ms, %s)",
			defaultPollTimeoutForReport, budget))
	} else {
		a.r.pass("the library's default budget is comfortable for this server")
	}
	a.log.Info("latency measured", "samples", len(shots), "min", s.min.String(),
		"p50", s.p50.String(), "p95", s.p95.String(), "max", s.max.String(),
		"recommendedPollTimeout", budget.String())
}

// rawBrowsePaging asks the server directly whether it pages a browse answer.
func rawBrowsePaging(ctx context.Context, server *gopcxmlda.Server, path string) (
	more bool, point string, err error) {

	var handle string
	resp, err := server.Browse(ctx, path, &handle, "", gopcxmlda.TBrowseOptions{})
	if err != nil {
		return false, "", err
	}
	return strings.EqualFold(strings.TrimSpace(resp.Response.MoreElements), "true"),
		strings.TrimSpace(resp.Response.ContinuationPoint), nil
}

// ---------------------------------------------------------------------------
// latency arithmetic
// ---------------------------------------------------------------------------

// The figures behind the recommended polling budget. The budget is a
// wall-clock deadline, so it has to hold several round trips; the library
// guarantees a minimum number of attempts regardless, but a budget that cannot
// hold them is a budget that relies on the guarantee.
const (
	pollAttemptsWanted          = 8
	minRecommendedPollTimeout   = time.Second
	defaultPollTimeoutForReport = 5 * time.Second
)

func recommendedPollTimeout(p95 time.Duration) time.Duration {
	want := p95 * pollAttemptsWanted
	if want < minRecommendedPollTimeout {
		return minRecommendedPollTimeout
	}
	return want.Round(100 * time.Millisecond)
}

type latency struct{ min, p50, p95, max time.Duration }

func summarise(shots []time.Duration) latency {
	if len(shots) == 0 {
		return latency{}
	}
	sorted := slices.Clone(shots)
	slices.Sort(sorted)
	return latency{
		min: sorted[0],
		p50: sorted[percentileIndex(len(sorted), 50)],
		p95: sorted[percentileIndex(len(sorted), 95)],
		max: sorted[len(sorted)-1],
	}
}

// percentileIndex is the nearest-rank index, which needs no interpolation and
// never runs off the end of the slice.
func percentileIndex(n, pct int) int {
	if n == 0 {
		return 0
	}
	i := (pct*n + 99) / 100 // ceil(pct/100 * n)
	if i < 1 {
		i = 1
	}
	if i > n {
		i = n
	}
	return i - 1
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// notListed reports the requested plants the park does not list.
func notListed(want, listed []uint8) []uint8 {
	var out []uint8
	for _, p := range want {
		if !slices.Contains(listed, p) {
			out = append(out, p)
		}
	}
	return out
}

// unlistedPlant finds a plant number the park does not use, for the probe that
// asks how the server answers for an item that does not exist.
func unlistedPlant(listed []uint8) (uint8, bool) {
	for n := 255; n >= 0; n-- {
		if !slices.Contains(listed, uint8(n)) {
			return uint8(n), true
		}
	}
	return 0, false
}

func stateCell(states energontrol.PlantStates, plant uint8) string {
	s, ok := states.Get(plant)
	switch {
	case !ok:
		return "(not read)"
	case s.Err != nil:
		return "unknown: " + firstLine(s.Err)
	default:
		return s.Ctrl.String()
	}
}

func rbhCell(states energontrol.RbhStates, plant uint8) string {
	s, ok := states.Get(plant)
	switch {
	case !ok:
		return "(not read)"
	case s.Err != nil:
		return "unknown: " + firstLine(s.Err)
	default:
		return energontrol.RbhStatusString(s.Status)
	}
}

func iceCell(states energontrol.IceDetStates, plant uint8) string {
	s, ok := states.Get(plant)
	switch {
	case !ok:
		return "(not read)"
	case s.Err != nil:
		return "unknown: " + firstLine(s.Err)
	default:
		return energontrol.IceDetStatusString(s.Status)
	}
}

func statesLine(states energontrol.PlantStates) string {
	parts := make([]string, 0, len(states))
	for _, s := range states {
		parts = append(parts, s.String())
	}
	return strings.Join(parts, "; ")
}

// firstLine keeps a joined error on one line of a table.
func firstLine(err error) string {
	if err == nil {
		return ""
	}
	if i := strings.IndexByte(err.Error(), '\n'); i >= 0 {
		return err.Error()[:i] + " …"
	}
	return err.Error()
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func round(d time.Duration) string { return d.Round(time.Millisecond).String() }

// ---------------------------------------------------------------------------
// report rendering
// ---------------------------------------------------------------------------

type report struct{ w io.Writer }

func (r *report) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(r.w, format, args...)
}

func (r *report) head(s string) { r.printf("\n%s\n%s\n", s, strings.Repeat("=", len(s))) }

func (r *report) section(s string) {
	r.printf("\n── %s %s\n", s, strings.Repeat("─", maxInt(0, 66-len(s))))
}

func (r *report) kv(k, v string) { r.printf("  %-38s %s\n", k, v) }
func (r *report) pass(s string)  { r.printf("  [ ok ]   %s\n", wrapIndent(s, 11, 78)) }
func (r *report) warn(s string)  { r.printf("  [warn]   %s\n", wrapIndent(s, 11, 78)) }
func (r *report) fail(s string)  { r.printf("  [FAIL]   %s\n", wrapIndent(s, 11, 78)) }
func (r *report) note(s string)  { r.printf("           %s\n", wrapIndent(s, 11, 78)) }
func (r *report) line(s string)  { r.printf("%s\n", s) }

func (r *report) table(header []string, rows func(add func(...string))) {
	tw := tabwriter.NewWriter(r.w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "  %s\n", strings.Join(header, "\t"))
	rows(func(cells ...string) {
		_, _ = fmt.Fprintf(tw, "  %s\n", strings.Join(cells, "\t"))
	})
	_ = tw.Flush()
}

// wrapIndent wraps s to width, indenting continuation lines by indent spaces.
func wrapIndent(s string, indent, width int) string {
	limit := width - indent
	if limit < 20 {
		limit = 20
	}
	var out strings.Builder
	line := 0
	for i, word := range strings.Fields(s) {
		switch {
		case i == 0:
			out.WriteString(word)
			line = len(word)
		case line+1+len(word) > limit:
			out.WriteString("\n" + strings.Repeat(" ", indent) + word)
			line = len(word)
		default:
			out.WriteString(" " + word)
			line += 1 + len(word)
		}
	}
	return out.String()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
