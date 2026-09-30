package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/capydatabase/capydb-cli/internal/api"
)

func TestKVStateLabel(t *testing.T) {
	cases := map[string]api.KVStore{
		"running": {State: "running"},
		"stopped (the organization is suspended and offline; the store keeps its data and starts again when the suspension lifts)": {State: "stopped", StoppedReason: "org_suspended"},
		"stopped (the store keeps its data)": {State: "stopped"},
	}
	for want, store := range cases {
		if got := kvStateLabel(store); got != want {
			t.Errorf("kvStateLabel(%+v) = %q, want %q", store, got, want)
		}
	}
}

func TestBackupTableMarksExpiredBackups(t *testing.T) {
	backups := []api.Backup{
		{ID: "bk_1", State: "completed", VerificationState: "verified", CreatedAt: time.Now(), BackupKey: "k1"},
		{ID: "bk_2", State: "expired", VerificationState: "verified", CreatedAt: time.Now(), BackupKey: "k2"},
	}
	var out bytes.Buffer
	writeBackupTable(&out, backups)
	if !strings.Contains(out.String(), "expired*") || !strings.Contains(out.String(), "cannot be restored") {
		t.Fatalf("expired backup not explained:\n%s", out.String())
	}

	out.Reset()
	writeBackupTable(&out, backups[:1])
	if strings.Contains(out.String(), "expired") {
		t.Fatalf("note printed without an expired backup:\n%s", out.String())
	}
}
