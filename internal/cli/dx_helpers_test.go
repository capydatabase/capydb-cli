package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeProject is the one project the fake control plane serves.
var fakeProject = map[string]any{
	"id":             "prj_1",
	"name":           "demo",
	"slug":           "demo",
	"environment":    "non_production",
	"state":          "ready",
	"runtime_status": "active",
	"plan":           "ship",
	"region":         "eu-north-1",
}

// newFakeControlPlane serves GET /v1/projects and GET /v1/projects/prj_1 (with
// project overrides) plus the given "METHOD /path" routes; anything else
// fails the test.
func newFakeControlPlane(t *testing.T, project map[string]any, routes map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	merged := map[string]any{}
	for key, value := range fakeProject {
		merged[key] = value
	}
	for key, value := range project {
		merged[key] = value
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		if handler, ok := routes[key]; ok {
			handler(w, r)
			return
		}
		switch key {
		case "GET /v1/projects":
			writeJSON(t, w, map[string]any{"projects": []any{merged}})
		case "GET /v1/projects/prj_1":
			writeJSON(t, w, map[string]any{"project": merged})
		default:
			t.Errorf("unexpected request: %s", key)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// regionsFixture is the GET /v1/regions payload: neutral ids plus their
// display labels.
func regionsFixture() map[string]any {
	return map[string]any{
		"regions": []string{"eu-north-1"},
		"region_details": []map[string]any{
			{"id": "eu-north-1", "display_name": "EU North 1", "location": "Helsinki, Finland"},
		},
	}
}
