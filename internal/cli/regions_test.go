package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/capydatabase/capydb-cli/internal/api"
)

func TestRegionsShowsDisplayNames(t *testing.T) {
	server := newFakeControlPlane(t, nil, map[string]http.HandlerFunc{
		"GET /v1/regions": func(w http.ResponseWriter, r *http.Request) { writeJSON(t, w, regionsFixture()) },
	})

	// The bare command lists, like `regions list`.
	for _, args := range [][]string{{"regions"}, {"regions", "list"}} {
		output, err := runCommand(t, t.TempDir(), append(args, "--api-url", server.URL, "--api-key", "capy_test")...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, output)
		}
		for _, want := range []string{"REGION", "NAME", "LOCATION", "eu-north-1", "EU North 1", "Helsinki, Finland"} {
			if !strings.Contains(output, want) {
				t.Fatalf("%v output missing %q:\n%s", args, want, output)
			}
		}
	}

	output, err := runCommand(t, t.TempDir(), "regions", "-o", "json", "--api-url", server.URL, "--api-key", "capy_test")
	if err != nil {
		t.Fatalf("regions json: %v\n%s", err, output)
	}
	var payload struct {
		Regions []api.RegionDetail `json:"regions"`
	}
	if err := json.Unmarshal([]byte(output), &payload); err != nil {
		t.Fatalf("decode %q: %v", output, err)
	}
	if len(payload.Regions) != 1 || payload.Regions[0].ID != "eu-north-1" || payload.Regions[0].DisplayName != "EU North 1" {
		t.Fatalf("json regions = %+v", payload.Regions)
	}
}

func TestSelectRegionMatchesIDsAndPassesOthersThrough(t *testing.T) {
	regions := []api.RegionDetail{{ID: "eu-north-1", DisplayName: "EU North 1", Location: "Helsinki, Finland"}}

	got, err := selectRegion(regions, "EU-NORTH-1", true)
	if err != nil || got != "eu-north-1" {
		t.Fatalf("selectRegion(listed) = %q, %v", got, err)
	}
	// A deprecated name is not listed; the control plane resolves it.
	got, err = selectRegion(regions, "hel1", true)
	if err != nil || got != "hel1" {
		t.Fatalf("selectRegion(alias) = %q, %v", got, err)
	}
	got, err = selectRegion(regions, "", true)
	if err != nil || got != "" {
		t.Fatalf("selectRegion(non-interactive) = %q, %v", got, err)
	}
}

func TestRegionLabel(t *testing.T) {
	cases := map[string]api.RegionDetail{
		"eu-north-1 (EU North 1, Helsinki, Finland)": {ID: "eu-north-1", DisplayName: "EU North 1", Location: "Helsinki, Finland"},
		"eu-north-1":           {ID: "eu-north-1"},
		"us-east-1 (Virginia)": {ID: "us-east-1", DisplayName: "us-east-1", Location: "Virginia"},
	}
	for want, region := range cases {
		if got := regionLabel(region); got != want {
			t.Errorf("regionLabel(%+v) = %q, want %q", region, got, want)
		}
	}
}
