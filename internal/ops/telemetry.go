package ops

import (
	"fmt"

	"github.com/x0ryz/hakobu/internal/node"
	"github.com/x0ryz/hakobu/internal/store"
	"github.com/x0ryz/hakobu/internal/store/teldb"
)

// An app's errors, logs and traces are kept on the server it runs on
// (node.Telemetry): the panel's own server's in the panel's telemetry.db,
// another server's on it, read over the link while it's connected.

// appTelemetry is where app's telemetry is kept; an app the panel doesn't
// know (deleted since) is on no other server.
func appTelemetry(s *store.Store, app string) (node.TelemetryKeeper, error) {
	a, err := s.GetApp(ctx(), app)
	if err != nil {
		return node.Telemetry{Q: s.Tel}, nil
	}
	if sv, ok := appServer(s, a); ok && sv.ID != 0 {
		n, err := serverNode(sv.Name)
		if err != nil {
			return nil, fmt.Errorf("%s keeps its errors and logs on server %s, which isn't connected: %w", app, sv.Name, err)
		}
		return n, nil
	}
	return node.Telemetry{Q: s.Tel}, nil
}

// AppEvents lists an app's telemetry, newest first: with logs, or only
// what went wrong (errors, crashes, out-of-memory kills, health checks),
// which an app's logs would otherwise bury.
func AppEvents(s *store.Store, app string, logs bool, limit int64) ([]teldb.TelemetryEvent, error) {
	t, err := appTelemetry(s, app)
	if err != nil {
		return nil, err
	}
	return t.AppEvents(ctx(), app, logs, limit)
}

// AppEvent is one of an app's events.
func AppEvent(s *store.Store, app string, id int64) (teldb.TelemetryEvent, error) {
	t, err := appTelemetry(s, app)
	if err != nil {
		return teldb.TelemetryEvent{}, err
	}
	return t.AppEvent(ctx(), app, id)
}

// AppTraces is an app's kept traces, newest first.
func AppTraces(s *store.Store, app string, limit int64) ([]teldb.ListTracesRow, error) {
	t, err := appTelemetry(s, app)
	if err != nil {
		return nil, err
	}
	return t.AppTraces(ctx(), app, limit)
}

// TraceByID is the kept trace of app with the SDK's trace ID.
func TraceByID(s *store.Store, app, traceID string) (int64, error) {
	t, err := appTelemetry(s, app)
	if err != nil {
		return 0, err
	}
	return t.TraceByID(ctx(), app, traceID)
}

// StoreEnvelope keeps what an envelope of app holds where its telemetry is.
func StoreEnvelope(s *store.Store, app string, body []byte) error {
	t, err := appTelemetry(s, app)
	if err != nil {
		return err
	}
	return t.StoreEnvelope(ctx(), app, body)
}

// addAppEvent records what the panel saw of app where its telemetry is.
func addAppEvent(s *store.Store, e teldb.CreateTelemetryEventParams) error {
	t, err := appTelemetry(s, e.AppName)
	if err != nil {
		return err
	}
	return t.AddAppEvent(ctx(), e)
}
