package ingest

import (
	"fmt"
	"testing"
)

// What loguru's Sentry integration sends for logger.error: the text in
// logentry, with a timestamp and whatever the call put in it.
func loguruEvent(at, url string) []byte {
	return fmt.Appendf(nil, `{"level":"error","logger":"src.phone.executor","logentry":{"message":"%s | ERROR    | src.phone.executor:upload_xml_dump:26 - UI dump: %s","formatted":"","params":[]}}`, at, url)
}

func exceptionEvent(value, function string, line int) []byte {
	return fmt.Appendf(nil, `{"exception":{"values":[{"type":"ConnectionClosedError","value":%q,"stacktrace":{"frames":[
		{"module":"agent.ws","filename":"agent/ws.py","function":%q,"lineno":%d,"in_app":true},
		{"module":"websockets.asyncio.connection","filename":"connection.py","function":"recv","lineno":317,"in_app":false}]}}]}}`, value, function, line)
}

func TestFingerprintGroupsTheSameProblem(t *testing.T) {
	same := []struct {
		name string
		a, b []byte
	}{
		{"a log message with another time and URL",
			loguruEvent("2026-10-14 09:28:07.716", "https://r2.example.com/ui_dumps/36fe97e1-dd9f-4cba-9d6c-afbd096f34aa_1791970087.xml?X-Amz-Signature=ef32"),
			loguruEvent("2026-10-14 08:55:34.386", "https://r2.example.com/ui_dumps/5e853e20-d8f6-46ac-b4ca-1a426104e4be_1791968133.xml?X-Amz-Signature=d2eb")},
		{"an exception with another value and line",
			exceptionEvent("no close frame received or sent", "run_forever", 86),
			exceptionEvent("sent 1011 (internal error) keepalive ping timeout", "run_forever", 91)},
		{"a logging call's template with other values",
			[]byte(`{"logger":"app","logentry":{"message":"payment %s failed","params":["a"],"formatted":"payment a failed"}}`),
			[]byte(`{"logger":"app","logentry":{"message":"payment %s failed","params":["b"],"formatted":"payment b failed"}}`)},
		{"the SDK's own fingerprint",
			[]byte(`{"fingerprint":["checkout"],"message":"one"}`),
			[]byte(`{"fingerprint":["checkout"],"message":"two"}`)},
	}
	for _, c := range same {
		if a, b := Fingerprint("error", "error", c.a), Fingerprint("error", "error", c.b); a != b {
			t.Errorf("%s: %s != %s", c.name, a, b)
		}
	}

	different := []struct {
		name string
		a, b []byte
	}{
		{"exceptions raised in other functions",
			exceptionEvent("x", "run_forever", 86),
			exceptionEvent("x", "send", 86)},
		{"messages of other loggers",
			[]byte(`{"logger":"a","message":"boom"}`),
			[]byte(`{"logger":"b","message":"boom"}`)},
		{"other messages",
			[]byte(`{"message":"database is down"}`),
			[]byte(`{"message":"disk is full"}`)},
	}
	for _, c := range different {
		if a, b := Fingerprint("error", "error", c.a), Fingerprint("error", "error", c.b); a == b {
			t.Errorf("%s: both %s", c.name, a)
		}
	}

	if Fingerprint("health", "error", nil) == Fingerprint("health", "info", nil) {
		t.Error("a failed health check and a recovered one are one issue")
	}
	if Fingerprint("crash", "", nil) == Fingerprint("oom", "", nil) {
		t.Error("a crash and an out-of-memory kill are one issue")
	}
}
