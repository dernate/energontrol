package opcxmlda

// The adapter, driven against a real HTTP server with real SOAP responses.
//
// This is the layer the core's in-memory transport sits above and therefore
// cannot reach: correlating a response with its request, deciding what an error
// from the client library is a statement about, value decoding, browse paging,
// and the payloads that go out. Testing only above it is how the item-level
// error escalation survived a suite of 130 tests. The core's fake proves the
// protocol logic; this file proves the layer the protocol logic stands on.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dernate/energontrol/v2"
	"github.com/dernate/gopcxmlda"
)

// The item names these tests read and write.
//
// They are plain literals rather than something borrowed from the core's item
// namer, and deliberately so: the adapter is name-agnostic — it correlates
// whatever it was asked for — and a test that took its names from the core
// would be asserting the core's naming a second time instead of the adapter's
// correlation.
func ctrlItem(plant uint8) string { return fmt.Sprintf("Loc/Wec/Plant%d/Ctrl/Ctrl", plant) }
func setCtrlItem(plant uint8) string {
	return fmt.Sprintf("Loc/Wec/Plant%d/Ctrl/SetCtrl", plant)
}
func sessionRequestItem(plant uint8) string {
	return fmt.Sprintf("Loc/Wec/Plant%d/Ctrl/SessionRequest", plant)
}
func plantBranchPath(plant uint8) string { return fmt.Sprintf("Loc/Wec/Plant%d", plant) }

const parkBranchPath = "Loc/Wec"

func envelope(inner string) string {
	return `<?xml version="1.0" encoding="utf-8"?>` +
		`<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://schemas.xmlsoap.org/soap/envelope/" ` +
		`xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" ` +
		`xmlns:xsd="http://www.w3.org/2001/XMLSchema">` +
		`<SOAP-ENV:Body>` + inner + `</SOAP-ENV:Body></SOAP-ENV:Envelope>`
}

// requestedItemNames pulls the item names out of a Read payload.
func requestedItemNames(payload string) []string {
	var names []string
	for _, part := range strings.Split(payload, `:Items ItemName="`)[1:] {
		if end := strings.Index(part, `"`); end >= 0 {
			names = append(names, part[:end])
		}
	}
	return names
}

// opcResponder answers one OPC XML-DA request. It gets the SOAPAction and the
// request body and returns the XML to put inside the SOAP body.
type opcResponder func(action, body string) string

// newAdapterTest starts an HTTP server answering with respond and returns the
// energontrol.Transport in front of it.
func newAdapterTest(t *testing.T, respond opcResponder) energontrol.Transport {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		_, _ = io.WriteString(w, envelope(respond(r.Header.Get("SOAPAction"), string(body))))
	}))
	t.Cleanup(srv.Close)

	parsed, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return New(&gopcxmlda.Server{
		Url: parsed, LocaleID: "en-us", Timeout: 5 * time.Second,
	})
}

// statusOK is the GetStatus reply of a healthy server, for the tests that need
// a command to get past ServerAvailable.
func statusOK() string {
	now := time.Now().Format(time.RFC3339)
	return fmt.Sprintf(
		`<GetStatusResponse xmlns="http://opcfoundation.org/webservices/XMLDA/1.0/">`+
			`<GetStatusResult RcvTime="%s" ReplyTime="%s" ServerState="running"/><Status/>`+
			`</GetStatusResponse>`, now, now)
}

// readReply wraps item elements in a ReadResponse, with anything extra — an
// <Errors> element, say — appended after the item list.
func readReply(items, extra string) string {
	now := time.Now().Format(time.RFC3339)
	return fmt.Sprintf(
		`<ReadResponse xmlns="http://opcfoundation.org/webservices/XMLDA/1.0/">`+
			`<ReadResult RcvTime="%s" ReplyTime="%s" ServerState="running"/>`+
			`<RItemList>%s</RItemList>%s</ReadResponse>`, now, now, items, extra)
}

func writeReply(items, extra string) string {
	now := time.Now().Format(time.RFC3339)
	return fmt.Sprintf(
		`<WriteResponse xmlns="http://opcfoundation.org/webservices/XMLDA/1.0/">`+
			`<WriteResult RcvTime="%s" ReplyTime="%s" ServerState="running"/>`+
			`<RItemList>%s</RItemList>%s</WriteResponse>`, now, now, items, extra)
}

// reqItem is one item of a request, with the handle the client generated for
// it, so a reply can echo it the way a real server does.
type reqItem struct {
	name   string
	handle string
}

var (
	itemsElementRE = regexp.MustCompile(`:Items\s+([^>]*?)/?>`)
	attrRE         = regexp.MustCompile(`([A-Za-z]+)="([^"]*)"`)
)

// requestItems pulls the item names and client item handles out of a Read or
// Write payload.
func requestItems(payload string) []reqItem {
	var out []reqItem
	for _, m := range itemsElementRE.FindAllStringSubmatch(payload, -1) {
		var it reqItem
		for _, a := range attrRE.FindAllStringSubmatch(m[1], -1) {
			switch a[1] {
			case "ItemName":
				it.name = a[2]
			case "ClientItemHandle":
				it.handle = a[2]
			}
		}
		if it.name != "" || it.handle != "" {
			out = append(out, it)
		}
	}
	return out
}

// goodItem is a well-formed response item carrying one unsigned long.
func goodItem(it reqItem, value uint64) string {
	return fmt.Sprintf(
		`<Items ItemName="%s" ClientItemHandle="%s">`+
			`<Value xsi:type="xsd:unsignedLong">%d</Value>`+
			`<Quality QualityField="good"/></Items>`, it.name, it.handle, value)
}

// resultOf returns the ItemResult for one name, or fails the test.
func resultOf(t *testing.T, resp energontrol.Response, name string) energontrol.ItemResult {
	t.Helper()
	for _, r := range resp.Items {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no result for %q in %+v", name, resp.Items)
	return energontrol.ItemResult{}
}

// browsePages answers successive browses with the given pages. Each page is a
// slice of element names; every page but the last announces more elements.
func browsePages(t *testing.T, pages [][]string, withPoint bool) energontrol.Transport {
	t.Helper()
	var mu sync.Mutex
	seen := 0
	return newAdapterTest(t, func(action, body string) string {
		if strings.Contains(action, "GetStatus") {
			return statusOK()
		}
		mu.Lock()
		page := seen
		if page < len(pages) {
			seen++
		}
		mu.Unlock()
		if page >= len(pages) {
			page = len(pages) - 1
		}

		var elems strings.Builder
		for _, name := range pages[page] {
			fmt.Fprintf(&elems,
				`<Elements Name="%s" ItemName="Loc/Wec/%s" HasChildren="true" IsItem="false"/>`,
				name, name)
		}
		more, point := "false", ""
		if page < len(pages)-1 {
			more = "true"
			if withPoint {
				point = fmt.Sprintf(` ContinuationPoint="page-%d"`, page+1)
			}
		}
		now := time.Now().Format(time.RFC3339)
		return fmt.Sprintf(
			`<BrowseResponse xmlns="http://opcfoundation.org/webservices/XMLDA/1.0/" `+
				`MoreElements="%s"%s>`+
				`<BrowseResult RcvTime="%s" ReplyTime="%s" ServerState="running"/>`+
				`%s</BrowseResponse>`, more, point, now, now, elems.String())
	})
}

// readPayloadOf runs one read against an adapter and returns the SOAP body that
// went out.
func readPayloadOf(t *testing.T, opts ...energontrol.Option) string {
	t.Helper()
	var mu sync.Mutex
	var payload string

	tr := newAdapterTest(t, func(action, body string) string {
		if strings.Contains(action, "Read") {
			mu.Lock()
			payload = body
			mu.Unlock()
		}
		var items strings.Builder
		for _, it := range requestItems(body) {
			fmt.Fprintf(&items,
				`<Items ItemName="%s" ClientItemHandle="%s" Timestamp="%s">`+
					`<Value xsi:type="xsd:unsignedLong">2</Value>`+
					`<Quality QualityField="good"/></Items>`,
				it.name, it.handle, time.Now().UTC().Format(time.RFC3339))
		}
		return readReply(items.String(), "")
	})

	if _, err := energontrol.New(tr, opts...).PlantCtrlState(context.Background(), 2); err != nil {
		t.Fatalf("PlantCtrlState: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return payload
}

// TestWireArrayTypeIsLongWord drives a real *gopcxmlda.Server against an HTTP
// test server and inspects the SOAP body that goes out.
//
// Enercon types SessionRequest, SetCtrl, SetRbh, SetIceDet and SessionSubmit as
// arrays of long word, that is 32-bit unsigned. gopcxmlda derives the XML type
// from the Go type, so a []uint64 would emit ArrayOfUnsignedLong (64-bit) — what
// v1 sent — while a []uint32 emits ArrayOfUnsignedInt, which is the long word.
// Nothing else in the test suite exercises the real encoder, so this is the only
// place the mistake would be caught.
func TestWireArrayTypeIsLongWord(t *testing.T) {
	var mu sync.Mutex
	var writeBodies []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		action := r.Header.Get("SOAPAction")
		w.Header().Set("Content-Type", "text/xml")
		now := time.Now().Format(time.RFC3339)

		switch {
		case strings.Contains(action, "GetStatus"):
			_, _ = fmt.Fprint(w, envelope(fmt.Sprintf(
				`<GetStatusResponse xmlns="http://opcfoundation.org/webservices/XMLDA/1.0/">`+
					`<GetStatusResult RcvTime="%s" ReplyTime="%s" ServerState="running"/><Status/>`+
					`</GetStatusResponse>`, now, now)))
		case strings.Contains(action, "Read"):
			var items strings.Builder
			for _, name := range requestedItemNames(string(body)) {
				typ, value := "unsignedLong", "0"
				switch {
				case strings.HasSuffix(name, "/SessionState"):
					typ, value = "unsignedShort", "0"
				case strings.HasSuffix(name, "/Ctrl/Ctrl"):
					value = "0" // running, so a stop is needed
				}
				fmt.Fprintf(&items,
					`<Items ItemName="%s"><Value xsi:type="xsd:%s">%s</Value>`+
						`<Quality QualityField="good"/></Items>`, name, typ, value)
			}
			_, _ = fmt.Fprint(w, envelope(fmt.Sprintf(
				`<ReadResponse xmlns="http://opcfoundation.org/webservices/XMLDA/1.0/">`+
					`<ReadResult RcvTime="%s" ReplyTime="%s" ServerState="running"/>`+
					`<RItemList>%s</RItemList></ReadResponse>`, now, now, items.String())))
		case strings.Contains(action, "Write"):
			mu.Lock()
			writeBodies = append(writeBodies, string(body))
			mu.Unlock()
			_, _ = fmt.Fprint(w, envelope(fmt.Sprintf(
				`<WriteResponse xmlns="http://opcfoundation.org/webservices/XMLDA/1.0/">`+
					`<WriteResult RcvTime="%s" ReplyTime="%s" ServerState="running"/>`+
					`<RItemList/></WriteResponse>`, now, now)))
		default:
			http.Error(w, "unexpected SOAPAction "+action, http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	parsed, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	server := &gopcxmlda.Server{Url: parsed, LocaleID: "en-us", Timeout: 5 * time.Second}

	// The session never advances past "free" here, so the command fails — the
	// point is the payload of the SessionRequest write that goes out first.
	client := energontrol.New(New(server), energontrol.WithSessionPolling(time.Millisecond, 5*time.Millisecond))
	if _, err := client.Stop(context.Background(), 169592065, false, true, 2); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(writeBodies) == 0 {
		t.Fatal("no write reached the server")
	}
	payload := writeBodies[0]
	if !strings.Contains(payload, "ArrayOfUnsignedInt") {
		t.Errorf("write payload does not declare ArrayOfUnsignedInt (long word):\n%s", payload)
	}
	if strings.Contains(payload, "ArrayOfUnsignedLong") {
		t.Errorf("write payload declares ArrayOfUnsignedLong (64 bit), not a long word:\n%s", payload)
	}
	if !strings.Contains(payload, "SessionRequest") {
		t.Errorf("first write is not the SessionRequest:\n%s", payload)
	}
	// The options are XML attribute names and case sensitive.
	if !strings.Contains(payload, "ReturnItemName") {
		t.Errorf("write payload does not request ItemName:\n%s", payload)
	}
}

// TestWireReadOptionsUseSpecCasing checks the read payload on the wire too.
func TestWireReadOptionsUseSpecCasing(t *testing.T) {
	var mu sync.Mutex
	var readBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		now := time.Now().Format(time.RFC3339)
		w.Header().Set("Content-Type", "text/xml")
		if strings.Contains(r.Header.Get("SOAPAction"), "Read") {
			mu.Lock()
			readBody = string(body)
			mu.Unlock()
		}
		_, _ = fmt.Fprint(w, envelope(fmt.Sprintf(
			`<ReadResponse xmlns="http://opcfoundation.org/webservices/XMLDA/1.0/">`+
				`<ReadResult RcvTime="%s" ReplyTime="%s" ServerState="running"/>`+
				`<RItemList><Items ItemName="Loc/Wec/Plant2/Ctrl/Ctrl">`+
				`<Value xsi:type="xsd:unsignedLong">2</Value>`+
				`<Quality QualityField="good"/></Items></RItemList></ReadResponse>`, now, now)))
	}))
	defer srv.Close()

	parsed, _ := url.Parse(srv.URL)
	server := &gopcxmlda.Server{Url: parsed, LocaleID: "en-us", Timeout: 5 * time.Second}
	states, err := energontrol.New(New(server)).PlantCtrlState(context.Background(), 2)
	if err != nil {
		t.Fatalf("PlantCtrlState: %v", err)
	}
	if states[0].Ctrl != energontrol.CtrlStop90 {
		t.Errorf("state = %s, want %s", states[0].Ctrl, energontrol.CtrlStop90)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, option := range []string{"ReturnItemName", "ReturnItemTime", "ReturnErrorText"} {
		if !strings.Contains(readBody, option+"=") {
			t.Errorf("read payload is missing %s:\n%s", option, readBody)
		}
	}
	if strings.Contains(readBody, "returnItemName") {
		t.Errorf("read payload uses the v1 lower-case spelling:\n%s", readBody)
	}
	// Per the specification's WSDL the Read element itself carries no
	// attributes — only an Options and an ItemList child. LocaleID and
	// ClientRequestHandle belong on RequestOptions, and gopcxmlda used to put
	// them on Read, where a strictly validating server would have rejected
	// them and where the request handle was never echoed back.
	readElement := readBody[strings.Index(readBody, ":Read"):]
	readElement = readElement[:strings.Index(readElement, ">")]
	if strings.Contains(readElement, "=") {
		t.Errorf("the Read element carries attributes of its own: <%s>", readElement)
	}
	for _, want := range []string{"ClientRequestHandle=", "LocaleID="} {
		if !strings.Contains(readBody, want) {
			t.Errorf("read payload is missing %s:\n%s", want, readBody)
		}
	}
}

// OPC XML-DA reports a problem with one item in that item's ResultID attribute
// and, when ReturnErrorText is requested — which this package does, and which
// is the specification's own default — adds an <Errors> element carrying the
// localised text for it. gopcxmlda surfaces that element as an error from
// Read, so a complete and perfectly usable response arrives with a non-nil
// error as soon as a single item is faulted.
//
// Treating that as a request failure made this package's whole per-item error
// path unreachable against a conformant server.
func TestAdapterItemFaultIsNotARequestFailure(t *testing.T) {
	tr := newAdapterTest(t, func(action, body string) string {
		var items strings.Builder
		for _, it := range requestItems(body) {
			if strings.Contains(it.name, "Plant3") {
				fmt.Fprintf(&items,
					`<Items ItemName="%s" ClientItemHandle="%s" ResultID="E_UNKNOWNITEMNAME"/>`,
					it.name, it.handle)
				continue
			}
			items.WriteString(goodItem(it, 2))
		}
		return readReply(items.String(),
			`<Errors ID="E_UNKNOWNITEMNAME"><Text>the item does not exist</Text></Errors>`)
	})

	names := []string{ctrlItem(2), ctrlItem(3), ctrlItem(4)}
	resp, err := tr.Read(context.Background(), names, energontrol.ReadOptions{})
	if err != nil {
		t.Fatalf("one faulted item failed the whole read: %v", err)
	}
	if len(resp.Items) != 3 {
		t.Fatalf("got %d results for 3 requested items: %+v", len(resp.Items), resp.Items)
	}
	faulted := resultOf(t, resp, ctrlItem(3))
	if faulted.ResultID == "" {
		t.Error("the faulted item carries no ResultID")
	}
	// The localised text belonging to the ResultID is kept as diagnosis.
	if !strings.Contains(faulted.ResultID, "does not exist") {
		t.Errorf("ResultID = %q, want the server's error text alongside it", faulted.ResultID)
	}
	for _, name := range []string{ctrlItem(2), ctrlItem(4)} {
		if r := resultOf(t, resp, name); len(r.Values) != 1 || r.Values[0] != 2 {
			t.Errorf("%s: values = %v, want [2]", name, r.Values)
		}
	}
}

// The same defect where it did the most damage: one unknown item name used to
// fail a read for an entire park.
func TestAdapterItemFaultDoesNotBlindAPark(t *testing.T) {
	tr := newAdapterTest(t, func(action, body string) string {
		if strings.Contains(action, "GetStatus") {
			return statusOK()
		}
		var items strings.Builder
		for _, it := range requestItems(body) {
			if strings.Contains(it.name, "Plant3") {
				fmt.Fprintf(&items,
					`<Items ItemName="%s" ClientItemHandle="%s" ResultID="E_UNKNOWNITEMNAME"/>`,
					it.name, it.handle)
				continue
			}
			items.WriteString(goodItem(it, uint64(energontrol.CtrlStop90)))
		}
		return readReply(items.String(),
			`<Errors ID="E_UNKNOWNITEMNAME"><Text>the item does not exist</Text></Errors>`)
	})

	states, err := energontrol.New(tr).PlantCtrlState(context.Background(), 2, 3, 4)
	if err != nil {
		t.Fatalf("PlantCtrlState: %v", err)
	}
	if len(states) != 3 {
		t.Fatalf("got %d states for 3 plants", len(states))
	}
	if got, _ := states.Get(3); !errors.Is(got.Err, energontrol.ErrItemFault) {
		t.Errorf("plant 3: err = %v, want it to wrap energontrol.ErrItemFault", got.Err)
	}
	for _, plant := range []uint8{2, 4} {
		got, _ := states.Get(plant)
		if got.Err != nil {
			t.Errorf("plant %d: err = %v, want the readable plants unaffected", plant, got.Err)
		}
		if got.Ctrl != energontrol.CtrlStop90 {
			t.Errorf("plant %d: state = %s, want %s", plant, got.Ctrl, energontrol.CtrlStop90)
		}
	}
}

// The tolerance is narrow on purpose: an <Errors> element that no returned item
// accounts for is not explained by the items, so it stays a request failure.
func TestAdapterUnexplainedErrorsElementIsARequestFailure(t *testing.T) {
	tr := newAdapterTest(t, func(action, body string) string {
		var items strings.Builder
		for _, it := range requestItems(body) {
			items.WriteString(goodItem(it, 0))
		}
		return readReply(items.String(),
			`<Errors ID="E_SERVERSTATE"><Text>something else is wrong</Text></Errors>`)
	})

	if _, err := tr.Read(context.Background(), []string{ctrlItem(2)}, energontrol.ReadOptions{}); err == nil {
		t.Fatal("an error no item accounts for was swallowed")
	}
}

// A SOAP fault is a statement about the request and always surfaces.
func TestAdapterSoapFaultIsARequestFailure(t *testing.T) {
	tr := newAdapterTest(t, func(action, body string) string {
		return `<SOAP-ENV:Fault><faultcode>SOAP-ENV:Server</faultcode>` +
			`<faultstring>internal error</faultstring></SOAP-ENV:Fault>`
	})

	_, err := tr.Read(context.Background(), []string{ctrlItem(2)}, energontrol.ReadOptions{})
	if err == nil {
		t.Fatal("a SOAP fault produced no error")
	}
	var fault *gopcxmlda.SoapFaultError
	if !errors.As(err, &fault) {
		t.Errorf("err = %v, want it to carry the SOAP fault", err)
	}
}

// And it still surfaces when the response also carries item-level errors: the
// classification looks at every leaf of the joined error, not just at whether
// an item-level one is somewhere in there.
func TestAdapterSoapFaultAlongsideItemErrorsIsARequestFailure(t *testing.T) {
	tr := newAdapterTest(t, func(action, body string) string {
		var items strings.Builder
		for _, it := range requestItems(body) {
			fmt.Fprintf(&items,
				`<Items ItemName="%s" ClientItemHandle="%s" ResultID="E_UNKNOWNITEMNAME"/>`,
				it.name, it.handle)
		}
		return `<SOAP-ENV:Fault><faultcode>SOAP-ENV:Server</faultcode>` +
			`<faultstring>internal error</faultstring></SOAP-ENV:Fault>` +
			readReply(items.String(),
				`<Errors ID="E_UNKNOWNITEMNAME"><Text>the item does not exist</Text></Errors>`)
	})

	if _, err := tr.Read(context.Background(), []string{ctrlItem(2)}, energontrol.ReadOptions{}); err == nil {
		t.Fatal("a SOAP fault was swallowed because item errors were also present")
	}
}

// A write is classified the same way, and a per-item ResultID on a write comes
// through as an unconfirmed write rather than as a failed request.
func TestAdapterWriteReportsPerItemFaults(t *testing.T) {
	tr := newAdapterTest(t, func(action, body string) string {
		var items strings.Builder
		for _, it := range requestItems(body) {
			fmt.Fprintf(&items,
				`<Items ItemName="%s" ClientItemHandle="%s" ResultID="E_BADRIGHTS"/>`,
				it.name, it.handle)
		}
		return writeReply(items.String(),
			`<Errors ID="E_BADRIGHTS"><Text>insufficient rights</Text></Errors>`)
	})

	resp, err := tr.Write(context.Background(), []energontrol.ItemWrite{
		{Name: setCtrlItem(2), Value: []uint32{2, 1, 1}},
	})
	if err != nil {
		t.Fatalf("a rejected item failed the whole write: %v", err)
	}
	if r := resultOf(t, resp, setCtrlItem(2)); r.ResultID == "" {
		t.Error("the rejected write carries no ResultID")
	}
}

// Where the boundary of the per-item handling runs.
//
// Everything above deals with a response the client library could parse, in
// which the server reported a problem with one item — that is what stays
// per-item. A response the library cannot parse at all is a different thing:
// gopcxmlda is deliberately fail-fast and all-or-nothing about decoding, so a
// value element without the xsi:type the schema requires ends the whole
// response, not one item of it.
//
// That is the transport's contract and not something for this package to work
// around: a reply this malformed says nothing trustworthy about any of its
// items, and salvaging the ones that happened to parse would mean deciding —
// on a guess — that the rest of the document is still to be believed. It is
// pinned here so the boundary is visible rather than incidental.
func TestAdapterUnparseableResponseIsARequestFailure(t *testing.T) {
	tr := newAdapterTest(t, func(action, body string) string {
		var items strings.Builder
		for _, it := range requestItems(body) {
			value := `<Value xsi:type="xsd:unsignedLong">2</Value>`
			if strings.Contains(it.name, "Plant3") {
				value = `<Value>2</Value>` // no xsi:type: unparseable
			}
			fmt.Fprintf(&items,
				`<Items ItemName="%s" ClientItemHandle="%s">%s`+
					`<Quality QualityField="good"/></Items>`, it.name, it.handle, value)
		}
		return readReply(items.String(), "")
	})

	_, err := tr.Read(context.Background(),
		[]string{ctrlItem(2), ctrlItem(3), ctrlItem(4)}, energontrol.ReadOptions{})
	if err == nil {
		t.Fatal("a response that could not be decoded was accepted")
	}
	if !strings.Contains(err.Error(), "xsi:type") {
		t.Errorf("err = %v, want it to name what could not be decoded", err)
	}
}

// A write reply confirms items; it is not obliged to echo their values.
//
// ReturnValuesOnReply invites a server to echo them, and nothing above the port
// looks at a write reply's values anyway — so an item echoed without one is a
// confirmation, not a value that could not be decoded. Decoding it as one
// failed every command against a server that confirms without echoing, which
// is what an Enercon SCADA does. Nothing in the suite could see it: the core's
// in-memory transport never reaches this decoding at all, and it took driving
// the whole library through real SOAP to surface.
func TestAdapterWriteConfirmationNeedsNoValue(t *testing.T) {
	tr := newAdapterTest(t, func(action, body string) string {
		var items strings.Builder
		for _, it := range requestItems(body) {
			// A bare confirmation: name, handle, no Value element.
			fmt.Fprintf(&items, `<Items ItemName="%s" ClientItemHandle="%s"/>`,
				it.name, it.handle)
		}
		return writeReply(items.String(), "")
	})

	resp, err := tr.Write(context.Background(), []energontrol.ItemWrite{
		{Name: setCtrlItem(2), Value: []uint32{2, 1, 1}},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := resultOf(t, resp, setCtrlItem(2))
	if got.Err != nil {
		t.Errorf("err = %v, want a confirmation: a write reply carries no value to decode",
			got.Err)
	}
	if got.ResultID != "" {
		t.Errorf("ResultID = %q, want none", got.ResultID)
	}
}

// A read is the other way round: there the value is the answer, and an item
// that carries none is an item that could not be read.
func TestAdapterReadStillNeedsAValue(t *testing.T) {
	tr := newAdapterTest(t, func(action, body string) string {
		var items strings.Builder
		for _, it := range requestItems(body) {
			fmt.Fprintf(&items, `<Items ItemName="%s" ClientItemHandle="%s">`+
				`<Quality QualityField="good"/></Items>`, it.name, it.handle)
		}
		return readReply(items.String(), "")
	})

	resp, err := tr.Read(context.Background(), []string{ctrlItem(2)}, energontrol.ReadOptions{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := resultOf(t, resp, ctrlItem(2)); !errors.Is(got.Err, energontrol.ErrUnexpectedType) {
		t.Errorf("err = %v, want it to wrap ErrUnexpectedType", got.Err)
	}
}

// OPC XML-DA does not promise that a response lists items in request order.
// Correlation is by ClientItemHandle, so the order must not matter.
func TestAdapterCorrelatesReorderedResponses(t *testing.T) {
	tr := newAdapterTest(t, func(action, body string) string {
		reqs := requestItems(body)
		var items strings.Builder
		for i := len(reqs) - 1; i >= 0; i-- {
			// The value encodes the plant number, so a mismatch is visible.
			var plant uint64
			_, _ = fmt.Sscanf(reqs[i].name, "Loc/Wec/Plant%d/", &plant)
			items.WriteString(goodItem(reqs[i], plant))
		}
		return readReply(items.String(), "")
	})

	names := []string{ctrlItem(2), ctrlItem(5), ctrlItem(7)}
	resp, err := tr.Read(context.Background(), names, energontrol.ReadOptions{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	for _, want := range []struct {
		name  string
		value uint64
	}{{ctrlItem(2), 2}, {ctrlItem(5), 5}, {ctrlItem(7), 7}} {
		if got := resultOf(t, resp, want.name); got.Values[0] != want.value {
			t.Errorf("%s: value = %d, want %d", want.name, got.Values[0], want.value)
		}
	}
}

// A server that echoes no handle can still be correlated by item name.
func TestAdapterCorrelatesByItemNameWithoutHandles(t *testing.T) {
	tr := newAdapterTest(t, func(action, body string) string {
		var items strings.Builder
		for _, it := range requestItems(body) {
			fmt.Fprintf(&items,
				`<Items ItemName="%s"><Value xsi:type="xsd:unsignedLong">1</Value>`+
					`<Quality QualityField="good"/></Items>`, it.name)
		}
		return readReply(items.String(), "")
	})

	resp, err := tr.Read(context.Background(), []string{ctrlItem(2), ctrlItem(5)}, energontrol.ReadOptions{})
	if err != nil {
		t.Fatalf("correlation by ItemName failed: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Errorf("got %d results, want 2", len(resp.Items))
	}
}

// With neither a handle nor an item name a response cannot be matched to the
// request. Guessing by position could command the wrong turbine, so it is
// refused.
func TestAdapterRefusesAnUncorrelatableResponse(t *testing.T) {
	tr := newAdapterTest(t, func(action, body string) string {
		return readReply(
			`<Items><Value xsi:type="xsd:unsignedLong">1</Value>`+
				`<Quality QualityField="good"/></Items>`, "")
	})

	_, err := tr.Read(context.Background(), []string{ctrlItem(2)}, energontrol.ReadOptions{})
	if !errors.Is(err, energontrol.ErrUncorrelatable) {
		t.Fatalf("err = %v, want it to wrap energontrol.ErrUncorrelatable", err)
	}
}

// Where a server supplies both a handle and an item name, the two have to
// name the same item. The handle is authoritative per specification, so a
// contradiction is a server fault — and filing the item under the handle anyway
// is the very mistake positional matching was rejected for, only harder to see.
// Two contradicting items are two swapped turbines.
func TestAdapterRefusesAContradictionBetweenHandleAndItemName(t *testing.T) {
	tr := newAdapterTest(t, func(action, body string) string {
		reqs := requestItems(body)
		if len(reqs) < 2 {
			t.Fatalf("expected two requested items, got %d", len(reqs))
		}
		// Handle of the first item, name of the second, and vice versa.
		return readReply(
			fmt.Sprintf(
				`<Items ItemName="%s" ClientItemHandle="%s">`+
					`<Value xsi:type="xsd:unsignedLong">0</Value>`+
					`<Quality QualityField="good"/></Items>`+
					`<Items ItemName="%s" ClientItemHandle="%s">`+
					`<Value xsi:type="xsd:unsignedLong">2</Value>`+
					`<Quality QualityField="good"/></Items>`,
				reqs[1].name, reqs[0].handle, reqs[0].name, reqs[1].handle), "")
	})

	_, err := tr.Read(context.Background(), []string{ctrlItem(2), ctrlItem(3)}, energontrol.ReadOptions{})
	if !errors.Is(err, energontrol.ErrUncorrelatable) {
		t.Fatalf("err = %v, want it to wrap energontrol.ErrUncorrelatable: "+
			"the handle and the item name named different turbines", err)
	}
}

func TestAdapterRefusesADuplicatedItem(t *testing.T) {
	tr := newAdapterTest(t, func(action, body string) string {
		var items strings.Builder
		for _, it := range requestItems(body) {
			items.WriteString(goodItem(it, 1))
			items.WriteString(goodItem(it, 2))
		}
		return readReply(items.String(), "")
	})

	_, err := tr.Read(context.Background(), []string{ctrlItem(2)}, energontrol.ReadOptions{})
	if !errors.Is(err, energontrol.ErrUncorrelatable) {
		t.Fatalf("err = %v, want it to wrap energontrol.ErrUncorrelatable", err)
	}
}

// Which Go type an item's value carries depends on the xsi:type the server
// chose, not on what the client expects. v1 asserted .(uint64) and .(uint16)
// and panicked the calling process when a server disagreed.
func TestAdapterDecodesEveryNumericType(t *testing.T) {
	for _, tc := range []struct {
		xsiType string
		literal string
		want    uint64
	}{
		{"unsignedLong", "130", 130},
		{"unsignedInt", "130", 130},
		{"unsignedShort", "130", 130},
		{"unsignedByte", "130", 130},
		{"int", "130", 130},
		{"long", "130", 130},
		{"short", "130", 130},
		{"double", "130", 130},
		{"float", "130", 130},
	} {
		t.Run(tc.xsiType, func(t *testing.T) {
			tr := newAdapterTest(t, func(action, body string) string {
				var items strings.Builder
				for _, it := range requestItems(body) {
					fmt.Fprintf(&items,
						`<Items ItemName="%s" ClientItemHandle="%s">`+
							`<Value xsi:type="xsd:%s">%s</Value>`+
							`<Quality QualityField="good"/></Items>`,
						it.name, it.handle, tc.xsiType, tc.literal)
				}
				return readReply(items.String(), "")
			})

			resp, err := tr.Read(context.Background(), []string{ctrlItem(2)}, energontrol.ReadOptions{})
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			got := resultOf(t, resp, ctrlItem(2))
			if got.Err != nil {
				t.Fatalf("xsi:type %s rejected: %v", tc.xsiType, got.Err)
			}
			if len(got.Values) != 1 || got.Values[0] != tc.want {
				t.Errorf("values = %v, want [%d]", got.Values, tc.want)
			}
		})
	}
}

// A value that is not a number is a per-item problem, not a panic and not a
// failed request.
func TestAdapterRejectsAnUndecodableValue(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"a string", `<Value xsi:type="xsd:string">not a number</Value>`},
		{"a negative number", `<Value xsi:type="xsd:int">-1</Value>`},
		{"no value at all", ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newAdapterTest(t, func(action, body string) string {
				var items strings.Builder
				for _, it := range requestItems(body) {
					fmt.Fprintf(&items,
						`<Items ItemName="%s" ClientItemHandle="%s">%s`+
							`<Quality QualityField="good"/></Items>`, it.name, it.handle, tc.value)
				}
				return readReply(items.String(), "")
			})

			resp, err := tr.Read(context.Background(), []string{ctrlItem(2)}, energontrol.ReadOptions{})
			if err != nil {
				t.Fatalf("an undecodable value failed the whole read: %v", err)
			}
			got := resultOf(t, resp, ctrlItem(2))
			if !errors.Is(got.Err, energontrol.ErrUnexpectedType) {
				t.Errorf("err = %v, want it to wrap energontrol.ErrUnexpectedType", got.Err)
			}
		})
	}
}

// A server is free to type a single value as a scalar rather than as a
// one-element array, and the read-back checks only look at the leading
// elements. Rejecting that shape would turn an encoding choice into an
// unverifiable session.
func TestAdapterScalarBecomesAOneElementSlice(t *testing.T) {
	tr := newAdapterTest(t, func(action, body string) string {
		var items strings.Builder
		for _, it := range requestItems(body) {
			items.WriteString(goodItem(it, 7))
		}
		return readReply(items.String(), "")
	})

	resp, err := tr.Read(context.Background(), []string{sessionRequestItem(2)}, energontrol.ReadOptions{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	got := resultOf(t, resp, sessionRequestItem(2))
	if len(got.Values) != 1 || got.Values[0] != 7 {
		t.Errorf("values = %v, want [7]", got.Values)
	}
}

// The array items Enercon defines — SessionRequest and the Set… items — arrive
// as an ArrayOfUnsignedInt.
func TestAdapterDecodesAnArray(t *testing.T) {
	tr := newAdapterTest(t, func(action, body string) string {
		var items strings.Builder
		for _, it := range requestItems(body) {
			fmt.Fprintf(&items,
				`<Items ItemName="%s" ClientItemHandle="%s">`+
					`<Value xsi:type="xsd:ArrayOfUnsignedInt">`+
					`<xsd:unsignedInt>2</xsd:unsignedInt>`+
					`<xsd:unsignedInt>4711</xsd:unsignedInt>`+
					`<xsd:unsignedInt>815</xsd:unsignedInt>`+
					`</Value><Quality QualityField="good"/></Items>`, it.name, it.handle)
		}
		return readReply(items.String(), "")
	})

	resp, err := tr.Read(context.Background(), []string{setCtrlItem(2)}, energontrol.ReadOptions{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	got := resultOf(t, resp, setCtrlItem(2))
	if got.Err != nil {
		t.Fatalf("array rejected: %v", got.Err)
	}
	if len(got.Values) != 3 || got.Values[0] != 2 || got.Values[1] != 4711 || got.Values[2] != 815 {
		t.Errorf("values = %v, want [2 4711 815]", got.Values)
	}
}

// Quality and timestamp are facts about the item that the layers above decide
// on, so they have to arrive intact.
func TestAdapterCarriesQualityAndTimestamp(t *testing.T) {
	stamp := time.Now().UTC().Add(-90 * time.Second).Truncate(time.Second)
	tr := newAdapterTest(t, func(action, body string) string {
		var items strings.Builder
		for _, it := range requestItems(body) {
			fmt.Fprintf(&items,
				`<Items ItemName="%s" ClientItemHandle="%s" Timestamp="%s">`+
					`<Value xsi:type="xsd:unsignedLong">1</Value>`+
					`<Quality QualityField="badDeviceFailure"/></Items>`,
				it.name, it.handle, stamp.Format(time.RFC3339))
		}
		return readReply(items.String(), "")
	})

	resp, err := tr.Read(context.Background(), []string{ctrlItem(2)}, energontrol.ReadOptions{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	got := resultOf(t, resp, ctrlItem(2))
	if got.Quality != "badDeviceFailure" {
		t.Errorf("quality = %q, want it passed through", got.Quality)
	}
	if !got.Timestamp.Equal(stamp) {
		t.Errorf("timestamp = %s, want %s", got.Timestamp, stamp)
	}
}

// An item the server does not answer for is left out, and the layers above turn
// that into ErrItemMissing rather than into a value of zero.
func TestAdapterOmitsAnUnansweredItem(t *testing.T) {
	tr := newAdapterTest(t, func(action, body string) string {
		var items strings.Builder
		for _, it := range requestItems(body) {
			if strings.Contains(it.name, "Plant5") {
				continue
			}
			items.WriteString(goodItem(it, 1))
		}
		return readReply(items.String(), "")
	})

	resp, err := tr.Read(context.Background(), []string{ctrlItem(2), ctrlItem(5)}, energontrol.ReadOptions{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(resp.Items) != 1 || resp.Items[0].Name != ctrlItem(2) {
		t.Fatalf("results = %+v, want only plant 2", resp.Items)
	}
}

func TestAdapterStatusReportsServerState(t *testing.T) {
	tr := newAdapterTest(t, func(action, body string) string { return statusOK() })
	state, err := tr.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if state != "running" {
		t.Errorf("state = %q, want %q", state, "running")
	}
}

// A server may answer a browse with part of the listing and a continuation
// point. A partial listing must never pass for a complete one: a plant missing
// from a park listing is never commanded and never monitored, so the loss would
// be invisible.
func TestAdapterFollowsBrowseContinuationPoints(t *testing.T) {
	tr := browsePages(t, [][]string{
		{"Plant2", "Plant3"},
		{"Plant4", "Plant5"},
		{"Plant6"},
	}, true)

	nodes, err := tr.Browse(context.Background(), parkBranchPath, energontrol.BrowseFilter{})
	if err != nil {
		t.Fatalf("Browse: %v", err)
	}
	if len(nodes) != 5 {
		t.Fatalf("got %d nodes, want all 5 across the three pages: %+v", len(nodes), nodes)
	}
	for i, want := range []string{"Plant2", "Plant3", "Plant4", "Plant5", "Plant6"} {
		if nodes[i].Name != want {
			t.Errorf("node %d = %q, want %q", i, nodes[i].Name, want)
		}
	}
}

// More elements announced and no way to fetch them: an incomplete listing is an
// error, not a shorter park.
func TestAdapterRejectsAPagedBrowseWithoutAContinuationPoint(t *testing.T) {
	tr := browsePages(t, [][]string{{"Plant2"}, {"Plant3"}}, false)

	_, err := tr.Browse(context.Background(), parkBranchPath, energontrol.BrowseFilter{})
	if !errors.Is(err, energontrol.ErrBrowseIncomplete) {
		t.Fatalf("err = %v, want it to wrap energontrol.ErrBrowseIncomplete", err)
	}
}

// A server that keeps handing out the same continuation point would otherwise
// loop until the caller's context expires.
func TestAdapterRejectsARepeatedContinuationPoint(t *testing.T) {
	tr := newAdapterTest(t, func(action, body string) string {
		now := time.Now().Format(time.RFC3339)
		return fmt.Sprintf(
			`<BrowseResponse xmlns="http://opcfoundation.org/webservices/XMLDA/1.0/" `+
				`MoreElements="true" ContinuationPoint="always-the-same">`+
				`<BrowseResult RcvTime="%s" ReplyTime="%s" ServerState="running"/>`+
				`<Elements Name="Plant2" ItemName="Loc/Wec/Plant2" HasChildren="true"/>`+
				`</BrowseResponse>`, now, now)
	})

	_, err := tr.Browse(context.Background(), parkBranchPath, energontrol.BrowseFilter{})
	if !errors.Is(err, energontrol.ErrBrowseIncomplete) {
		t.Fatalf("err = %v, want it to wrap energontrol.ErrBrowseIncomplete", err)
	}
}

// End to end: a park the server pages is still the whole park.
func TestAdapterTurbinesFindsAPagedPark(t *testing.T) {
	var mu sync.Mutex
	parkPage := 0
	tr := newAdapterTest(t, func(action, body string) string {
		now := time.Now().Format(time.RFC3339)
		switch {
		case strings.Contains(action, "GetStatus"):
			return statusOK()
		case strings.Contains(action, "Read"):
			var items strings.Builder
			for _, it := range requestItems(body) {
				items.WriteString(goodItem(it, 4242))
			}
			return readReply(items.String(), "")
		case strings.Contains(action, "Browse"):
			if strings.Contains(body, `ItemPath="Loc/Wec"`) {
				mu.Lock()
				page := parkPage
				parkPage++
				mu.Unlock()
				if page == 0 {
					return fmt.Sprintf(
						`<BrowseResponse xmlns="http://opcfoundation.org/webservices/XMLDA/1.0/" `+
							`MoreElements="true" ContinuationPoint="p1">`+
							`<BrowseResult RcvTime="%s" ReplyTime="%s" ServerState="running"/>`+
							`<Elements Name="Plant2" ItemName="Loc/Wec/Plant2" HasChildren="true"/>`+
							`</BrowseResponse>`, now, now)
				}
				return fmt.Sprintf(
					`<BrowseResponse xmlns="http://opcfoundation.org/webservices/XMLDA/1.0/" `+
						`MoreElements="false">`+
						`<BrowseResult RcvTime="%s" ReplyTime="%s" ServerState="running"/>`+
						`<Elements Name="Plant3" ItemName="Loc/Wec/Plant3" HasChildren="true"/>`+
						`</BrowseResponse>`, now, now)
			}
			// Every plant offers a Ctrl branch with a SetCtrl item.
			elems := `<Elements Name="Ctrl" HasChildren="true"/>`
			if strings.Contains(body, `/Ctrl"`) {
				elems = `<Elements Name="SetCtrl" IsItem="true"/>`
			}
			return fmt.Sprintf(
				`<BrowseResponse xmlns="http://opcfoundation.org/webservices/XMLDA/1.0/" `+
					`MoreElements="false">`+
					`<BrowseResult RcvTime="%s" ReplyTime="%s" ServerState="running"/>`+
					`%s</BrowseResponse>`, now, now, elems)
		}
		t.Fatalf("unexpected SOAPAction %q", action)
		return ""
	})

	info, err := energontrol.New(tr).Turbines(context.Background())
	if err != nil {
		t.Fatalf("Turbines: %v", err)
	}
	if len(info.PlantNo) != 2 || info.PlantNo[0] != 2 || info.PlantNo[1] != 3 {
		t.Fatalf("plants = %v, want [2 3]: the second page was dropped", info.PlantNo)
	}
	for _, plant := range info.PlantNo {
		if !info.Ctrl[plant] {
			t.Errorf("plant %d: Ctrl not found", plant)
		}
	}
}

// A browse failure names the path that was refused, so an operator can see
// which node the SCADA would not list.
func TestAdapterBrowseFailureNamesThePath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer srv.Close()
	parsed, _ := url.Parse(srv.URL)
	tr := New(&gopcxmlda.Server{
		Url: parsed, LocaleID: "en-us", Timeout: 5 * time.Second,
	})

	_, err := tr.Browse(context.Background(), parkBranchPath, energontrol.BrowseFilter{})
	if err == nil {
		t.Fatal("a refused browse produced no error")
	}
	if want := "browse " + parkBranchPath; !strings.Contains(err.Error(), want) {
		t.Errorf("err = %q, want it to mention %q", err, want)
	}
}

// A browse answered by a server that is no longer running must not be used as a
// park listing.
func TestAdapterBrowseHonoursServerState(t *testing.T) {
	tr := newAdapterTest(t, func(action, body string) string {
		now := time.Now().Format(time.RFC3339)
		return fmt.Sprintf(
			`<BrowseResponse xmlns="http://opcfoundation.org/webservices/XMLDA/1.0/" `+
				`MoreElements="false">`+
				`<BrowseResult RcvTime="%s" ReplyTime="%s" ServerState="suspended"/>`+
				`<Elements Name="Plant2" ItemName="Loc/Wec/Plant2" HasChildren="true"/>`+
				`</BrowseResponse>`, now, now)
	})

	_, err := tr.Browse(context.Background(), parkBranchPath, energontrol.BrowseFilter{})
	if !errors.Is(err, energontrol.ErrServerNotRunning) {
		t.Fatalf("err = %v, want it to wrap energontrol.ErrServerNotRunning", err)
	}
}

// The filter is translated into the attributes OPC XML-DA defines for it.
func TestAdapterBrowseFilterReachesTheWire(t *testing.T) {
	var mu sync.Mutex
	var payload string
	tr := newAdapterTest(t, func(action, body string) string {
		mu.Lock()
		payload = body
		mu.Unlock()
		now := time.Now().Format(time.RFC3339)
		return fmt.Sprintf(
			`<BrowseResponse xmlns="http://opcfoundation.org/webservices/XMLDA/1.0/" `+
				`MoreElements="false">`+
				`<BrowseResult RcvTime="%s" ReplyTime="%s" ServerState="running"/>`+
				`</BrowseResponse>`, now, now)
	})

	if _, err := tr.Browse(context.Background(), plantBranchPath(2),
		energontrol.BrowseFilter{BranchesOnly: true, NamePattern: "Set*"}); err != nil {
		t.Fatalf("Browse: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{`BrowseFilter="branch"`, `ElementNameFilter="Set*"`} {
		if !strings.Contains(payload, want) {
			t.Errorf("browse payload is missing %s:\n%s", want, payload)
		}
	}
}

func TestToUint64(t *testing.T) {
	ok := []struct {
		in   any
		want uint64
	}{
		{uint64(130), 130}, {uint32(2), 2}, {uint16(175), 175}, {uint8(1), 1}, {uint(5), 5},
		{int(0), 0}, {int64(255), 255}, {int32(4), 4}, {int16(2), 2}, {int8(1), 1},
		{float64(129), 129}, {float32(2), 2},
	}
	for _, tc := range ok {
		got, err := toUint64(tc.in)
		if err != nil {
			t.Errorf("toUint64(%T %v): %v", tc.in, tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("toUint64(%T %v) = %d, want %d", tc.in, tc.in, got, tc.want)
		}
	}
	bad := []any{nil, "1", int(-1), int64(-5), float64(1.5), float64(-1), []uint64{1}, struct{}{}}
	for _, in := range bad {
		if got, err := toUint64(in); err == nil {
			t.Errorf("toUint64(%T %v) = %d, want an error", in, in, got)
		}
	}
}

// The OPC XML-DA option names are XML attribute names and therefore case
// sensitive. v1 sent "returnItemName", which a server ignores.
func TestRequestOptionsUseSpecCasing(t *testing.T) {
	for _, opts := range []map[string]interface{}{readOptions(0), writeOptions()} {
		for key := range opts {
			if key[0] < 'A' || key[0] > 'Z' {
				t.Errorf("option %q does not use the casing from the specification", key)
			}
		}
	}
	if _, ok := readOptions(0)[optReturnItemName]; !ok {
		t.Error("reads must request ItemName so responses can be correlated")
	}
}

// WithMaxStateAge demands a fresh value rather than only detecting an old one.
//
// The item timestamp says how old the answer was; MaxAge obliges the server to
// go to the device instead of answering from its cache. The first detects a
// stale value, the second prevents it — and until gopcxmlda v1.2.1 there was no
// way to send the attribute at all, so only the detecting half existed.
//
// The attribute lands on the ReadRequestItemList, which is where the
// specification puts it — not among the RequestOptions, which have no MaxAge.
func TestMaxAgeReachesTheWire(t *testing.T) {
	payload := readPayloadOf(t, energontrol.WithMaxStateAge(30*time.Second))

	if !strings.Contains(payload, `ItemList MaxAge="30000"`) {
		t.Errorf("MaxAge is not on the item list:\n%s", payload)
	}
	options := payload[strings.Index(payload, ":Options"):]
	options = options[:strings.Index(options, ">")]
	if strings.Contains(options, "MaxAge") {
		t.Errorf("MaxAge was rendered as a RequestOptions attribute: %s", options)
	}
}

// Without the option nothing is sent. The specification reads MaxAge 0 as "give
// me the most accurate data available", so sending it for a caller who never
// asked would silently turn every read into a device read.
func TestNoMaxAgeWithoutTheOption(t *testing.T) {
	if payload := readPayloadOf(t); strings.Contains(payload, "MaxAge") {
		t.Errorf("MaxAge was sent although the option is off:\n%s", payload)
	}
}

// Sub-millisecond ages round down to a device read, which is the strictest
// thing the attribute can express.
func TestSubMillisecondMaxAgeBecomesADeviceRead(t *testing.T) {
	payload := readPayloadOf(t, energontrol.WithMaxStateAge(500*time.Microsecond))
	if !strings.Contains(payload, `ItemList MaxAge="0"`) {
		t.Errorf("an age below a millisecond did not become a device read:\n%s", payload)
	}
}

// The timestamp check stays. A server that ignores MaxAge and answers from its
// cache anyway is still caught, which is the whole reason the second half was
// not dropped.
func TestTimestampCheckStillCatchesAServerThatIgnoresMaxAge(t *testing.T) {
	tr := newAdapterTest(t, func(action, body string) string {
		var items strings.Builder
		for _, it := range requestItems(body) {
			// Stale by an hour, despite the MaxAge the request carries.
			fmt.Fprintf(&items,
				`<Items ItemName="%s" ClientItemHandle="%s" Timestamp="%s">`+
					`<Value xsi:type="xsd:unsignedLong">0</Value>`+
					`<Quality QualityField="good"/></Items>`,
				it.name, it.handle, time.Now().UTC().Add(-time.Hour).Format(time.RFC3339))
		}
		return readReply(items.String(), "")
	})

	states, err := energontrol.New(tr, energontrol.WithMaxStateAge(30*time.Second)).
		PlantCtrlState(context.Background(), 2)
	if err != nil {
		t.Fatalf("PlantCtrlState: %v", err)
	}
	if !errors.Is(states[0].Err, energontrol.ErrStaleValue) {
		t.Errorf("err = %v, want it to wrap energontrol.ErrStaleValue", states[0].Err)
	}
}
