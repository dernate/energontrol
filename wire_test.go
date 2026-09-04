package energontrol

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda"
)

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
	client := New(server, WithSessionPolling(time.Millisecond, 5*time.Millisecond))
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
	states, err := New(server).PlantCtrlState(context.Background(), 2)
	if err != nil {
		t.Fatalf("PlantCtrlState: %v", err)
	}
	if states[0].Ctrl != CtrlStop90 {
		t.Errorf("state = %s, want %s", states[0].Ctrl, CtrlStop90)
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
}

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
