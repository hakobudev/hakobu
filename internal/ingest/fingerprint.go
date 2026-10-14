package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
)

// Fingerprint is the issue an event belongs to: events of the same kind
// with the same fingerprint are one problem seen again. Like Sentry, an
// exception goes by its type and where it was raised (the functions on its
// stack, not their line numbers, which move with any edit above them); a
// message without one by its logger and its text with the numbers, IDs
// and URLs taken out, which differ every time. An SDK's own fingerprint
// wins. Events hakobu records itself (crashes, out-of-memory kills, health
// checks) go by kind and level: there's one of each per app.
//
// It's a hash: the column isn't encrypted like the message it comes from.
func Fingerprint(kind, level string, payload []byte) string {
	parts := []string{kind}
	if kind == "error" {
		parts = append(parts, eventKey(payload)...)
	} else {
		parts = append(parts, level)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}

func eventKey(payload []byte) []string {
	var e struct {
		Fingerprint []string        `json:"fingerprint"`
		Message     json.RawMessage `json:"message"`
		LogEntry    struct {
			Message   string `json:"message"`
			Formatted string `json:"formatted"`
		} `json:"logentry"`
		Logger    string          `json:"logger"`
		Exception json.RawMessage `json:"exception"`
	}
	_ = json.Unmarshal(payload, &e) // what isn't there stays empty

	if len(e.Fingerprint) > 1 || len(e.Fingerprint) == 1 && e.Fingerprint[0] != "{{ default }}" {
		return append([]string{"sdk"}, e.Fingerprint...)
	}

	type frame struct {
		Module   string `json:"module"`
		Filename string `json:"filename"`
		Function string `json:"function"`
		InApp    *bool  `json:"in_app"`
	}
	type exception struct {
		Type       string `json:"type"`
		Stacktrace struct {
			Frames []frame `json:"frames"`
		} `json:"stacktrace"`
	}
	var exceptions struct {
		Values []exception `json:"values"`
	}
	if json.Unmarshal(e.Exception, &exceptions) != nil {
		_ = json.Unmarshal(e.Exception, &exceptions.Values)
	}
	if n := len(exceptions.Values); n > 0 {
		// The last is the one raised; the ones before it led to it.
		ex := exceptions.Values[n-1]
		key := []string{"exception", ex.Type}
		frames := ex.Stacktrace.Frames
		var own []frame
		for _, f := range frames {
			if f.InApp != nil && *f.InApp {
				own = append(own, f)
			}
		}
		if len(own) > 0 {
			frames = own // the app's code, not the libraries it went through
		}
		for _, f := range frames {
			key = append(key, f.Module+"|"+f.Filename+"|"+f.Function)
		}
		return key
	}

	var msg string
	if json.Unmarshal(e.Message, &msg) != nil {
		_ = json.Unmarshal(e.Message, &e.LogEntry)
	}
	if msg == "" {
		// The template of a logging call ("Failed %s"), before its values.
		msg = e.LogEntry.Message
		if msg == "" {
			msg = e.LogEntry.Formatted
		}
	}
	msg, _, _ = strings.Cut(strings.TrimSpace(msg), "\n")
	return []string{"message", e.Logger, normalizeMessage(msg)}
}

var (
	urlPattern    = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://\S+`)
	hexPattern    = regexp.MustCompile(`\b[0-9a-fA-F]{8,}(-[0-9a-fA-F]{4,})*\b`)
	numberPattern = regexp.MustCompile(`\d+`)
	spacePattern  = regexp.MustCompile(`\s+`)
)

// normalizeMessage takes out of a message what differs between two of the
// same: URLs, IDs and hashes, numbers (which takes timestamps too).
func normalizeMessage(msg string) string {
	msg = urlPattern.ReplaceAllString(msg, "<url>")
	msg = hexPattern.ReplaceAllString(msg, "<id>")
	msg = numberPattern.ReplaceAllString(msg, "<n>")
	return strings.TrimSpace(spacePattern.ReplaceAllString(msg, " "))
}
