package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/x0ryz/hakobu/internal/ingest"
	"github.com/x0ryz/hakobu/internal/secret"
	"github.com/x0ryz/hakobu/internal/store/teldb"
)

// Apps' errors, logs and traces are kept on the server they run on, in its
// telemetry.db, sealed with its own key: the panel reads them over the link
// and keeps none. The panel's own server keeps its apps' in the panel's.

// Tel is this server's telemetry.db, opened when `hakobu node` starts.
var Tel *teldb.Queries

// TelemetryKeeper keeps apps' telemetry: a Node, or Telemetry itself.
type TelemetryKeeper interface {
	// AppEvents is an app's telemetry, newest first: with logs, or only
	// what went wrong (errors, crashes, out-of-memory kills, health).
	AppEvents(ctx context.Context, app int64, logs bool, limit int64) ([]teldb.TelemetryEvent, error)
	AppEvent(ctx context.Context, app int64, id int64) (teldb.TelemetryEvent, error)
	// AddAppEvent records what the panel saw of an app: a crash, an
	// out-of-memory kill, a health check.
	AddAppEvent(ctx context.Context, e teldb.CreateTelemetryEventParams) error
	// LastAppEvent is when the app last had an event of kind, "" if none.
	LastAppEvent(ctx context.Context, app int64, kind string) (string, error)
	AppTraces(ctx context.Context, app int64, limit int64) ([]teldb.ListTracesRow, error)
	AppTrace(ctx context.Context, app int64, id int64) (teldb.Trace, error)
	// TraceByID is the kept trace of app with the SDK's trace ID.
	TraceByID(ctx context.Context, app int64, traceID string) (int64, error)
	// TraceRoutes is the app's requests counted by route and hour, from
	// the hour since (Unix) on.
	TraceRoutes(ctx context.Context, app int64, since int64) ([]teldb.TraceRoute, error)
	DropTelemetry(ctx context.Context, app int64) error
	// StoreEnvelope keeps what a Sentry envelope of app holds: errors,
	// logs and traces; other items are dropped.
	StoreEnvelope(ctx context.Context, app int64, body []byte) error
}

// Telemetry is a TelemetryKeeper over a telemetry.db.
type Telemetry struct{ Q *teldb.Queries }

var _ TelemetryKeeper = Telemetry{}

var errNoTelemetry = errors.New("this server keeps no telemetry")

func (t Telemetry) q() (*teldb.Queries, error) {
	if t.Q == nil {
		return nil, errNoTelemetry
	}
	return t.Q, nil
}

func (t Telemetry) AppEvents(ctx context.Context, app int64, logs bool, limit int64) ([]teldb.TelemetryEvent, error) {
	q, err := t.q()
	if err != nil {
		return nil, err
	}
	if logs {
		return q.ListTelemetryEvents(ctx, teldb.ListTelemetryEventsParams{AppID: app, Limit: limit})
	}
	return q.ListProblems(ctx, teldb.ListProblemsParams{AppID: app, Limit: limit})
}

func (t Telemetry) AppEvent(ctx context.Context, app int64, id int64) (teldb.TelemetryEvent, error) {
	q, err := t.q()
	if err != nil {
		return teldb.TelemetryEvent{}, err
	}
	return q.GetTelemetryEvent(ctx, teldb.GetTelemetryEventParams{ID: id, AppID: app})
}

func (t Telemetry) AddAppEvent(ctx context.Context, e teldb.CreateTelemetryEventParams) error {
	q, err := t.q()
	if err != nil {
		return err
	}
	return q.CreateTelemetryEvent(ctx, e)
}

func (t Telemetry) LastAppEvent(ctx context.Context, app int64, kind string) (string, error) {
	q, err := t.q()
	if err != nil {
		return "", err
	}
	return q.LastTelemetryOfKind(ctx, teldb.LastTelemetryOfKindParams{AppID: app, Kind: kind})
}

func (t Telemetry) AppTraces(ctx context.Context, app int64, limit int64) ([]teldb.ListTracesRow, error) {
	q, err := t.q()
	if err != nil {
		return nil, err
	}
	return q.ListTraces(ctx, teldb.ListTracesParams{AppID: app, Limit: limit})
}

func (t Telemetry) AppTrace(ctx context.Context, app int64, id int64) (teldb.Trace, error) {
	q, err := t.q()
	if err != nil {
		return teldb.Trace{}, err
	}
	return q.GetTrace(ctx, teldb.GetTraceParams{ID: id, AppID: app})
}

func (t Telemetry) TraceByID(ctx context.Context, app int64, traceID string) (int64, error) {
	q, err := t.q()
	if err != nil {
		return 0, err
	}
	return q.TraceByTraceID(ctx, teldb.TraceByTraceIDParams{AppID: app, TraceID: traceID})
}

func (t Telemetry) TraceRoutes(ctx context.Context, app int64, since int64) ([]teldb.TraceRoute, error) {
	q, err := t.q()
	if err != nil {
		return nil, err
	}
	return q.ListTraceRoutes(ctx, teldb.ListTraceRoutesParams{AppID: app, Hour: since})
}

func (t Telemetry) DropTelemetry(ctx context.Context, app int64) error {
	q, err := t.q()
	if err != nil {
		return err
	}
	for _, del := range []func(context.Context, int64) error{q.DeleteTracesOfApp, q.DeleteTraceRoutesOfApp, q.DeleteTelemetryOfApp} {
		if err := del(ctx, app); err != nil {
			return err
		}
	}
	return nil
}

// maxEventsPerEnvelope caps the rows one envelope can add: SDKs batch at
// most 100 log records, and each record is a row of its own.
const maxEventsPerEnvelope = 1000

func (t Telemetry) StoreEnvelope(ctx context.Context, app int64, body []byte) error {
	q, err := t.q()
	if err != nil {
		return err
	}
	items, err := ingest.ParseEnvelope(body)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadEnvelope, err)
	}
	saved := 0
	var errs []error
	save := func(kind string, sum ingest.Summary, payload []byte) {
		if saved++; saved > maxEventsPerEnvelope {
			return
		}
		if err := q.CreateTelemetryEvent(ctx, teldb.CreateTelemetryEventParams{AppID: app, Kind: kind, Level: sum.Level, Message: secret.String(sum.Message), Payload: secret.String(payload), TraceID: sum.TraceID}); err != nil {
			errs = append(errs, err)
		}
	}
	for _, item := range items {
		switch item.Type {
		case "event":
			save("error", ingest.ExtractEventSummary(item), item.Payload)
		case "log":
			for _, entry := range ingest.ExtractLogEntries(item) {
				save("log", entry.Summary, entry.Payload)
			}
		case "transaction":
			if tx, ok := ingest.ExtractTransaction(item); ok {
				if err := RecordTransaction(ctx, q, app, tx, item.Payload); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// ErrBadEnvelope is an envelope that can't be read.
var ErrBadEnvelope = errors.New("invalid envelope")

// Apps whose Sentry SDK traces requests (traces_sample_rate) send each one
// as a transaction. They're all counted by route and hour, and the slow and
// failed ones kept whole for a week (migration 003_traces.sql).

// slowTrace is how long a request takes to be kept whole.
const slowTrace = time.Second

// MaxRoutesPerHour caps the routes counted for an app in an hour: names
// that still carry IDs mustn't grow the table without end. The rest count
// as OtherRoute.
const (
	MaxRoutesPerHour = 500
	OtherRoute       = "(other routes)"
)

// BucketBounds are the upper bounds in milliseconds of the histogram's
// buckets but the last, which has none.
var BucketBounds = [...]int64{10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

func bucketOf(ms int64) int64 {
	for i, b := range BucketBounds {
		if ms < b {
			return int64(i)
		}
	}
	return int64(len(BucketBounds))
}

// RecordTransaction counts a transaction of app and keeps it whole if it
// was slow or failed.
func RecordTransaction(ctx context.Context, q *teldb.Queries, app int64, tx ingest.Transaction, payload []byte) error {
	now := time.Now()
	hour := now.Truncate(time.Hour).Unix()
	name := tx.Name
	if known, err := q.HasTraceRoute(ctx, teldb.HasTraceRouteParams{AppID: app, Hour: hour, Name: name}); err == nil && !known {
		if n, err := q.CountTraceRoutes(ctx, teldb.CountTraceRoutesParams{AppID: app, Hour: hour}); err == nil && n >= MaxRoutesPerHour {
			name = OtherRoute
		}
	}
	ms := tx.Duration.Milliseconds()
	var failed int64
	if tx.Failed() {
		failed = 1
	}
	if err := q.CountTrace(ctx, teldb.CountTraceParams{AppID: app, Name: name, Hour: hour, Errors: failed, Ms: ms, Bucket: bucketOf(ms)}); err != nil {
		return err
	}
	if !tx.Failed() && tx.Duration < slowTrace {
		return nil
	}
	slow := ""
	if span, ok := tx.Slowest(); ok {
		slow = fmt.Sprintf("%s %s (%d ms)", span.Op, span.Description, span.Duration.Milliseconds())
	}
	return q.CreateTrace(ctx, teldb.CreateTraceParams{
		AppID: app, TraceID: tx.TraceID, Name: tx.Name, Status: tx.Status, HttpStatus: int64(tx.HTTPStatus),
		DurationMs: ms, SlowSpan: secret.String(slow), Payload: secret.String(payload), CreatedAt: now.Unix(),
	})
}

// Local keeps telemetry in Tel.

func (Local) AppEvents(ctx context.Context, app int64, logs bool, limit int64) ([]teldb.TelemetryEvent, error) {
	return Telemetry{Tel}.AppEvents(ctx, app, logs, limit)
}

func (Local) AppEvent(ctx context.Context, app int64, id int64) (teldb.TelemetryEvent, error) {
	return Telemetry{Tel}.AppEvent(ctx, app, id)
}

func (Local) AddAppEvent(ctx context.Context, e teldb.CreateTelemetryEventParams) error {
	return Telemetry{Tel}.AddAppEvent(ctx, e)
}

func (Local) LastAppEvent(ctx context.Context, app int64, kind string) (string, error) {
	return Telemetry{Tel}.LastAppEvent(ctx, app, kind)
}

func (Local) AppTraces(ctx context.Context, app int64, limit int64) ([]teldb.ListTracesRow, error) {
	return Telemetry{Tel}.AppTraces(ctx, app, limit)
}

func (Local) AppTrace(ctx context.Context, app int64, id int64) (teldb.Trace, error) {
	return Telemetry{Tel}.AppTrace(ctx, app, id)
}

func (Local) TraceByID(ctx context.Context, app int64, traceID string) (int64, error) {
	return Telemetry{Tel}.TraceByID(ctx, app, traceID)
}

func (Local) TraceRoutes(ctx context.Context, app int64, since int64) ([]teldb.TraceRoute, error) {
	return Telemetry{Tel}.TraceRoutes(ctx, app, since)
}

func (Local) DropTelemetry(ctx context.Context, app int64) error {
	return Telemetry{Tel}.DropTelemetry(ctx, app)
}

func (Local) StoreEnvelope(ctx context.Context, app int64, body []byte) error {
	return Telemetry{Tel}.StoreEnvelope(ctx, app, body)
}
