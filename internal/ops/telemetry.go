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

// appTelemetry is where app's telemetry is kept.
func appTelemetry(s *store.Store, app store.App) (node.TelemetryKeeper, error) {
	if sv, ok := appServer(s, app); ok && sv.ID != 0 {
		n, err := serverNode(sv)
		if err != nil {
			return nil, fmt.Errorf("%s keeps its errors and logs on %s, which isn't connected: %w", app.Name, sv.label(), err)
		}
		return n, nil
	}
	return node.Telemetry{Q: s.Tel}, nil
}

// appTelemetryByID is appTelemetry for the app with ID id; an app the
// panel doesn't know (deleted since) is on no other server.
func appTelemetryByID(s *store.Store, id int64) (node.TelemetryKeeper, error) {
	a, err := s.GetAppByID(ctx(), id)
	if err != nil {
		return node.Telemetry{Q: s.Tel}, nil
	}
	return appTelemetry(s, a)
}

// AppEvents lists an app's telemetry, newest first: with logs, or only
// what went wrong (errors, crashes, out-of-memory kills, health checks),
// which an app's logs would otherwise bury.
func AppEvents(s *store.Store, app store.App, logs bool, limit int64) ([]teldb.TelemetryEvent, error) {
	t, err := appTelemetry(s, app)
	if err != nil {
		return nil, err
	}
	return t.AppEvents(ctx(), app.ID, logs, limit)
}

// AppEvent is one of an app's events.
func AppEvent(s *store.Store, app store.App, id int64) (teldb.TelemetryEvent, error) {
	t, err := appTelemetry(s, app)
	if err != nil {
		return teldb.TelemetryEvent{}, err
	}
	return t.AppEvent(ctx(), app.ID, id)
}

// AppTraces is an app's kept traces, newest first.
func AppTraces(s *store.Store, app store.App, limit int64) ([]teldb.ListTracesRow, error) {
	t, err := appTelemetry(s, app)
	if err != nil {
		return nil, err
	}
	return t.AppTraces(ctx(), app.ID, limit)
}

// TraceByID is the kept trace of app with the SDK's trace ID.
func TraceByID(s *store.Store, app store.App, traceID string) (int64, error) {
	t, err := appTelemetry(s, app)
	if err != nil {
		return 0, err
	}
	return t.TraceByID(ctx(), app.ID, traceID)
}

// StoreEnvelope keeps what an envelope of the app with ID id holds where
// its telemetry is.
func StoreEnvelope(s *store.Store, id int64, body []byte) error {
	t, err := appTelemetryByID(s, id)
	if err != nil {
		return err
	}
	return t.StoreEnvelope(ctx(), id, body)
}

// addAppEvent records what the panel saw of an app where its telemetry is.
func addAppEvent(s *store.Store, e teldb.CreateTelemetryEventParams) error {
	t, err := appTelemetryByID(s, e.AppID)
	if err != nil {
		return err
	}
	return t.AddAppEvent(ctx(), e)
}
