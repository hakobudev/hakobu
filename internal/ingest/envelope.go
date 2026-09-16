// Package ingest parses Sentry's envelope wire format, so any official
// Sentry SDK can send errors and structured logs straight to hako's
// own ingestion endpoint (cmd/ingest.go) instead of a Sentry account.
package ingest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
)

// Item is one envelope item: a type tag ("event", "transaction", "log", ...)
// and its raw JSON payload, kept unparsed beyond what a summary needs so a
// later feature can read fields this package doesn't know about yet.
type Item struct {
	Type    string
	Payload []byte
}

// ParseEnvelope parses the newline-delimited envelope format described at
// https://develop.sentry.dev/sdk/data-model/envelopes/: an envelope header
// line, followed by (item header, item payload) pairs. An item header's
// "length" field, when present, is the exact byte count of its payload
// (which may itself contain newlines); when absent, the payload is exactly
// the following line.
func ParseEnvelope(body []byte) ([]Item, error) {
	r := bufio.NewReader(bytes.NewReader(body))

	// Envelope header — carries event_id/dsn/sent_at, none of which this
	// package needs; just consume the line to reach the items.
	if _, err := r.ReadBytes('\n'); err != nil && err != io.EOF {
		return nil, err
	}

	var items []Item
	for {
		headerLine, err := r.ReadBytes('\n')
		trimmed := bytes.TrimRight(headerLine, "\n")
		if len(trimmed) == 0 {
			break
		}

		var header struct {
			Type   string `json:"type"`
			Length *int   `json:"length"`
		}
		if jsonErr := json.Unmarshal(trimmed, &header); jsonErr != nil {
			return items, jsonErr
		}

		var payload []byte
		if header.Length != nil {
			payload = make([]byte, *header.Length)
			if _, readErr := io.ReadFull(r, payload); readErr != nil {
				return items, readErr
			}
			r.ReadByte() // consume the payload's trailing newline, if any
		} else {
			payloadLine, readErr := r.ReadBytes('\n')
			payload = bytes.TrimRight(payloadLine, "\n")
			if readErr != nil && readErr != io.EOF {
				return items, readErr
			}
		}

		items = append(items, Item{Type: header.Type, Payload: payload})

		if err == io.EOF {
			break
		}
	}
	return items, nil
}

// Summary is what's worth storing as a searchable row for an error or log
// item — the rest of the item stays in TelemetryEvent.Payload for a detail
// view.
type Summary struct {
	Level   string
	Message string
}

// ExtractEventSummary reads an "event" or "transaction" item's top-level
// message, falling back to the first exception's "type: value" when the SDK
// didn't set one directly (the common case for uncaught exceptions).
func ExtractEventSummary(item Item) Summary {
	var e struct {
		Message   string `json:"message"`
		Level     string `json:"level"`
		Exception struct {
			Values []struct {
				Type  string `json:"type"`
				Value string `json:"value"`
			} `json:"values"`
		} `json:"exception"`
	}
	json.Unmarshal(item.Payload, &e)

	msg := e.Message
	if msg == "" && len(e.Exception.Values) > 0 {
		v := e.Exception.Values[0]
		msg = v.Type + ": " + v.Value
	}
	level := e.Level
	if level == "" {
		level = "error"
	}
	return Summary{Level: level, Message: msg}
}

// ExtractLogEntries reads a "log" item, which batches multiple log records
// per Sentry's structured-logs format (one envelope item, an "items" array
// of {level, body, ...} records).
func ExtractLogEntries(item Item) []Summary {
	var l struct {
		Items []struct {
			Level string `json:"level"`
			Body  string `json:"body"`
		} `json:"items"`
	}
	json.Unmarshal(item.Payload, &l)

	summaries := make([]Summary, 0, len(l.Items))
	for _, entry := range l.Items {
		summaries = append(summaries, Summary{Level: entry.Level, Message: entry.Body})
	}
	return summaries
}
