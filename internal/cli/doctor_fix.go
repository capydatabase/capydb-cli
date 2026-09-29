package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/api"
	"github.com/capydatabase/capydb-cli/internal/config"
	"github.com/capydatabase/capydb-cli/internal/configlint"
	"github.com/capydatabase/capydb-cli/internal/envfile"
	"github.com/capydatabase/capydb-cli/internal/gitignore"
	"github.com/capydatabase/capydb-cli/internal/project"
	"github.com/capydatabase/capydb-cli/internal/scan"
)

// `capydb doctor --fix` applies the remedies for findings doctor already
// reports. Two kinds:
//
//   - safe: only adds what is missing - database env vars absent from the
//     linked env file, drizzle-kit's schemaFilter, Prisma's directUrl, the
//     direct URL for drizzle-kit credentials. Existing values are never
//     overwritten. Applied without asking.
//   - destructive: removes something - a stale project link, database vars
//     that shadow the linked env file from other env files. Asked one by one
//     on a terminal; applied without asking only with --yes; otherwise
//     skipped with the command to run.
//
// Everything else doctor reports (auth, psql, pooled prepared statements in
// code, the migration history) needs a person and is listed as manual.

const (
	fixApplied = "fixed"
	fixSkipped = "skipped"
	fixManual  = "manual"
)

type doctorFix struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// fixConfirmer asks before a destructive fix.
type fixConfirmer func(question string) (bool, error)

func (a *app) newFixConfirmer(cmd *cobra.Command, yes bool) fixConfirmer {
	return func(question string) (bool, error) {
		if yes {
			return true, nil
		}
		if !stdinIsInteractive() {
			return false, nil
		}
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), question)
		answer, err := promptLine("Apply? [y/N]")
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
			return true, nil
		}
		return false, nil
	}
}

// linkedEnvFile resolves the env file the link owns, as an absolute path and
// relative to the repository root (the form the env scanners report).
func linkedEnvFile(cwd string, link config.ProjectConfig) (absolute, relative string) {
	return envTargetPath(cwd, link.AppPath, link.EnvFile), filepath.Clean(envIgnoreEntry(link.AppPath, link.EnvFile))
}

// missingLinkedEnvVars lists the link's database variables absent from its
// env file.
func missingLinkedEnvVars(cwd string, link config.ProjectConfig) ([]string, error) {
	absolute, _ := linkedEnvFile(cwd, link)
	values, err := envfile.Values(absolute)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, name := range []string{link.DatabaseURLVar, link.DirectURLVar} {
		if strings.TrimSpace(name) != "" && values[name] == "" {
			missing = append(missing, name)
		}
	}
	return missing, nil
}

func (a *app) runDoctorFixes(cmd *cobra.Command, confirm fixConfirmer) ([]doctorFix, error) {
	fixes := []doctorFix{}
	link, linkErr := config.LoadProjectConfig(a.cwd)
	linked := linkErr == nil && strings.TrimSpace(link.ProjectID) != ""
	if linkErr != nil && !errors.Is(linkErr, os.ErrNotExist) {
		return nil, linkErr
	}

	// 1. Stale link / missing env vars: both need the project, so one lookup.
	if linked {
		fix, stale := a.fixLinkAndEnv(cmd, link, confirm)
		fixes = append(fixes, fix...)
		if stale {
			linked = false
		}
	}

	// 2. Env shadowing: other env files pointing the same keys elsewhere.
	fixes = append(fixes, a.fixEnvShadowing(link, linked, confirm)...)

	// 3. Configuration the linter flags and that has a mechanical fix.
	findings, err := configlint.Run(a.cwd)
	if err != nil {
		fixes = append(fixes, doctorFix{Name: "db_config", Status: fixSkipped, Detail: err.Error()})
	} else {
		fixes = append(fixes, a.fixConfigFindings(findings)...)
	}
	return fixes, nil
}

func (a *app) fixLinkAndEnv(cmd *cobra.Command, link config.ProjectConfig, confirm fixConfirmer) ([]doctorFix, bool) {
	ctx := cmd.Context()
	authConfig, err := a.resolveAuth(false, link.APIURL)
	if err != nil {
		return []doctorFix{{Name: "auth", Status: fixManual, Detail: "run `capydb login` (needed to check the link and write env vars)"}}, false
	}
	client, err := a.newAPIClient(a.resolveAPIURL(link.APIURL), authConfig.APIKey)
	if err != nil {
		return []doctorFix{{Name: "project_link", Status: fixSkipped, Detail: err.Error()}}, false
	}
	if _, _, err := client.GetProject(ctx, link.ProjectID); err != nil {
		if apiErr, ok := errors.AsType[*api.APIError](err); !ok || apiErr.StatusCode != 404 {
			return []doctorFix{{Name: "project_link", Status: fixSkipped, Detail: "could not check the linked project: " + err.Error()}}, false
		}
		path := config.ProjectConfigPath(a.cwd)
		question := fmt.Sprintf("The linked project %s no longer exists. Remove the stale link %s?", link.ProjectID, path)
		ok, err := confirm(question)
		switch {
		case err != nil:
			return []doctorFix{{Name: "project_link", Status: fixSkipped, Detail: err.Error()}}, true
		case !ok:
			return []doctorFix{{Name: "project_link", Status: fixSkipped, Detail: "stale link to " + link.ProjectID + " kept; re-run with --yes or run `capydb unlink`, then `capydb link`"}}, true
		}
		if err := os.Remove(path); err != nil {
			return []doctorFix{{Name: "project_link", Status: fixSkipped, Detail: err.Error()}}, true
		}
		return []doctorFix{{Name: "project_link", Status: fixApplied, Detail: "removed the stale link to " + link.ProjectID + "; run `capydb link` to link a project"}}, true
	}

	missing, err := missingLinkedEnvVars(a.cwd, link)
	if err != nil {
		return []doctorFix{{Name: "env_vars", Status: fixSkipped, Detail: err.Error()}}, false
	}
	if len(missing) == 0 {
		return nil, false
	}
	connections, err := client.GetProjectConnection(ctx, link.ProjectID)
	if err != nil {
		return []doctorFix{{Name: "env_vars", Status: fixSkipped, Detail: "fetch connection strings: " + err.Error()}}, false
	}
	plan := project.BuildEnvPlan(projectDetectionFromConfig(link), connections.DirectURL, connections.PooledURL)
	updates := map[string]string{}
	for _, name := range missing {
		if value := plan.Vars[name]; value != "" {
			updates[name] = value
		}
	}
	if len(updates) == 0 {
		return []doctorFix{{Name: "env_vars", Status: fixManual, Detail: "missing " + strings.Join(missing, ", ") + "; run `capydb env pull`"}}, false
	}
	absolute, relative := linkedEnvFile(a.cwd, link)
	keep := func(string, string, string) (bool, error) { return false, nil }
	if err := envfile.UpsertWithResolver(absolute, updates, keep); err != nil {
		return []doctorFix{{Name: "env_vars", Status: fixSkipped, Detail: err.Error()}}, false
	}
	if err := gitignore.EnsureLocalConfigIgnored(a.cwd, relative); err != nil {
		return []doctorFix{{Name: "env_vars", Status: fixSkipped, Detail: err.Error()}}, false
	}
	names := make([]string, 0, len(updates))
	for name := range updates {
		names = append(names, name)
	}
	sort.Strings(names)
	return []doctorFix{{Name: "env_vars", Status: fixApplied, Detail: fmt.Sprintf("added %s to %s", strings.Join(names, ", "), relative)}}, false
}

func (a *app) fixEnvShadowing(link config.ProjectConfig, linked bool, confirm fixConfirmer) []doctorFix {
	conflicts, err := scan.DetectEnvConflicts(a.cwd)
	if err != nil || len(conflicts) == 0 {
		return nil
	}
	if !linked {
		return []doctorFix{{Name: "env_shadowing", Status: fixManual, Detail: "env keys point at different databases, and without a project link there is no file to keep; remove the database vars from all but one env file"}}
	}
	_, owner := linkedEnvFile(a.cwd, link)
	// file -> keys to remove from it
	remove := map[string][]string{}
	for _, conflict := range conflicts {
		for _, assignment := range conflict.Assignments {
			if filepath.Clean(assignment.File) != owner {
				remove[assignment.File] = append(remove[assignment.File], conflict.Key)
			}
		}
	}
	files := make([]string, 0, len(remove))
	for file := range remove {
		files = append(files, file)
	}
	sort.Strings(files)

	var fixes []doctorFix
	for _, file := range files {
		keys := remove[file]
		sort.Strings(keys)
		question := fmt.Sprintf("%s sets %s to a different database than %s, which the project link owns. Remove %s from %s?", file, strings.Join(keys, ", "), owner, strings.Join(keys, ", "), file)
		ok, err := confirm(question)
		switch {
		case err != nil:
			fixes = append(fixes, doctorFix{Name: "env_shadowing", Status: fixSkipped, Detail: err.Error()})
			continue
		case !ok:
			fixes = append(fixes, doctorFix{Name: "env_shadowing", Status: fixSkipped, Detail: fmt.Sprintf("kept %s in %s; re-run with --yes or remove them by hand", strings.Join(keys, ", "), file)})
			continue
		}
		if _, err := envfile.RemoveKeys(filepath.Join(a.cwd, file), keys); err != nil {
			fixes = append(fixes, doctorFix{Name: "env_shadowing", Status: fixSkipped, Detail: err.Error()})
			continue
		}
		fixes = append(fixes, doctorFix{Name: "env_shadowing", Status: fixApplied, Detail: fmt.Sprintf("removed %s from %s (%s owns them)", strings.Join(keys, ", "), file, owner)})
	}
	return fixes
}

var (
	drizzleDialectLine  = regexp.MustCompile(`(?m)^([ \t]*)dialect\s*:[^\n]*\n`)
	prismaDatasourceURL = regexp.MustCompile(`(?m)^([ \t]*)url\s*=[^\n]*\n`)
)

// fixConfigFindings applies the additive fixes for linter findings. Each
// fix re-reads the file and changes it only when the finding's condition
// still holds, so running --fix twice changes nothing the second time.
func (a *app) fixConfigFindings(findings []configlint.Finding) []doctorFix {
	var fixes []doctorFix
	for _, finding := range findings {
		path := filepath.Join(a.cwd, finding.File)
		var apply func(string) (string, string)
		switch finding.Rule {
		case "missing_schema_filter":
			apply = func(content string) (string, string) {
				if strings.Contains(content, "schemaFilter") {
					return content, ""
				}
				match := drizzleDialectLine.FindStringSubmatchIndex(content)
				if match == nil {
					return content, ""
				}
				indent := content[match[2]:match[3]]
				return content[:match[1]] + indent + `schemaFilter: ["public"],` + "\n" + content[match[1]:], `added schemaFilter: ["public"]`
			}
		case "pooled_url_for_migrations":
			apply = func(content string) (string, string) {
				var report codemodReport
				rewritten := codemodDrizzleConfig(path, content, &report)
				if rewritten == content {
					return content, ""
				}
				return rewritten, "drizzle-kit credentials now prefer DATABASE_DIRECT_URL"
			}
		case "prisma_missing_direct_url":
			apply = func(content string) (string, string) {
				if strings.Contains(content, "directUrl") {
					return content, ""
				}
				match := prismaDatasourceURL.FindStringSubmatchIndex(content)
				if match == nil {
					return content, ""
				}
				indent := content[match[2]:match[3]]
				return content[:match[1]] + indent + `directUrl = env("DIRECT_URL")` + "\n" + content[match[1]:], `added directUrl = env("DIRECT_URL") (written by capydb link / env pull)`
			}
		default:
			detail := fmt.Sprintf("%s [%s] %s", finding.File, finding.Rule, finding.Message)
			if finding.Fix != "" {
				detail += " -> " + finding.Fix
			}
			fixes = append(fixes, doctorFix{Name: "db_config", Status: fixManual, Detail: detail})
			continue
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			fixes = append(fixes, doctorFix{Name: "db_config", Status: fixSkipped, Detail: err.Error()})
			continue
		}
		rewritten, description := apply(string(raw))
		if description == "" {
			fixes = append(fixes, doctorFix{Name: "db_config", Status: fixManual, Detail: fmt.Sprintf("%s [%s] could not be rewritten mechanically -> %s", finding.File, finding.Rule, finding.Fix)})
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			fixes = append(fixes, doctorFix{Name: "db_config", Status: fixSkipped, Detail: err.Error()})
			continue
		}
		if err := os.WriteFile(path, []byte(rewritten), info.Mode().Perm()); err != nil {
			fixes = append(fixes, doctorFix{Name: "db_config", Status: fixSkipped, Detail: err.Error()})
			continue
		}
		fixes = append(fixes, doctorFix{Name: "db_config", Status: fixApplied, Detail: finding.File + ": " + description})
	}
	return fixes
}
