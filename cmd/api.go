package cmd

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"

	"github.com/x0ryz/hako/internal/ops"
	"github.com/x0ryz/hako/internal/store"
)

// registerAPIRoutes wires the JSON HTTP API (for scripts/MCP/etc.) used to
// operate on projects/databases running on this (server) machine, guarded
// by a bearer token. Routes live under /console-api rather than /api
// because /api/{project_id}/envelope/ is reserved for the Sentry-compatible
// ingestion endpoint (cmd/ingest.go) — that path is dictated by every
// Sentry SDK's DSN handling, so it can't move; this one can.
func registerAPIRoutes(mux *http.ServeMux, s *store.Store, token string) {
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got := r.Header.Get("Authorization")
			want := "Bearer " + token
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}

	mux.HandleFunc("GET /console-api/projects", auth(func(w http.ResponseWriter, r *http.Request) {
		projects, err := s.ListProjects()
		writeJSON(w, projects, err)
	}))

	mux.HandleFunc("GET /console-api/databases", auth(func(w http.ResponseWriter, r *http.Request) {
		databases, err := s.ListDatabases()
		writeJSON(w, databases, err)
	}))

	mux.HandleFunc("POST /console-api/projects/{name}/redeploy", auth(func(w http.ResponseWriter, r *http.Request) {
		containerID, err := ops.RedeployProject(s, r.PathValue("name"))
		writeJSON(w, map[string]string{"container_id": containerID}, err)
	}))

	mux.HandleFunc("POST /console-api/projects/{name}/rollback", auth(func(w http.ResponseWriter, r *http.Request) {
		containerID, err := ops.RollbackProject(s, r.PathValue("name"))
		writeJSON(w, map[string]string{"container_id": containerID}, err)
	}))

	mux.HandleFunc("DELETE /console-api/projects/{name}", auth(func(w http.ResponseWriter, r *http.Request) {
		err := ops.DeleteProject(s, r.PathValue("name"))
		writeJSON(w, map[string]string{"status": "ok"}, err)
	}))

	mux.HandleFunc("POST /console-api/projects/{name}/domain", auth(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Domain string `json:"domain"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		err := ops.SetProjectDomain(s, r.PathValue("name"), body.Domain)
		writeJSON(w, map[string]string{"status": "ok"}, err)
	}))

	mux.HandleFunc("POST /console-api/projects/{name}/link", auth(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Database string `json:"database"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		err := ops.LinkDatabase(s, r.PathValue("name"), body.Database)
		writeJSON(w, map[string]string{"status": "ok"}, err)
	}))

	mux.HandleFunc("POST /console-api/databases", auth(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name    string `json:"name"`
			Kind    string `json:"kind"`
			Version string `json:"version"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		containerID, err := ops.CreateDatabase(s, body.Name, body.Kind, body.Version)
		writeJSON(w, map[string]string{"container_id": containerID}, err)
	}))
}

func writeJSON(w http.ResponseWriter, v interface{}, err error) {
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
