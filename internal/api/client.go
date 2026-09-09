package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/capydatabase/capydbclient"
)

// APIError is a non-2xx control-plane response. It is the shared transport
// error type; callers match it with errors.As/AsType as before.
type APIError = capydbclient.APIError

type Client struct {
	doer *capydbclient.Doer
}

// CreateProjectRequest, and the other shared control-plane entities/request
// bodies below, come from the shared capydbclient module (single source of truth
// mirrored from the OpenAPI spec) rather than being re-declared here.
type CreateProjectRequest = capydbclient.CreateProjectRequest

type Organization = capydbclient.Organization

type Viewer = capydbclient.Viewer

// The control-plane entities and request bodies below are aliases of the shared
// capydbclient module (the single Go mirror of the OpenAPI component schemas),
// so the CLI and the Terraform provider cannot drift from each other or from
// the API. Names follow the schema names; a few keep a shorter CLI-facing
// alias where the command output already uses that word.
type (
	ActiveQuery                        = capydbclient.ActiveQuerySample
	Backup                             = capydbclient.Backup
	CLILoginSessionStart               = capydbclient.CLILoginSessionStartResponse
	CLILoginSessionStartRequest        = capydbclient.CLILoginSessionStartRequest
	CLILoginSessionStatus              = capydbclient.CLILoginSessionPollResponse
	CreateImportRequest                = capydbclient.CreateImportRequest
	CreateRestorePointRequest          = capydbclient.CreateRestorePointRequest
	CreateRestoreRequest               = capydbclient.CreateRestoreRequest
	ExportDownload                     = capydbclient.ExportDownload
	ImportPreflight                    = capydbclient.ImportPreflightResult
	ImportPreflightCheck               = capydbclient.ImportPreflightCheck
	ImportPreflightExtension           = capydbclient.SourceExtension
	ImportPreflightForeignKey          = capydbclient.SourceForeignKey
	ImportPreflightSource              = capydbclient.SourceInspection
	ImportUpload                       = capydbclient.ImportUpload
	IndexAdvisorReport                 = capydbclient.IndexAdvisorReport
	IndexHygieneReport                 = capydbclient.IndexHygieneReport
	IndexSuggestion                    = capydbclient.IndexSuggestion
	KVCredentials                      = capydbclient.KVCredentials
	KVStore                            = capydbclient.KVStore
	Preview                            = capydbclient.PreviewDatabase
	ProjectAlert                       = capydbclient.ProjectAlert
	ProjectAuditEvent                  = capydbclient.ProjectAuditEvent
	ProjectExport                      = capydbclient.ProjectExport
	ProjectExtension                   = capydbclient.ProjectExtensionStatus
	ProjectIntegration                 = capydbclient.ProjectIntegration
	ProjectLogEntry                    = capydbclient.ProjectLogEntry
	ProjectLogs                        = capydbclient.ProjectLogs
	ProjectObservability               = capydbclient.ProjectObservability
	ProvisionCloudflareDatabaseRequest = capydbclient.ProvisionCloudflareDatabaseRequest
	ProvisionCloudflareDatabaseResult  = capydbclient.ProvisionCloudflareDatabaseResponse
	PublicStatusComponent              = capydbclient.StatusComponent
	PublicStatusResponse               = capydbclient.StatusResponse
	RestorePoint                       = capydbclient.RestorePoint
	SQLResult                          = capydbclient.SQLQueryResult
	ScheduledBackup                    = capydbclient.ScheduledBackup
	RedundantIndex                     = capydbclient.RedundantIndex
	SlowQuery                          = capydbclient.SlowQuerySample
	UnusedIndex                        = capydbclient.UnusedIndex
	UpsertScheduledBackupRequest       = capydbclient.UpsertScheduledBackupRequest
)

type CreatePreviewRequest = capydbclient.CreatePreviewRequest

type Job = capydbclient.Job

type Project = capydbclient.Project

type DatabaseSchema = capydbclient.DatabaseSchema

type SchemaNamespace = capydbclient.SchemaNamespace

type SchemaTable = capydbclient.SchemaTable

type SchemaColumn = capydbclient.SchemaColumn

type SchemaEnum = capydbclient.SchemaEnum

type SchemaExtension = capydbclient.SchemaExtension

type SchemaForeignKey = capydbclient.SchemaForeignKey

type SchemaUniqueConstraint = capydbclient.SchemaUniqueConstraint

type GeneratedTypes = capydbclient.GeneratedTypes

type ConnectionInfo = capydbclient.ConnectionInfo

type ProjectConnectionInfo = ConnectionInfo

type PreviewConnectionInfo = ConnectionInfo

type PreviewDetails struct {
	Connections PreviewConnectionInfo `json:"connections"`
	Preview     Preview               `json:"preview"`
}

// APIKey is an organization (or project-scoped) API key. Plaintext secrets
// are never returned by list endpoints; only the prefix is shown.
type APIKey = capydbclient.APIKey

type CreateAPIKeyRequest = capydbclient.CreateAPIKeyRequest

type WebhookEndpoint = capydbclient.WebhookEndpoint

type CreateWebhookEndpointRequest = capydbclient.CreateWebhookEndpointRequest

type WebhookDelivery = capydbclient.WebhookDelivery

const defaultHTTPTimeout = 30 * time.Second

// defaultUploadTimeout bounds dump uploads, which can far outlast the normal
// API timeout but must not hang forever.
const defaultUploadTimeout = 30 * time.Minute

// resolveHTTPTimeout returns the HTTP client timeout, honouring the
// CAPYDB_HTTP_TIMEOUT env var (a Go duration string such as "45s" or "2m").
func resolveHTTPTimeout() (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv("CAPYDB_HTTP_TIMEOUT"))
	if raw == "" {
		return defaultHTTPTimeout, nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid CAPYDB_HTTP_TIMEOUT %q: %w (use a Go duration such as 30s or 2m)", raw, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("invalid CAPYDB_HTTP_TIMEOUT %q: duration must be positive", raw)
	}
	return parsed, nil
}

// resolveUploadTimeout returns the overall dump-upload deadline. It defaults
// to defaultUploadTimeout and honours an explicit CAPYDB_HTTP_TIMEOUT override.
func resolveUploadTimeout() (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv("CAPYDB_HTTP_TIMEOUT"))
	if raw == "" {
		return defaultUploadTimeout, nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid CAPYDB_HTTP_TIMEOUT %q: %w (use a Go duration such as 30s or 2m)", raw, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("invalid CAPYDB_HTTP_TIMEOUT %q: duration must be positive", raw)
	}
	return parsed, nil
}

// NewClient builds an API client. The version is the CLI build version and is
// used for the User-Agent header; the long "1.2.3 (commit: …)" form is reduced
// to its leading token.
func NewClient(baseURL, apiKey, version string) (*Client, error) {
	timeout, err := resolveHTTPTimeout()
	if err != nil {
		return nil, err
	}

	versionToken := "dev"
	if fields := strings.Fields(strings.TrimSpace(version)); len(fields) > 0 {
		versionToken = fields[0]
	}

	return &Client{
		doer: &capydbclient.Doer{
			APIKey:  strings.TrimSpace(apiKey),
			BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
			HTTPClient: &http.Client{
				Timeout: timeout,
			},
			UserAgent:    "capydb-cli/" + versionToken,
			RetryBackoff: []time.Duration{0, time.Second, 3 * time.Second},
		},
	}, nil
}

func (c *Client) GetPublicStatus(ctx context.Context) (PublicStatusResponse, error) {
	var response PublicStatusResponse
	if err := c.do(ctx, http.MethodGet, "/status", nil, &response); err != nil {
		return PublicStatusResponse{}, err
	}
	return response, nil
}

func (c *Client) CreateProject(ctx context.Context, request CreateProjectRequest) (Project, Job, error) {
	var response struct {
		Job     Job     `json:"job"`
		Project Project `json:"project"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/projects", request, &response); err != nil {
		return Project{}, Job{}, err
	}
	return response.Project, response.Job, nil
}

func (c *Client) GetViewer(ctx context.Context) (Viewer, error) {
	var response Viewer
	if err := c.do(ctx, http.MethodGet, "/v1/me", nil, &response); err != nil {
		return Viewer{}, err
	}
	return response, nil
}

func (c *Client) StartCLILoginSession(ctx context.Context, request CLILoginSessionStartRequest) (CLILoginSessionStart, error) {
	var response CLILoginSessionStart
	if err := c.do(ctx, http.MethodPost, "/v1/cli/login/sessions", request, &response); err != nil {
		return CLILoginSessionStart{}, err
	}
	return response, nil
}

func (c *Client) PollCLILoginSession(ctx context.Context, sessionID, pollToken string) (CLILoginSessionStatus, error) {
	var response CLILoginSessionStatus
	path := "/v1/cli/login/sessions/" + strings.TrimSpace(sessionID)
	// The poll token is the sole secret gating minted-key delivery; it travels
	// in a header so it never lands in server/proxy access logs.
	if err := c.doWithHeader(ctx, http.MethodGet, path, nil, &response, "X-CapyDB-Poll-Token", strings.TrimSpace(pollToken)); err != nil {
		return CLILoginSessionStatus{}, err
	}
	return response, nil
}

func (c *Client) CreateBackup(ctx context.Context, projectID, label string) (Job, error) {
	var response struct {
		Job Job `json:"job"`
	}
	payload := map[string]string{}
	if strings.TrimSpace(label) != "" {
		payload["label"] = strings.TrimSpace(label)
	}
	var body any
	if len(payload) > 0 {
		body = payload
	}
	if err := c.do(ctx, http.MethodPost, "/v1/projects/"+projectID+"/backups", body, &response); err != nil {
		return Job{}, err
	}
	return response.Job, nil
}

func (c *Client) CreateImport(ctx context.Context, projectID string, request CreateImportRequest) (Job, error) {
	request.SourceURL = strings.TrimSpace(request.SourceURL)
	request.UploadKey = strings.TrimSpace(request.UploadKey)

	var response struct {
		Job Job `json:"job"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/projects/"+projectID+"/imports", request, &response); err != nil {
		return Job{}, err
	}
	return response.Job, nil
}

// StartImportFollow begins a near-zero-downtime follow import from a live
// source. The API requires an explicit confirm (the follower writes into the
// project's live database from the start); the command gates before calling.
func (c *Client) StartImportFollow(ctx context.Context, projectID, sourceURL string) (Job, error) {
	request := struct {
		Confirm   bool   `json:"confirm"`
		SourceURL string `json:"source_url"`
	}{Confirm: true, SourceURL: strings.TrimSpace(sourceURL)}
	var response struct {
		Job Job `json:"job"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/projects/"+projectID+"/imports/follow", request, &response); err != nil {
		return Job{}, err
	}
	return response.Job, nil
}

// ImportFollowStatus enqueues a follow-import status read; poll the returned job
// for the status JSON in its result.
func (c *Client) ImportFollowStatus(ctx context.Context, projectID string) (Job, error) {
	var response struct {
		Job Job `json:"job"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/projects/"+projectID+"/imports/follow/status", nil, &response); err != nil {
		return Job{}, err
	}
	return response.Job, nil
}

// UpdateProjectEnvironment flips a project's environment label between
// production and non_production - the dial that gates overwrite-restore.
func (c *Client) UpdateProjectEnvironment(ctx context.Context, projectID, environment string) (Project, error) {
	var response struct {
		Project Project `json:"project"`
	}
	body := map[string]string{"environment": environment}
	if err := c.do(ctx, http.MethodPatch, "/v1/projects/"+projectID, body, &response); err != nil {
		return Project{}, err
	}
	return response.Project, nil
}

// ImportFollowCutover finalizes a follow import (drains the stream, transfers
// ownership, drops the source slot, flips the project live). The API requires
// an explicit confirm: cutover replaces the project's live database.
func (c *Client) ImportFollowCutover(ctx context.Context, projectID string) (Job, error) {
	var response struct {
		Job Job `json:"job"`
	}
	body := map[string]bool{"confirm": true}
	if err := c.do(ctx, http.MethodPost, "/v1/projects/"+projectID+"/imports/follow/cutover", body, &response); err != nil {
		return Job{}, err
	}
	return response.Job, nil
}

// ImportFollowAbort cancels an in-progress follow import.
func (c *Client) ImportFollowAbort(ctx context.Context, projectID string) (Job, error) {
	var response struct {
		Job Job `json:"job"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/projects/"+projectID+"/imports/follow/abort", nil, &response); err != nil {
		return Job{}, err
	}
	return response.Job, nil
}

// CreateImportUpload requests a presigned upload URL for a dump file destined
// for an import into the project.
func (c *Client) CreateImportUpload(ctx context.Context, projectID string) (ImportUpload, error) {
	var response struct {
		Upload ImportUpload `json:"upload"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/projects/"+projectID+"/imports/uploads", nil, &response); err != nil {
		return ImportUpload{}, err
	}
	return response.Upload, nil
}

// UploadDump streams a local dump file to a presigned PUT URL. The presigned
// URL embeds its own authorization, so no API key header is sent. progress,
// when non-nil, is invoked as bytes are read from the file (sent, total).
// Uploads can far outlast the normal API timeout, so the overall deadline is
// the upload timeout (default 30m, CAPYDB_HTTP_TIMEOUT overrides it) applied
// via the context - cancelling ctx aborts the upload immediately.
func (c *Client) UploadDump(ctx context.Context, uploadURL, filePath string, progress func(sent, total int64)) error {
	timeout, err := resolveUploadTimeout()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open dump file: %w", err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat dump file: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("dump path %s is a directory", filePath)
	}

	body := io.Reader(file)
	if progress != nil {
		body = &progressReader{reader: file, total: info.Size(), progress: progress}
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, body)
	if err != nil {
		return fmt.Errorf("build upload request: %w", err)
	}
	request.ContentLength = info.Size()
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("User-Agent", c.doer.UserAgent)

	uploadClient := &http.Client{}
	response, err := uploadClient.Do(request)
	if err != nil {
		return fmt.Errorf("upload dump: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		detail := strings.TrimSpace(string(raw))
		if detail != "" {
			return fmt.Errorf("upload dump: storage returned status %d: %s", response.StatusCode, detail)
		}
		return fmt.Errorf("upload dump: storage returned status %d", response.StatusCode)
	}
	return nil
}

// progressReader reports cumulative bytes read to a callback so callers can
// render upload progress.
type progressReader struct {
	progress func(sent, total int64)
	reader   io.Reader
	sent     int64
	total    int64
}

func (r *progressReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.sent += int64(n)
		r.progress(r.sent, r.total)
	}
	return n, err
}

// CreatePreviewDatabase queues a preview database and returns the created
// preview plus the async job. Connection details are intentionally not fetched
// here - callers decide whether a connection-fetch failure is fatal.
func (c *Client) CreatePreviewDatabase(ctx context.Context, projectID string, request CreatePreviewRequest) (Preview, Job, error) {
	var response struct {
		Job     Job     `json:"job"`
		Preview Preview `json:"preview"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/projects/"+projectID+"/preview-databases", request, &response); err != nil {
		return Preview{}, Job{}, err
	}
	return response.Preview, response.Job, nil
}

// MintProjectApproval mints a single-use approval token for one destructive
// project action ("project.delete" or "project.restore_overwrite"). The raw
// token is returned exactly once; it expires ~10 minutes after mint.
func (c *Client) MintProjectApproval(ctx context.Context, projectID, action string) (capydbclient.ProjectApproval, error) {
	var response struct {
		Approval capydbclient.ProjectApproval `json:"approval"`
	}
	payload := map[string]string{"action": action}
	if err := c.do(ctx, http.MethodPost, "/v1/projects/"+projectID+"/approvals", payload, &response); err != nil {
		return capydbclient.ProjectApproval{}, err
	}
	return response.Approval, nil
}

func (c *Client) CreateRestore(ctx context.Context, projectID string, request CreateRestoreRequest) (Job, error) {
	var response struct {
		Job Job `json:"job"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/projects/"+projectID+"/restores", request, &response); err != nil {
		return Job{}, err
	}
	return response.Job, nil
}

func (c *Client) DeletePreviewDatabase(ctx context.Context, previewID string) (Job, error) {
	var response struct {
		Job Job `json:"job"`
	}
	if err := c.do(ctx, http.MethodDelete, "/v1/preview-databases/"+previewID, nil, &response); err != nil {
		return Job{}, err
	}
	return response.Job, nil
}

func (c *Client) GetJob(ctx context.Context, jobID string) (Job, error) {
	var response struct {
		Job Job `json:"job"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/jobs/"+jobID, nil, &response); err != nil {
		return Job{}, err
	}
	return response.Job, nil
}

// GetJobResult fetches a job's structured result payload. The API returns a
// result only for tenant-visible job types (currently
// project.import_follow_status: unit_active, result, lag_bytes, sentinel);
// for everything else this is nil. Decoded locally until capydbclient's Job
// carries the field.
func (c *Client) GetJobResult(ctx context.Context, jobID string) (json.RawMessage, error) {
	var response struct {
		Job struct {
			Result json.RawMessage `json:"result"`
		} `json:"job"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/jobs/"+jobID, nil, &response); err != nil {
		return nil, err
	}
	return response.Job.Result, nil
}

// ListProjectJobs returns the project's recent lifecycle jobs, newest first.
func (c *Client) ListProjectJobs(ctx context.Context, projectID string, limit int) ([]Job, error) {
	path := "/v1/projects/" + projectID + "/jobs"
	if limit > 0 {
		values := url.Values{}
		values.Set("limit", strconv.Itoa(limit))
		path += "?" + values.Encode()
	}

	var response struct {
		Jobs []Job `json:"jobs"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	return response.Jobs, nil
}

// ProjectLogsQuery parameterizes one log fetch. Cursor (tail mode) takes
// precedence over Hours (window mode).
type ProjectLogsQuery struct {
	Cursor     string
	Hours      int
	Limit      int
	Severities []string
}

// GetProjectLogs fetches one window (or tail increment) of the project's
// database logs.
func (c *Client) GetProjectLogs(ctx context.Context, projectID string, query ProjectLogsQuery) (ProjectLogs, error) {
	values := url.Values{}
	if query.Cursor != "" {
		values.Set("cursor", query.Cursor)
	} else if query.Hours > 0 {
		values.Set("hours", strconv.Itoa(query.Hours))
	}
	if query.Limit > 0 {
		values.Set("limit", strconv.Itoa(query.Limit))
	}
	if len(query.Severities) > 0 {
		values.Set("severity", strings.Join(query.Severities, ","))
	}
	path := "/v1/projects/" + projectID + "/logs"
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}

	var response struct {
		Logs ProjectLogs `json:"logs"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &response); err != nil {
		return ProjectLogs{}, err
	}
	return response.Logs, nil
}

func (c *Client) GetProject(ctx context.Context, projectID string) (Project, ProjectConnectionInfo, error) {
	var response struct {
		Project Project `json:"project"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/projects/"+projectID, nil, &response); err != nil {
		return Project{}, ProjectConnectionInfo{}, err
	}
	return response.Project, ProjectConnectionInfo{}, nil
}

func (c *Client) GetProjectConnection(ctx context.Context, projectID string) (ProjectConnectionInfo, error) {
	var response struct {
		Connections ProjectConnectionInfo `json:"connections"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/projects/"+projectID+"/connections", nil, &response); err != nil {
		return ProjectConnectionInfo{}, err
	}
	return response.Connections, nil
}

func (c *Client) ListProjectIntegrations(ctx context.Context, projectID string) ([]ProjectIntegration, error) {
	var response struct {
		Integrations []ProjectIntegration `json:"integrations"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/projects/"+projectID+"/integrations", nil, &response); err != nil {
		return nil, err
	}
	return capydbclient.NormalizeList(response.Integrations), nil
}

// ProvisionCloudflareDatabase creates a Cloudflare-billed database. It is the
// one call that carries no CapyDB credential: the Cloudflare signature is the
// authentication, verified by the control plane.
func (c *Client) ProvisionCloudflareDatabase(ctx context.Context, request ProvisionCloudflareDatabaseRequest) (ProvisionCloudflareDatabaseResult, error) {
	var result ProvisionCloudflareDatabaseResult
	if err := c.do(ctx, http.MethodPost, "/v1/integrations/cloudflare/databases", request, &result); err != nil {
		return ProvisionCloudflareDatabaseResult{}, err
	}
	return result, nil
}

func (c *Client) GetProjectObservability(ctx context.Context, projectID string) (ProjectObservability, error) {
	var response struct {
		Observability ProjectObservability `json:"observability"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/projects/"+projectID+"/observability", nil, &response); err != nil {
		return ProjectObservability{}, err
	}
	return response.Observability, nil
}

// RunSQL executes a read-mostly SQL query against the project database via the
// control plane. maxRows of 0 leaves the server default in place.
//
// allowUnqualifiedWrites opts out of the control plane's destructive-statement
// guard, which otherwise refuses an UPDATE or DELETE with no top-level WHERE
// and any TRUNCATE. It is sent only when true so an older control plane (which
// does not know the field) keeps behaving as it does today.
//
// readOnly asks the server to run the statement inside a READ ONLY
// transaction, so Postgres itself refuses any write (SQLSTATE 25006) - an
// executor-proven guarantee rather than a client-side check. Also sent only
// when true, for the same older-control-plane reason.
func (c *Client) RunSQL(ctx context.Context, projectID, query string, maxRows int, allowUnqualifiedWrites, readOnly bool) (SQLResult, error) {
	payload := map[string]any{
		"query": query,
	}
	if maxRows > 0 {
		payload["max_rows"] = maxRows
	}
	if allowUnqualifiedWrites {
		payload["allow_unqualified_writes"] = true
	}
	if readOnly {
		payload["read_only"] = true
	}

	var response struct {
		Result SQLResult `json:"result"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/projects/"+projectID+"/sql", payload, &response); err != nil {
		return SQLResult{}, err
	}
	return response.Result, nil
}

// ImportPreflight checks a source database against the project before an
// import is queued.
func (c *Client) ImportPreflight(ctx context.Context, projectID, sourceURL string) (ImportPreflight, error) {
	var response struct {
		Preflight ImportPreflight `json:"preflight"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/projects/"+projectID+"/imports/preflight", map[string]string{
		"source_url": strings.TrimSpace(sourceURL),
	}, &response); err != nil {
		return ImportPreflight{}, err
	}
	return response.Preflight, nil
}

func (c *Client) ListBackups(ctx context.Context, projectID string) ([]Backup, error) {
	var response struct {
		Backups []Backup `json:"backups"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/projects/"+projectID+"/backups", nil, &response); err != nil {
		return nil, err
	}
	return response.Backups, nil
}

func (c *Client) CreateExport(ctx context.Context, projectID string) (string, Job, error) {
	var response struct {
		ExportID string `json:"export_id"`
		Job      Job    `json:"job"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/projects/"+projectID+"/exports", nil, &response); err != nil {
		return "", Job{}, err
	}
	return response.ExportID, response.Job, nil
}

func (c *Client) ListExports(ctx context.Context, projectID string) ([]ProjectExport, error) {
	var response struct {
		Exports []ProjectExport `json:"exports"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/projects/"+projectID+"/exports", nil, &response); err != nil {
		return nil, err
	}
	return response.Exports, nil
}

func (c *Client) GetExportDownload(ctx context.Context, projectID, exportID string) (ExportDownload, error) {
	var response struct {
		Download ExportDownload `json:"download"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/projects/"+projectID+"/exports/"+exportID+"/download", nil, &response); err != nil {
		return ExportDownload{}, err
	}
	return response.Download, nil
}

// DownloadExport streams a presigned GET URL to a local file. The presigned
// URL embeds its own authorization, so no API key header is sent. progress,
// when non-nil, is invoked as bytes arrive (received, total; total is 0 when
// the server sends no Content-Length). Refuses to overwrite an existing file.
// Downloads share the upload timeout knob (default 30m, CAPYDB_HTTP_TIMEOUT
// overrides it), applied via the context.
func (c *Client) DownloadExport(ctx context.Context, downloadURL, destPath string, progress func(received, total int64)) error {
	timeout, err := resolveUploadTimeout()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	file, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create export file: %w", err)
	}
	defer func() { _ = file.Close() }()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return fmt.Errorf("build download request: %w", err)
	}
	request.Header.Set("User-Agent", c.doer.UserAgent)

	downloadClient := &http.Client{}
	response, err := downloadClient.Do(request)
	if err != nil {
		return fmt.Errorf("download export: %w", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		detail := strings.TrimSpace(string(raw))
		if detail != "" {
			return fmt.Errorf("download export: storage returned status %d: %s", response.StatusCode, detail)
		}
		return fmt.Errorf("download export: storage returned status %d", response.StatusCode)
	}

	body := io.Reader(response.Body)
	if progress != nil {
		body = &progressReader{reader: response.Body, total: max(response.ContentLength, 0), progress: progress}
	}
	if _, err := io.Copy(file, body); err != nil {
		return fmt.Errorf("write export file: %w", err)
	}
	return nil
}

func (c *Client) ListScheduledBackups(ctx context.Context, projectID string) ([]ScheduledBackup, error) {
	var response struct {
		ScheduledBackups []ScheduledBackup `json:"scheduled_backups"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/projects/"+projectID+"/scheduled-backups", nil, &response); err != nil {
		return nil, err
	}
	return response.ScheduledBackups, nil
}

// UpsertScheduledBackup creates or replaces the project's default backup
// schedule and returns the stored schedule.
func (c *Client) UpsertScheduledBackup(ctx context.Context, projectID string, request UpsertScheduledBackupRequest) (ScheduledBackup, error) {
	var response struct {
		ScheduledBackup ScheduledBackup `json:"scheduled_backup"`
	}
	if err := c.do(ctx, http.MethodPut, "/v1/projects/"+projectID+"/scheduled-backups/default", request, &response); err != nil {
		return ScheduledBackup{}, err
	}
	return response.ScheduledBackup, nil
}

// RotateCredentials queues a rotation of the project's database credential.
// With graceHours 0 the previous connection strings stop working once the job
// completes. With graceHours > 0 a new database username is issued and the
// outgoing credential keeps authenticating until the window ends (max 720).
func (c *Client) RotateCredentials(ctx context.Context, projectID string, graceHours int) (Job, error) {
	var response struct {
		Job Job `json:"job"`
	}
	var body any
	if graceHours > 0 {
		body = map[string]int{"grace_hours": graceHours}
	}
	if err := c.do(ctx, http.MethodPost, "/v1/projects/"+projectID+"/credentials/rotate", body, &response); err != nil {
		return Job{}, err
	}
	return response.Job, nil
}

func (c *Client) ListRegions(ctx context.Context) ([]string, error) {
	var response struct {
		Regions []string `json:"regions"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/regions", nil, &response); err != nil {
		return nil, err
	}
	return response.Regions, nil
}

func (c *Client) ListPreviewDatabases(ctx context.Context, projectID string) ([]PreviewDetails, error) {
	var response struct {
		PreviewDatabases []Preview `json:"preview_databases"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/projects/"+projectID+"/preview-databases", nil, &response); err != nil {
		return nil, err
	}

	details := make([]PreviewDetails, 0, len(response.PreviewDatabases))
	for _, preview := range response.PreviewDatabases {
		details = append(details, PreviewDetails{Preview: preview})
	}
	return details, nil
}

func (c *Client) GetPreviewConnection(ctx context.Context, previewID string) (PreviewConnectionInfo, error) {
	var response struct {
		Connections PreviewConnectionInfo `json:"connections"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/preview-databases/"+previewID+"/connections", nil, &response); err != nil {
		return PreviewConnectionInfo{}, err
	}
	return response.Connections, nil
}

func (c *Client) ListProjects(ctx context.Context, organizationID string) ([]Project, error) {
	path := "/v1/projects"
	if trimmed := strings.TrimSpace(organizationID); trimmed != "" {
		values := url.Values{}
		values.Set("organization_id", trimmed)
		path += "?" + values.Encode()
	}

	var response struct {
		Projects []Project `json:"projects"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	return response.Projects, nil
}

func (c *Client) ExtendPreviewDatabase(ctx context.Context, previewID string, ttlHours int) (Preview, error) {
	var response struct {
		Preview Preview `json:"preview"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/preview-databases/"+previewID+"/extend", map[string]int{
		"ttl_hours": ttlHours,
	}, &response); err != nil {
		return Preview{}, err
	}
	return response.Preview, nil
}

func (c *Client) ResetPreviewDatabase(ctx context.Context, previewID string) (Job, error) {
	var response struct {
		Job Job `json:"job"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/preview-databases/"+previewID+"/reset", nil, &response); err != nil {
		return Job{}, err
	}
	return response.Job, nil
}

func (c *Client) ListRestorePoints(ctx context.Context, projectID string) ([]RestorePoint, error) {
	var response struct {
		RestorePoints []RestorePoint `json:"restore_points"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/projects/"+projectID+"/restore-points", nil, &response); err != nil {
		return nil, err
	}
	return response.RestorePoints, nil
}

func (c *Client) CreateRestorePoint(ctx context.Context, projectID string, request CreateRestorePointRequest) (RestorePoint, error) {
	var response struct {
		RestorePoint RestorePoint `json:"restore_point"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/projects/"+projectID+"/restore-points", request, &response); err != nil {
		return RestorePoint{}, err
	}
	return response.RestorePoint, nil
}

func (c *Client) DeleteRestorePoint(ctx context.Context, projectID, restorePointID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/projects/"+projectID+"/restore-points/"+restorePointID, nil, nil)
}

func (c *Client) GetOrganization(ctx context.Context, orgID string) (Organization, error) {
	var response struct {
		Organization Organization `json:"organization"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/organizations/"+orgID, nil, &response); err != nil {
		return Organization{}, err
	}
	return response.Organization, nil
}

func (c *Client) ListAPIKeys(ctx context.Context, orgID string) ([]APIKey, error) {
	var response struct {
		APIKeys []APIKey `json:"api_keys"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/organizations/"+orgID+"/api-keys", nil, &response); err != nil {
		return nil, err
	}
	return response.APIKeys, nil
}

// CreateAPIKey creates an org-wide or project-scoped key. The plaintext key is
// returned exactly once.
func (c *Client) CreateAPIKey(ctx context.Context, orgID string, request CreateAPIKeyRequest) (APIKey, string, error) {
	var response struct {
		APIKey       APIKey `json:"api_key"`
		PlaintextKey string `json:"plaintext_api_key"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/organizations/"+orgID+"/api-keys", request, &response); err != nil {
		return APIKey{}, "", err
	}
	return response.APIKey, response.PlaintextKey, nil
}

func (c *Client) RevokeAPIKey(ctx context.Context, orgID, keyID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/organizations/"+orgID+"/api-keys/"+keyID, nil, nil)
}

func (c *Client) ListWebhookEndpoints(ctx context.Context, orgID string) ([]WebhookEndpoint, error) {
	var response struct {
		WebhookEndpoints []WebhookEndpoint `json:"webhook_endpoints"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/organizations/"+orgID+"/webhook-endpoints", nil, &response); err != nil {
		return nil, err
	}
	return response.WebhookEndpoints, nil
}

// CreateWebhookEndpoint registers an outbound webhook receiver. The signing
// secret is returned exactly once.
func (c *Client) CreateWebhookEndpoint(ctx context.Context, orgID string, request CreateWebhookEndpointRequest) (WebhookEndpoint, string, error) {
	var response struct {
		WebhookEndpoint WebhookEndpoint `json:"webhook_endpoint"`
		PlaintextSecret string          `json:"plaintext_secret"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/organizations/"+orgID+"/webhook-endpoints", request, &response); err != nil {
		return WebhookEndpoint{}, "", err
	}
	return response.WebhookEndpoint, response.PlaintextSecret, nil
}

func (c *Client) DeleteWebhookEndpoint(ctx context.Context, orgID, endpointID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/organizations/"+orgID+"/webhook-endpoints/"+endpointID, nil, nil)
}

// RotateWebhookEndpointSecret generates a new signing secret and returns it
// exactly once.
func (c *Client) RotateWebhookEndpointSecret(ctx context.Context, orgID, endpointID string) (WebhookEndpoint, string, error) {
	var response struct {
		WebhookEndpoint WebhookEndpoint `json:"webhook_endpoint"`
		PlaintextSecret string          `json:"plaintext_secret"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/organizations/"+orgID+"/webhook-endpoints/"+endpointID+"/rotate-secret", nil, &response); err != nil {
		return WebhookEndpoint{}, "", err
	}
	return response.WebhookEndpoint, response.PlaintextSecret, nil
}

func (c *Client) ListWebhookDeliveries(ctx context.Context, orgID, endpointID string, limit int) ([]WebhookDelivery, error) {
	path := "/v1/organizations/" + orgID + "/webhook-endpoints/" + endpointID + "/deliveries"
	if limit > 0 {
		values := url.Values{}
		values.Set("limit", strconv.Itoa(limit))
		path += "?" + values.Encode()
	}

	var response struct {
		Deliveries []WebhookDelivery `json:"deliveries"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	return response.Deliveries, nil
}

// SendTestWebhookEvent enqueues one synthetic webhook.test event to the
// endpoint so the receiver and its signature verification can be exercised
// end-to-end. The test event is delivered with a single attempt (no retries);
// the outcome shows up in the delivery listing.
func (c *Client) SendTestWebhookEvent(ctx context.Context, orgID, endpointID string) (WebhookDelivery, error) {
	var response struct {
		Delivery WebhookDelivery `json:"delivery"`
	}
	path := "/v1/organizations/" + orgID + "/webhook-endpoints/" + endpointID + "/test"
	if err := c.do(ctx, http.MethodPost, path, nil, &response); err != nil {
		return WebhookDelivery{}, err
	}
	return response.Delivery, nil
}

// RedeliverWebhookDelivery re-enqueues an existing delivery's payload as a
// fresh pending delivery to the same endpoint (same event envelope, new
// delivery id, attempts reset). The original delivery is kept as the
// historical record.
func (c *Client) RedeliverWebhookDelivery(ctx context.Context, orgID, endpointID, deliveryID string) (WebhookDelivery, error) {
	var response struct {
		Delivery WebhookDelivery `json:"delivery"`
	}
	path := "/v1/organizations/" + orgID + "/webhook-endpoints/" + endpointID + "/deliveries/" + deliveryID + "/redeliver"
	if err := c.do(ctx, http.MethodPost, path, nil, &response); err != nil {
		return WebhookDelivery{}, err
	}
	return response.Delivery, nil
}

func (c *Client) ListProjectAuditEvents(ctx context.Context, projectID string, limit int) ([]ProjectAuditEvent, error) {
	path := "/v1/projects/" + projectID + "/audit-events"
	if limit > 0 {
		values := url.Values{}
		values.Set("limit", strconv.Itoa(limit))
		path += "?" + values.Encode()
	}

	var response struct {
		AuditEvents []ProjectAuditEvent `json:"audit_events"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	return response.AuditEvents, nil
}

func (c *Client) ListProjectExtensions(ctx context.Context, projectID string) ([]ProjectExtension, error) {
	var response struct {
		Extensions []ProjectExtension `json:"extensions"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/projects/"+projectID+"/extensions", nil, &response); err != nil {
		return nil, err
	}
	return response.Extensions, nil
}

// GetProjectIndexAdvisor returns index suggestions derived from the predicates
// the project's queries actually ran. Read-only: candidates are costed as
// hypothetical indexes, so nothing is created on the database.
func (c *Client) GetProjectIndexAdvisor(ctx context.Context, projectID string, minFilter, minSelectivity int) (IndexAdvisorReport, error) {
	query := url.Values{}
	if minFilter > 0 {
		query.Set("min_filter", strconv.Itoa(minFilter))
	}
	if minSelectivity > 0 {
		query.Set("min_selectivity", strconv.Itoa(minSelectivity))
	}
	path := "/v1/projects/" + projectID + "/advisor/indexes"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}

	var response struct {
		Advisor IndexAdvisorReport `json:"advisor"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &response); err != nil {
		return IndexAdvisorReport{}, err
	}
	return response.Advisor, nil
}

// GetProjectIndexHygiene returns the indexes the database is paying for
// without using: never scanned, or covered by a wider index. Read-only, and it
// needs no extension - unlike the suggestion side.
func (c *Client) GetProjectIndexHygiene(ctx context.Context, projectID string) (IndexHygieneReport, error) {
	var response struct {
		Hygiene IndexHygieneReport `json:"hygiene"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/projects/"+projectID+"/advisor/index-hygiene", nil, &response); err != nil {
		return IndexHygieneReport{}, err
	}
	response.Hygiene.UnusedIndexes = capydbclient.NormalizeList(response.Hygiene.UnusedIndexes)
	response.Hygiene.RedundantIndexes = capydbclient.NormalizeList(response.Hygiene.RedundantIndexes)
	return response.Hygiene, nil
}

// EnableProjectExtension queues enabling a Postgres extension and returns the
// async job.
func (c *Client) EnableProjectExtension(ctx context.Context, projectID, name string) (Job, error) {
	var response struct {
		Job Job `json:"job"`
	}
	payload := map[string]string{"name": strings.TrimSpace(name)}
	if err := c.do(ctx, http.MethodPost, "/v1/projects/"+projectID+"/extensions", payload, &response); err != nil {
		return Job{}, err
	}
	return response.Job, nil
}

// DisableProjectExtension queues dropping a Postgres extension and returns the
// async job.
// UpdateProjectExtension bumps an already-enabled extension to the version the
// platform provides. Customer-initiated by design: an extension's upgrade
// scripts can change behaviour inside the customer's data. No-op when already
// current, so retries are safe.
func (c *Client) UpdateProjectExtension(ctx context.Context, projectID, name string) (Job, error) {
	var response struct {
		Job Job `json:"job"`
	}
	path := "/v1/projects/" + projectID + "/extensions/" + url.PathEscape(strings.TrimSpace(name)) + "/update"
	if err := c.do(ctx, http.MethodPost, path, nil, &response); err != nil {
		return Job{}, err
	}
	return response.Job, nil
}

// UpgradeProjectMinor restarts the project's database onto the PostgreSQL minor
// already installed on the platform. Minors are binary-compatible, so this is a
// restart rather than a migration.
func (c *Client) UpgradeProjectMinor(ctx context.Context, projectID string) (Job, error) {
	var response struct {
		Job Job `json:"job"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/projects/"+projectID+"/upgrade/minor", nil, &response); err != nil {
		return Job{}, err
	}
	return response.Job, nil
}

// MajorUpgradePreflight enqueues the read-only check of whether the project can
// move to targetMajor. The verdict lands in the job result.
func (c *Client) MajorUpgradePreflight(ctx context.Context, projectID string, targetMajor int) (Job, error) {
	var response struct {
		Job Job `json:"job"`
	}
	path := fmt.Sprintf("/v1/projects/%s/upgrade/major/preflight?target_major=%d", projectID, targetMajor)
	if err := c.do(ctx, http.MethodPost, path, nil, &response); err != nil {
		return Job{}, err
	}
	return response.Job, nil
}

func (c *Client) DisableProjectExtension(ctx context.Context, projectID, name string) (Job, error) {
	var response struct {
		Job Job `json:"job"`
	}
	if err := c.do(ctx, http.MethodDelete, "/v1/projects/"+projectID+"/extensions/"+url.PathEscape(strings.TrimSpace(name)), nil, &response); err != nil {
		return Job{}, err
	}
	return response.Job, nil
}

func (c *Client) ListProjectAlerts(ctx context.Context, projectID string) ([]ProjectAlert, error) {
	var response struct {
		Alerts []ProjectAlert `json:"alerts"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/projects/"+projectID+"/alerts", nil, &response); err != nil {
		return nil, err
	}
	return response.Alerts, nil
}

func (c *Client) AcknowledgeProjectAlert(ctx context.Context, projectID, alertID string) error {
	return c.do(ctx, http.MethodPost, "/v1/projects/"+projectID+"/alerts/"+alertID+"/acknowledge", nil, nil)
}

// GetProjectSchema fetches the canonical schema document for the project's
// database.
func (c *Client) GetProjectSchema(ctx context.Context, projectID string) (DatabaseSchema, error) {
	var response struct {
		Schema DatabaseSchema `json:"schema"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/projects/"+projectID+"/schema", nil, &response); err != nil {
		return DatabaseSchema{}, err
	}
	return response.Schema, nil
}

// GetPreviewSchema fetches the canonical schema document for a preview
// database.
func (c *Client) GetPreviewSchema(ctx context.Context, previewID string) (DatabaseSchema, error) {
	var response struct {
		Schema DatabaseSchema `json:"schema"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/preview-databases/"+previewID+"/schema", nil, &response); err != nil {
		return DatabaseSchema{}, err
	}
	return response.Schema, nil
}

// GenerateProjectSchemaTypes generates source code (typescript, zod or
// drizzle) from the project database's live schema. style applies to
// typescript output only.
func (c *Client) GenerateProjectSchemaTypes(ctx context.Context, projectID, language, style string) (GeneratedTypes, error) {
	return c.generateSchemaTypes(ctx, "/v1/projects/"+projectID+"/schema/types", language, style)
}

// GeneratePreviewSchemaTypes is GenerateProjectSchemaTypes against a preview
// database.
func (c *Client) GeneratePreviewSchemaTypes(ctx context.Context, previewID, language, style string) (GeneratedTypes, error) {
	return c.generateSchemaTypes(ctx, "/v1/preview-databases/"+previewID+"/schema/types", language, style)
}

func (c *Client) generateSchemaTypes(ctx context.Context, basePath, language, style string) (GeneratedTypes, error) {
	query := url.Values{}
	if strings.TrimSpace(language) != "" {
		query.Set("language", strings.TrimSpace(language))
	}
	if strings.TrimSpace(style) != "" {
		query.Set("style", strings.TrimSpace(style))
	}
	path := basePath
	if len(query) > 0 {
		path += "?" + query.Encode()
	}

	var response struct {
		Types GeneratedTypes `json:"types"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &response); err != nil {
		return GeneratedTypes{}, err
	}
	return response.Types, nil
}

// do executes an API request via the shared transport. Idempotent GET requests
// are retried on network errors and 5xx responses following the client's retry
// backoff (default three attempts with 0s/1s/3s delays); non-GET requests are
// never retried.
func (c *Client) do(ctx context.Context, method, path string, payload any, dest any) error {
	return c.doer.Do(ctx, method, path, payload, dest)
}

// doWithHeader is do with one extra request header - used for secrets that must
// not appear in the URL (e.g. the device-login poll token).
func (c *Client) doWithHeader(ctx context.Context, method, path string, payload any, dest any, headerKey, headerValue string) error {
	return c.doer.Do(ctx, method, path, payload, dest, capydbclient.Header{Key: headerKey, Value: headerValue})
}

// ── K/V stores ───────────────────────────────────────────────────────────────

// ListKVStores returns every K/V store in the organization. The control plane
// requires an explicit organization for this endpoint - an admin principal has
// no implicit one - so organizationID is passed through when set.
func (c *Client) ListKVStores(ctx context.Context, organizationID string) ([]KVStore, error) {
	path := "/v1/kv"
	if trimmed := strings.TrimSpace(organizationID); trimmed != "" {
		path += "?organization_id=" + url.QueryEscape(trimmed)
	}
	var response struct {
		KVStores []KVStore `json:"kv_stores"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &response); err != nil {
		return nil, err
	}
	return response.KVStores, nil
}

// GetKVStore returns the project's store. A 404 means the project has no store
// yet, which callers distinguish with capydbclient.IsNotFound.
func (c *Client) GetKVStore(ctx context.Context, projectID string) (KVStore, error) {
	var store KVStore
	if err := c.do(ctx, http.MethodGet, "/v1/projects/"+projectID+"/kv", nil, &store); err != nil {
		return KVStore{}, err
	}
	return store, nil
}

// CreateKVStore provisions the project's store. The returned KVStore carries
// the plaintext token exactly once - only its hash is stored - so the caller
// must surface it before returning.
func (c *Client) CreateKVStore(ctx context.Context, projectID string) (KVStore, Job, error) {
	var response struct {
		Job     Job     `json:"job"`
		KVStore KVStore `json:"kv_store"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/projects/"+projectID+"/kv", map[string]any{}, &response); err != nil {
		return KVStore{}, Job{}, err
	}
	return response.KVStore, response.Job, nil
}

func (c *Client) DeleteKVStore(ctx context.Context, projectID string) (Job, error) {
	var response struct {
		Job Job `json:"job"`
	}
	if err := c.do(ctx, http.MethodDelete, "/v1/projects/"+projectID+"/kv", nil, &response); err != nil {
		return Job{}, err
	}
	return response.Job, nil
}

// GetKVCredentials returns the endpoints without the secret: the token cannot
// be read back, so RestToken is empty and TokenRequired is true.
func (c *Client) GetKVCredentials(ctx context.Context, projectID string) (KVCredentials, error) {
	var credentials KVCredentials
	if err := c.do(ctx, http.MethodGet, "/v1/projects/"+projectID+"/kv/credentials", nil, &credentials); err != nil {
		return KVCredentials{}, err
	}
	return credentials, nil
}

// RotateKVToken mints a new token and stops the old one immediately - there is
// no grace window. This call is synchronous: the response, not a job, is where
// the new plaintext lives.
func (c *Client) RotateKVToken(ctx context.Context, projectID string) (KVStore, error) {
	var response struct {
		KVStore KVStore `json:"kv_store"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/projects/"+projectID+"/kv/rotate-token", nil, &response); err != nil {
		return KVStore{}, err
	}
	return response.KVStore, nil
}

// FlushKVStore empties the keyspace. The store survives with the same endpoint
// and token; the data does not, and there is no backup to restore from.
func (c *Client) FlushKVStore(ctx context.Context, projectID string) (Job, error) {
	var response struct {
		Job Job `json:"job"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/projects/"+projectID+"/kv/flush", nil, &response); err != nil {
		return Job{}, err
	}
	return response.Job, nil
}
