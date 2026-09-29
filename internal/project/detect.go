package project

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Detection struct {
	AppPath       string
	DatabaseLayer string
	EnvFile       string
	Framework     string
	Profile       string
	ProjectName   string
}

type EnvPlan struct {
	DatabaseURLVar string
	DirectURLVar   string
	PooledURLVar   string
	Vars           map[string]string
}

type AmbiguousProjectError struct {
	Candidates []Detection
}

func (e *AmbiguousProjectError) Error() string {
	if len(e.Candidates) == 0 {
		return "multiple app candidates found"
	}

	labels := make([]string, 0, len(e.Candidates))
	for _, candidate := range e.Candidates {
		label := candidate.AppPath
		if label == "" {
			label = "."
		}
		labels = append(labels, label)
	}
	return fmt.Sprintf("multiple app candidates found: %s", strings.Join(labels, ", "))
}

const (
	DatabaseLayerActiveRecord = "active_record"
	DatabaseLayerDrizzle      = "drizzle"
	DatabaseLayerGeneric      = "generic"
	DatabaseLayerPG           = "pg"
	DatabaseLayerPGX          = "pgx"
	DatabaseLayerPrisma       = "prisma"
	DatabaseLayerSQLAlchemy   = "sqlalchemy"
)

const (
	FrameworkDjango    = "django"
	FrameworkGeneric   = "generic"
	FrameworkGo        = "go"
	FrameworkNextJS    = "nextjs"
	FrameworkNode      = "node"
	FrameworkPython    = "python"
	FrameworkRails     = "rails"
	FrameworkRemix     = "remix"
	FrameworkSvelteKit = "sveltekit"
)

type packageMetadata struct {
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	Workspaces      any               `json:"workspaces"`
}

func Detect(cwd, envFileOverride string) (Detection, error) {
	current := inspectProjectDir(cwd, ".", envFileOverride)
	if current.recognized() {
		return current.toDetection(), nil
	}

	candidates, err := FindCandidates(cwd, envFileOverride)
	if err != nil {
		return Detection{}, err
	}
	switch len(candidates) {
	case 0:
		return current.toDetection(), nil
	case 1:
		return candidates[0], nil
	default:
		return Detection{}, &AmbiguousProjectError{Candidates: candidates}
	}
}

func FindCandidates(cwd, envFileOverride string) ([]Detection, error) {
	var candidates []candidate
	err := filepath.WalkDir(cwd, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}

		relative, err := filepath.Rel(cwd, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}

		if shouldSkipDir(entry.Name()) {
			return filepath.SkipDir
		}
		if dirDepth(relative) > 3 {
			return filepath.SkipDir
		}

		inspection := inspectProjectDir(cwd, relative, envFileOverride)
		if !inspection.recognized() {
			return nil
		}

		candidates = append(candidates, inspection)
		if inspection.Framework != FrameworkGeneric || inspection.DatabaseLayer != DatabaseLayerGeneric {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(candidates, func(i, j int) bool {
		left := candidates[i].AppPath
		right := candidates[j].AppPath
		if left == right {
			return candidates[i].Profile < candidates[j].Profile
		}
		return left < right
	})

	detections := make([]Detection, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		key := candidate.AppPath + "|" + candidate.Profile
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		detections = append(detections, candidate.toDetection())
	}
	return detections, nil
}

func BuildEnvPlan(detection Detection, directURL, pooledURL string) EnvPlan {
	plan := EnvPlan{
		DatabaseURLVar: "DATABASE_URL",
		DirectURLVar:   "DATABASE_DIRECT_URL",
		PooledURLVar:   "DATABASE_POOL_URL",
		Vars:           make(map[string]string),
	}

	switch detection.DatabaseLayer {
	case DatabaseLayerPrisma:
		plan.Vars["DATABASE_URL"] = firstNonEmpty(pooledURL, directURL)
		if directURL != "" {
			plan.DirectURLVar = "DIRECT_URL"
			plan.Vars["DIRECT_URL"] = directURL
			plan.Vars["DATABASE_DIRECT_URL"] = directURL
		}
		if pooledURL != "" {
			plan.Vars["DATABASE_POOL_URL"] = pooledURL
		}
	case DatabaseLayerDrizzle, DatabaseLayerPG:
		plan.Vars["DATABASE_URL"] = firstNonEmpty(pooledURL, directURL)
		if directURL != "" {
			plan.Vars["DATABASE_DIRECT_URL"] = directURL
		}
		if pooledURL != "" {
			plan.Vars["DATABASE_POOL_URL"] = pooledURL
		}
	case DatabaseLayerPGX, DatabaseLayerSQLAlchemy, DatabaseLayerActiveRecord:
		plan.Vars["DATABASE_URL"] = firstNonEmpty(directURL, pooledURL)
		if directURL != "" {
			plan.Vars["DATABASE_DIRECT_URL"] = directURL
		}
		if pooledURL != "" {
			plan.Vars["DATABASE_POOL_URL"] = pooledURL
		}
	default:
		switch detection.Framework {
		case FrameworkNextJS, FrameworkSvelteKit, FrameworkRemix, FrameworkNode:
			plan.Vars["DATABASE_URL"] = firstNonEmpty(pooledURL, directURL)
		default:
			plan.Vars["DATABASE_URL"] = firstNonEmpty(directURL, pooledURL)
		}
		if directURL != "" {
			plan.Vars["DATABASE_DIRECT_URL"] = directURL
		}
		if pooledURL != "" {
			plan.Vars["DATABASE_POOL_URL"] = pooledURL
		}
	}

	// DIRECT_URL is the name ORM docs and other providers use for the
	// unpooled connection (Prisma's directUrl, the Neon/Supabase guides), so
	// it is written for every stack next to CapyDB's own DATABASE_DIRECT_URL.
	// DATABASE_URL keeps its per-stack default above.
	if directURL != "" {
		plan.Vars["DIRECT_URL"] = directURL
	}

	return plan
}

func BuildNextSteps(detection Detection) []string {
	switch detection.DatabaseLayer {
	case DatabaseLayerPrisma:
		return []string{
			"Prisma detected: keep DATABASE_URL for runtime and DIRECT_URL for schema.prisma migrations.",
			"Typical next step: npx prisma migrate dev",
		}
	case DatabaseLayerDrizzle:
		return []string{
			"Drizzle detected: use DATABASE_URL for app traffic and DIRECT_URL (same value as DATABASE_DIRECT_URL) for drizzle-kit migrations.",
			"Typical next step: drizzle-kit push or your usual migration command.",
		}
	case DatabaseLayerPG:
		return []string{
			"Node Postgres detected: use DATABASE_URL in server-side code only.",
		}
	case DatabaseLayerPGX:
		return []string{
			"pgx detected: DATABASE_URL points at the direct connection by default for Go services.",
		}
	case DatabaseLayerSQLAlchemy:
		return []string{
			"SQLAlchemy detected: DATABASE_URL points at the direct connection by default.",
			"Typical next step: run your Alembic migration command.",
		}
	case DatabaseLayerActiveRecord:
		return []string{
			"Rails/Active Record detected: DATABASE_URL points at the direct connection by default.",
			"Typical next step: bin/rails db:migrate",
		}
	default:
		switch detection.Framework {
		case FrameworkDjango:
			return []string{
				"Django detected: DATABASE_URL points at the direct connection by default.",
				"Typical next step: python manage.py migrate",
			}
		case FrameworkNextJS, FrameworkSvelteKit, FrameworkRemix:
			return []string{
				"Web app framework detected: DATABASE_URL points at the pooled connection by default.",
			}
		default:
			return nil
		}
	}
}

type candidate struct {
	AppPath       string
	DatabaseLayer string
	EnvFile       string
	Framework     string
	HasProject    bool
	Profile       string
	ProjectName   string
}

func (c candidate) recognized() bool {
	return c.HasProject && (c.Framework != FrameworkGeneric || c.DatabaseLayer != DatabaseLayerGeneric || c.AppPath == ".")
}

func (c candidate) toDetection() Detection {
	return Detection{
		AppPath:       c.AppPath,
		DatabaseLayer: c.DatabaseLayer,
		EnvFile:       c.EnvFile,
		Framework:     c.Framework,
		Profile:       c.Profile,
		ProjectName:   c.ProjectName,
	}
}

func inspectProjectDir(root, relativePath, envFileOverride string) candidate {
	fullPath := filepath.Join(root, relativePath)
	projectName := filepath.Base(fullPath)
	if projectName == "." || projectName == string(filepath.Separator) || strings.TrimSpace(projectName) == "" {
		projectName = filepath.Base(root)
	}

	meta, hasPackage := readPackageMetadata(fullPath)
	hasMonorepoPackage := hasPackage && meta.Workspaces != nil && !hasAppDependencies(meta)

	framework, hasProject := detectFramework(fullPath, meta, hasPackage)
	if hasMonorepoPackage && framework == FrameworkNode {
		hasProject = false
		framework = FrameworkGeneric
	}

	databaseLayer := detectDatabaseLayer(fullPath, framework, meta, hasPackage)
	if databaseLayer != DatabaseLayerGeneric {
		hasProject = true
	}

	envFile := chooseEnvFile(fullPath, framework, envFileOverride)
	profile := buildProfile(framework, databaseLayer)

	return candidate{
		AppPath:       cleanAppPath(relativePath),
		DatabaseLayer: databaseLayer,
		EnvFile:       envFile,
		Framework:     framework,
		HasProject:    hasProject,
		Profile:       profile,
		ProjectName:   projectName,
	}
}

func buildProfile(framework, databaseLayer string) string {
	if framework == "" {
		framework = FrameworkGeneric
	}
	if databaseLayer == "" || databaseLayer == DatabaseLayerGeneric {
		return framework
	}
	return framework + "+" + databaseLayer
}

func chooseEnvFile(projectDir, framework, override string) string {
	if trimmed := strings.TrimSpace(override); trimmed != "" {
		return trimmed
	}

	candidates := []string{".env", ".env.local"}
	switch framework {
	case FrameworkNextJS, FrameworkSvelteKit:
		candidates = []string{".env.local", ".env"}
	}

	for _, candidate := range candidates {
		if _, err := os.Stat(filepath.Join(projectDir, candidate)); err == nil {
			return candidate
		}
	}
	return candidates[0]
}

func detectDatabaseLayer(projectDir, framework string, meta packageMetadata, hasPackage bool) string {
	if fileExists(filepath.Join(projectDir, "schema.prisma")) || fileExists(filepath.Join(projectDir, "prisma", "schema.prisma")) {
		return DatabaseLayerPrisma
	}

	for _, name := range []string{"drizzle.config.ts", "drizzle.config.js", "drizzle.config.mjs", "drizzle.config.cjs"} {
		if fileExists(filepath.Join(projectDir, name)) {
			return DatabaseLayerDrizzle
		}
	}

	if framework == FrameworkRails {
		return DatabaseLayerActiveRecord
	}
	if hasPackage {
		switch {
		case packageHasDependency(meta, "drizzle-orm"):
			return DatabaseLayerDrizzle
		case packageHasDependency(meta, "@prisma/client") || packageHasDependency(meta, "prisma"):
			return DatabaseLayerPrisma
		case packageHasDependency(meta, "pg"):
			return DatabaseLayerPG
		}
	}
	if framework == FrameworkGo && fileContainsAny(filepath.Join(projectDir, "go.mod"), "github.com/jackc/pgx", "github.com/jackc/pgx/v5") {
		return DatabaseLayerPGX
	}
	if framework == FrameworkPython || framework == FrameworkDjango {
		if fileContainsAny(filepath.Join(projectDir, "pyproject.toml"), "sqlalchemy", "SQLAlchemy") ||
			fileContainsAny(filepath.Join(projectDir, "requirements.txt"), "sqlalchemy", "SQLAlchemy") {
			return DatabaseLayerSQLAlchemy
		}
	}
	return DatabaseLayerGeneric
}

func detectFramework(projectDir string, meta packageMetadata, hasPackage bool) (string, bool) {
	if hasPackage {
		switch {
		case packageHasDependency(meta, "next"):
			return FrameworkNextJS, true
		case packageHasDependency(meta, "@sveltejs/kit"):
			return FrameworkSvelteKit, true
		case packageHasDependency(meta, "@remix-run/react") || packageHasDependency(meta, "@remix-run/dev"):
			return FrameworkRemix, true
		default:
			return FrameworkNode, true
		}
	}

	switch {
	case fileExists(filepath.Join(projectDir, "manage.py")) ||
		fileContainsAny(filepath.Join(projectDir, "requirements.txt"), "django", "Django") ||
		fileContainsAny(filepath.Join(projectDir, "pyproject.toml"), "django", "Django"):
		return FrameworkDjango, true
	case fileExists(filepath.Join(projectDir, "Gemfile")) &&
		(fileContainsAny(filepath.Join(projectDir, "Gemfile"), "gem \"rails\"", "gem 'rails'") ||
			fileExists(filepath.Join(projectDir, "config", "application.rb"))):
		return FrameworkRails, true
	case fileExists(filepath.Join(projectDir, "go.mod")):
		return FrameworkGo, true
	case fileExists(filepath.Join(projectDir, "pyproject.toml")) || fileExists(filepath.Join(projectDir, "requirements.txt")):
		return FrameworkPython, true
	default:
		return FrameworkGeneric, false
	}
}

func readPackageMetadata(projectDir string) (packageMetadata, bool) {
	path := filepath.Join(projectDir, "package.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return packageMetadata{}, false
	}

	var meta packageMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return packageMetadata{}, false
	}
	return meta, true
}

func packageHasDependency(meta packageMetadata, name string) bool {
	if _, ok := meta.Dependencies[name]; ok {
		return true
	}
	if _, ok := meta.DevDependencies[name]; ok {
		return true
	}
	return false
}

func hasAppDependencies(meta packageMetadata) bool {
	for _, name := range []string{"next", "@sveltejs/kit", "@remix-run/react", "@remix-run/dev", "@prisma/client", "drizzle-orm", "pg"} {
		if packageHasDependency(meta, name) {
			return true
		}
	}
	return false
}

func shouldSkipDir(name string) bool {
	switch name {
	case ".git", ".next", ".turbo", "build", "coverage", "dist", "node_modules", "vendor":
		return true
	default:
		return strings.HasPrefix(name, ".")
	}
}

func dirDepth(relativePath string) int {
	if relativePath == "." || relativePath == "" {
		return 0
	}
	return len(strings.Split(relativePath, string(filepath.Separator)))
}

func cleanAppPath(relativePath string) string {
	if relativePath == "." {
		return ""
	}
	return relativePath
}

func fileContainsAny(path string, patterns ...string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	text := string(data)
	for _, pattern := range patterns {
		if strings.Contains(text, pattern) {
			return true
		}
	}
	return false
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
