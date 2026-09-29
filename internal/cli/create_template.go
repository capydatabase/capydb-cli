package cli

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/capydatabase/capydb-cli/internal/config"
)

//go:embed templates
var starterTemplateFiles embed.FS

// starterTemplate is a built-in schema plus seed applied to a new project.
type starterTemplate struct {
	name        string
	description string
	nextStep    string
}

// starterTemplates is the built-in set. "empty" applies nothing and exists
// so the choice can be stated explicitly (and is the default).
var starterTemplates = map[string]starterTemplate{
	"empty": {
		name:        "empty",
		description: "no tables (the default)",
	},
	"drizzle-starter": {
		name:        "drizzle-starter",
		description: "users and posts with a foreign key and sample rows",
		nextStep:    "run `capydb init drizzle` to scaffold drizzle.config.ts and a schema generated from these tables",
	},
	"auth-starter": {
		name:        "auth-starter",
		description: "users, OAuth accounts, sessions and verification tokens, with one demo user",
		nextStep:    "point your auth library at these tables; tokens are stored hashed (token_hash)",
	},
}

func starterTemplateNames() []string {
	names := make([]string, 0, len(starterTemplates))
	for name := range starterTemplates {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// script returns the template's schema followed by its seed.
func (t starterTemplate) script() (string, error) {
	if t.name == "empty" {
		return "", nil
	}
	var parts []string
	for _, file := range []string{"schema.sql", "seed.sql"} {
		content, err := starterTemplateFiles.ReadFile("templates/" + t.name + "/" + file)
		if err != nil {
			return "", fmt.Errorf("read template %s: %w", t.name, err)
		}
		parts = append(parts, string(content))
	}
	return strings.Join(parts, "\n"), nil
}

// withCreateTemplate adds --template to `capydb create`. It wraps the command
// instead of living inside it: the template is applied only after the
// project is created, linked and its env written, and the create flow itself
// stays unaware of templates.
func (a *app) withCreateTemplate(create *cobra.Command) *cobra.Command {
	var templateName string
	create.Flags().StringVar(&templateName, "template", "empty", "Starter template applied after the project is created: "+strings.Join(starterTemplateNames(), ", "))

	create.PreRunE = func(cmd *cobra.Command, args []string) error {
		if _, ok := starterTemplates[strings.TrimSpace(templateName)]; !ok {
			return usageErrorf("unknown --template %q; available: %s", templateName, strings.Join(starterTemplateNames(), ", "))
		}
		return nil
	}

	createRun := create.RunE
	create.RunE = func(cmd *cobra.Command, args []string) error {
		if err := createRun(cmd, args); err != nil {
			return err
		}
		template := starterTemplates[strings.TrimSpace(templateName)]
		if template.name == "empty" {
			return nil
		}
		return a.applyStarterTemplate(cmd, template)
	}
	return create
}

func (a *app) applyStarterTemplate(cmd *cobra.Command, template starterTemplate) error {
	ctx := cmd.Context()
	// create's own summary owns stdout in JSON mode.
	progress := cmd.OutOrStdout()
	if a.jsonOutput() {
		progress = cmd.ErrOrStderr()
	}

	script, err := template.script()
	if err != nil {
		return err
	}
	linkConfig, err := config.LoadProjectConfig(a.cwd)
	if err != nil {
		return fmt.Errorf("read the new project link: %w", err)
	}
	client, _, err := a.resolveClient(false, linkConfig.APIURL)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(progress, "Applying template %s (%s)\n", template.name, template.description)
	connections, err := client.GetProjectConnection(ctx, linkConfig.ProjectID)
	if err == nil && strings.TrimSpace(connections.DirectURL) == "" {
		err = fmt.Errorf("the project has no direct connection URL yet")
	}
	if err == nil {
		err = execSQLScript(ctx, connections.DirectURL, script)
	}
	if err != nil {
		// The script runs as one transaction, so the database is still empty.
		// Leave the SQL where `capydb seed` can apply it.
		fallback := filepath.Join(a.cwd, "capydb-template-"+template.name+".sql")
		if writeErr := os.WriteFile(fallback, []byte(script), 0o644); writeErr != nil {
			return fmt.Errorf("project %s was created and linked, but template %s failed: %w (saving the template SQL also failed: %v)", linkConfig.ProjectName, template.name, err, writeErr)
		}
		return fmt.Errorf("project %s was created and linked, but template %s failed and nothing was applied: %w; the template SQL is in %s - apply it with `capydb seed %s --confirm-production`", linkConfig.ProjectName, template.name, err, fallback, fallback)
	}

	_, _ = fmt.Fprintf(progress, "Template %s applied.\n", template.name)
	if template.nextStep != "" {
		_, _ = fmt.Fprintf(progress, "- Next: %s\n", template.nextStep)
	}
	return nil
}
