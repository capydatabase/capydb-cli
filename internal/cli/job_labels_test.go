package cli

import "testing"

// apiJobTypes is the Job.type enum of the control-plane OpenAPI spec
// (backend/internal/httpapi/openapi.json). Text output must never print a raw
// kind, so every one of them needs a customer-facing label.
var apiJobTypes = []string{
	"project.apply_plan", "project.backup", "project.export", "project.import", "project.restore",
	"project.rotate_credentials", "host.health_probe", "project.promote_preview", "project.backup_delete",
	"integration.sync_env", "kv.create", "kv.destroy", "kv.flush", "kv.resize", "kv.rotate_token",
	"integration.clerk_backfill", "project.extension_enable", "project.extension_update",
	"project.upgrade_major_preflight", "project.upgrade_minor", "project.upgrade_major",
	"project.upgrade_major_confirm", "project.upgrade_major_rollback", "project.relocate",
	"project.extension_disable", "project.expire_credential", "project.app_role_enable",
	"project.app_role_rotate", "project.import_follow_start", "project.import_follow_status",
	"project.import_follow_cutover", "project.import_follow_abort", "instance.create", "instance.destroy",
	"branch.create", "instance.pitr_restore", "instance.sleep", "instance.wake", "instance.seed_standby",
	"instance.set_readonly", "instance.arc_cache", "instance.recordsize", "kv.start", "kv.stop",
}

func TestEveryAPIJobTypeHasALabel(t *testing.T) {
	for _, jobType := range apiJobTypes {
		if _, ok := jobTypeLabels[jobType]; !ok {
			t.Errorf("job type %q has no customer-facing label", jobType)
		}
	}
}
