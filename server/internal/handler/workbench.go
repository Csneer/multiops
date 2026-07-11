package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	maxWorkbenchTextLength     = 16 * 1024
	workbenchPostCommitTimeout = 10 * time.Second
)

type CreateConnectorRequest struct {
	Key          string         `json:"key"`
	Name         string         `json:"name"`
	Type         string         `json:"type"`
	Capabilities map[string]any `json:"capabilities"`
	Config       map[string]any `json:"config"`
	Enabled      *bool          `json:"enabled"`
}

type IssueTemplateMatchRequest struct {
	SourceStatus *string        `json:"source_status"`
	LabelsAny    []string       `json:"labels_any"`
	Fields       map[string]any `json:"fields"`
}

type IssueTemplateOutputRequest struct {
	TitlePrefix       string  `json:"title_prefix"`
	DescriptionSource string  `json:"description_source"`
	Status            string  `json:"status"`
	Priority          string  `json:"priority"`
	AssigneeType      *string `json:"assignee_type"`
	AssigneeID        *string `json:"assignee_id"`
	AutoStart         *bool   `json:"auto_start"`
}

type CreateIssueTemplateRequest struct {
	ConnectorID string                     `json:"connector_id"`
	TemplateKey string                     `json:"template_key"`
	Name        string                     `json:"name"`
	Enabled     *bool                      `json:"enabled"`
	Priority    int32                      `json:"priority"`
	Match       IssueTemplateMatchRequest  `json:"match"`
	Output      IssueTemplateOutputRequest `json:"output"`
}

type IngestCreateIssueRequest struct {
	Description  *string `json:"description"`
	Status       string  `json:"status"`
	Priority     string  `json:"priority"`
	AssigneeType *string `json:"assignee_type"`
	AssigneeID   *string `json:"assignee_id"`
}

type IngestExternalRecordRequest struct {
	SourceType     string                    `json:"source_type"`
	ExternalID     string                    `json:"external_id"`
	ExternalKey    *string                   `json:"external_key"`
	Title          string                    `json:"title"`
	Summary        *string                   `json:"summary"`
	SourceStatus   *string                   `json:"source_status"`
	SourceURL      *string                   `json:"source_url"`
	SchemaVersion  *string                   `json:"schema_version"`
	IdempotencyKey string                    `json:"idempotency_key"`
	IssueID        *string                   `json:"issue_id"`
	CreateIssue    *IngestCreateIssueRequest `json:"create_issue"`
	ConnectorID    *string                   `json:"connector_id"`
	Labels         []string                  `json:"labels"`
	Fields         map[string]any            `json:"fields"`
	ObservedAt     *string                   `json:"observed_at"`
}

type ExternalRecordResponse struct {
	ID            string         `json:"id"`
	ConnectorID   *string        `json:"connector_id,omitempty"`
	SourceType    string         `json:"source_type"`
	ExternalID    string         `json:"external_id"`
	ExternalKey   *string        `json:"external_key,omitempty"`
	Title         string         `json:"title"`
	Summary       *string        `json:"summary,omitempty"`
	SourceStatus  *string        `json:"source_status,omitempty"`
	SourceURL     *string        `json:"source_url,omitempty"`
	SchemaVersion string         `json:"schema_version"`
	Labels        []string       `json:"labels"`
	Fields        map[string]any `json:"fields"`
	LastSeenAt    string         `json:"last_seen_at"`
}

type IngestExternalRecordResponse struct {
	ExternalRecord       ExternalRecordResponse `json:"external_record"`
	IssueID              *string                `json:"issue_id,omitempty"`
	ConnectorID          *string                `json:"connector_id,omitempty"`
	IssueTemplateID      *string                `json:"issue_template_id,omitempty"`
	IssueTemplateVersion *int32                 `json:"issue_template_version,omitempty"`
	Outcome              string                 `json:"outcome"`
	AttemptCount         int32                  `json:"attempt_count"`
}

type IssueExternalRecordBindingResponse struct {
	BindingID      string                 `json:"binding_id"`
	BindingRole    string                 `json:"binding_role"`
	BoundAt        string                 `json:"bound_at"`
	ExternalRecord ExternalRecordResponse `json:"external_record"`
}

func (h *Handler) CreateConnector(w http.ResponseWriter, r *http.Request) {
	var req CreateConnectorRequest
	if !decodeWorkbenchJSON(w, r, &req) {
		return
	}
	if err := validateRequiredWorkbenchText(map[string]string{"key": req.Key, "name": req.Name, "type": req.Type}); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if req.Capabilities == nil {
		req.Capabilities = map[string]any{}
	}
	if req.Config == nil {
		req.Config = map[string]any{}
	}
	capabilities, _ := json.Marshal(req.Capabilities)
	config, _ := json.Marshal(req.Config)
	creator, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace id")
	if !ok {
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	row, err := h.Queries.CreateConnectorInstance(r.Context(), db.CreateConnectorInstanceParams{WorkspaceID: workspaceID, Key: strings.TrimSpace(req.Key), Name: strings.TrimSpace(req.Name), ConnectorType: strings.TrimSpace(req.Type), Capabilities: capabilities, Config: config, Enabled: enabled, CreatedBy: parseUUID(creator)})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "connector key already exists")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to create connector")
		}
		return
	}
	writeJSON(w, http.StatusCreated, row)
}

func (h *Handler) CreateIssueTemplate(w http.ResponseWriter, r *http.Request) {
	var req CreateIssueTemplateRequest
	if !decodeWorkbenchJSON(w, r, &req) {
		return
	}
	if err := validateRequiredWorkbenchText(map[string]string{"connector_id": req.ConnectorID, "template_key": req.TemplateKey, "name": req.Name}); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	connectorID, ok := parseUUIDOrBadRequest(w, req.ConnectorID, "connector_id")
	if !ok {
		return
	}
	workspaceID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace id")
	if !ok {
		return
	}
	if _, err := h.Queries.GetEnabledConnectorInWorkspace(r.Context(), db.GetEnabledConnectorInWorkspaceParams{ID: connectorID, WorkspaceID: workspaceID}); err != nil {
		writeError(w, 400, "connector is not enabled in this workspace")
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	autoStart := true
	if req.Output.AutoStart != nil {
		autoStart = *req.Output.AutoStart
	}
	descriptionSource := req.Output.DescriptionSource
	if descriptionSource == "" {
		descriptionSource = "none"
	}
	if descriptionSource != "none" && descriptionSource != "summary" && descriptionSource != "title" {
		writeError(w, 400, "output.description_source is invalid")
		return
	}
	status := req.Output.Status
	if status == "" {
		status = "todo"
	}
	priority := req.Output.Priority
	if priority == "" {
		priority = "none"
	}
	if !validateIssueEnum(w, "output.status", status, validIssueStatuses) || !validateIssueEnum(w, "output.priority", priority, validIssuePriorities) {
		return
	}
	labels, err := normalizeLabels(req.Match.LabelsAny)
	if err != nil {
		writeError(w, 400, "match.labels_any "+err.Error())
		return
	}
	if err := validateScalarObject(req.Match.Fields); err != nil {
		writeError(w, 400, "match.fields "+err.Error())
		return
	}
	fields, _ := json.Marshal(defaultObject(req.Match.Fields))
	var assigneeType pgtype.Text
	var assigneeID pgtype.UUID
	if req.Output.AssigneeType != nil {
		assigneeType = pgtype.Text{String: strings.TrimSpace(*req.Output.AssigneeType), Valid: true}
	}
	if req.Output.AssigneeID != nil {
		id, ok := parseUUIDOrBadRequest(w, *req.Output.AssigneeID, "output.assignee_id")
		if !ok {
			return
		}
		assigneeID = id
	}
	if code, msg := h.validateAssigneePair(r.Context(), r, h.resolveWorkspaceID(r), assigneeType, assigneeID); code != 0 {
		writeError(w, code, msg)
		return
	}
	creator, ok := requireUserID(w, r)
	if !ok {
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "failed to start transaction")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	if _, err := qtx.LockEnabledConnectorForRouting(r.Context(), db.LockEnabledConnectorForRoutingParams{ID: connectorID, WorkspaceID: workspaceID}); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, "connector changed while creating template")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to lock connector")
		return
	}
	lockKey := strings.TrimSpace(req.TemplateKey)
	if err := qtx.LockConnectorTemplateKey(r.Context(), db.LockConnectorTemplateKeyParams{WorkspaceID: uuidToString(workspaceID), ConnectorID: uuidToString(connectorID), TemplateKey: pgtype.Text{String: lockKey, Valid: true}}); err != nil {
		writeError(w, 500, "failed to lock template version")
		return
	}
	version, err := qtx.GetNextIssueTemplateVersion(r.Context(), db.GetNextIssueTemplateVersionParams{WorkspaceID: workspaceID, ConnectorID: connectorID, TemplateKey: lockKey})
	if err != nil {
		writeError(w, 500, "failed to allocate template version")
		return
	}
	if enabled {
		if err := qtx.DisableEnabledIssueTemplate(r.Context(), db.DisableEnabledIssueTemplateParams{WorkspaceID: workspaceID, ConnectorID: connectorID, TemplateKey: lockKey}); err != nil {
			writeError(w, 500, "failed to disable prior template")
			return
		}
	}
	row, err := qtx.CreateIssueTemplate(r.Context(), db.CreateIssueTemplateParams{WorkspaceID: workspaceID, ConnectorID: connectorID, TemplateKey: lockKey, Version: version, Name: strings.TrimSpace(req.Name), Enabled: enabled, Priority: req.Priority, MatchSourceStatus: optionalText(req.Match.SourceStatus), MatchLabelsAny: labels, MatchFields: fields, TitlePrefix: req.Output.TitlePrefix, DescriptionSource: descriptionSource, Status: status, IssuePriority: priority, AssigneeType: assigneeType, AssigneeID: assigneeID, AutoStart: autoStart, CreatedBy: parseUUID(creator)})
	if err != nil {
		writeError(w, 500, "failed to create issue template")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, 500, "failed to commit issue template")
		return
	}
	writeJSON(w, http.StatusCreated, row)
}

func (h *Handler) IngestExternalRecord(w http.ResponseWriter, r *http.Request) {
	var req IngestExternalRecordRequest
	if !decodeWorkbenchJSON(w, r, &req) {
		return
	}
	if err := validateIngestExternalRecordRequest(req); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	workspaceID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace id")
	if !ok {
		return
	}
	labels, err := normalizeLabels(req.Labels)
	if err != nil {
		writeError(w, 400, "labels "+err.Error())
		return
	}
	if err := validateScalarObject(req.Fields); err != nil {
		writeError(w, 400, "fields "+err.Error())
		return
	}
	fieldsObject := defaultObject(req.Fields)
	fieldsJSON, _ := json.Marshal(fieldsObject)
	var connectorID pgtype.UUID
	var validatedTemplate db.IssueTemplate
	if req.ConnectorID != nil {
		connectorID, ok = parseUUIDOrBadRequest(w, *req.ConnectorID, "connector_id")
		if !ok {
			return
		}
		connector, err := h.Queries.GetEnabledConnectorInWorkspace(r.Context(), db.GetEnabledConnectorInWorkspaceParams{ID: connectorID, WorkspaceID: workspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "connector is not enabled in this workspace")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load connector")
			return
		}
		if connector.ConnectorType != strings.TrimSpace(req.SourceType) {
			writeError(w, 400, "source_type does not match connector type")
			return
		}
		validatedTemplate, err = h.Queries.SelectMatchingIssueTemplate(r.Context(), db.SelectMatchingIssueTemplateParams{WorkspaceID: workspaceID, ConnectorID: connectorID, SourceStatus: optionalText(req.SourceStatus), Labels: labels, Fields: fieldsJSON})
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusUnprocessableEntity, "no enabled issue template matched")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to select issue template")
			return
		}
		if code, msg := h.validateAssigneePair(r.Context(), r, h.resolveWorkspaceID(r), validatedTemplate.AssigneeType, validatedTemplate.AssigneeID); code != 0 {
			writeError(w, code, msg)
			return
		}
	}
	var issueID pgtype.UUID
	if req.IssueID != nil {
		issue, ok := h.loadIssueForUser(w, r, *req.IssueID)
		if !ok {
			return
		}
		issueID = issue.ID
	}
	creatorID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	var createParams service.IssueCreateParams
	var createOpts service.IssueCreateOpts
	if req.CreateIssue != nil {
		createParams, createOpts, ok = h.explicitIngestIssueParams(w, r, req, workspaceID, creatorID)
		if !ok {
			return
		}
	}
	observedAt, err := parseIngestObservedAt(req.ObservedAt)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, 500, "failed to start transaction")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)
	if connectorID.Valid {
		connector, err := qtx.LockEnabledConnectorForRouting(r.Context(), db.LockEnabledConnectorForRoutingParams{ID: connectorID, WorkspaceID: workspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusConflict, "connector changed during ingest")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to lock connector")
			return
		}
		if connector.ConnectorType != strings.TrimSpace(req.SourceType) {
			writeError(w, http.StatusConflict, "connector changed during ingest")
			return
		}
	}
	fingerprint, err := ingestRequestFingerprint(req, connectorID, labels, fieldsObject, observedAt, req.ObservedAt != nil && strings.TrimSpace(*req.ObservedAt) != "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to fingerprint ingest request")
		return
	}
	attempt, err := qtx.RecordOrBumpIntegrationIngestAttempt(r.Context(), db.RecordOrBumpIntegrationIngestAttemptParams{WorkspaceID: workspaceID, SourceType: strings.TrimSpace(req.SourceType), IdempotencyKey: strings.TrimSpace(req.IdempotencyKey), RequestFingerprint: fingerprint, ConnectorID: connectorID, ObservedAt: timestamptz(observedAt)})
	if err != nil {
		writeError(w, 500, "failed to record ingest attempt")
		return
	}
	if !attempt.Inserted {
		if attempt.RequestFingerprint != "" && attempt.RequestFingerprint != fingerprint {
			writeError(w, http.StatusConflict, "idempotency key was already used for a different request")
			return
		}
		h.respondDuplicateIngest(w, r, tx, workspaceID, attempt, req.CreateIssue != nil)
		return
	}
	var template db.IssueTemplate
	if connectorID.Valid {
		template, err = qtx.SelectMatchingIssueTemplate(r.Context(), db.SelectMatchingIssueTemplateParams{WorkspaceID: workspaceID, ConnectorID: connectorID, SourceStatus: optionalText(req.SourceStatus), Labels: labels, Fields: fieldsJSON})
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusConflict, "matched issue template changed during ingest")
			return
		}
		if err != nil {
			writeError(w, 500, "failed to select issue template")
			return
		}
		if template.ID != validatedTemplate.ID {
			writeError(w, http.StatusConflict, "matched issue template changed during ingest")
			return
		}
		status := template.Status
		if !template.AutoStart {
			status = "backlog"
		}
		description := pgtype.Text{}
		switch template.DescriptionSource {
		case "summary":
			description = optionalText(req.Summary)
		case "title":
			description = pgtype.Text{String: strings.TrimSpace(req.Title), Valid: true}
		}
		createParams = service.IssueCreateParams{WorkspaceID: workspaceID, Title: template.TitlePrefix + strings.TrimSpace(req.Title), Description: description, Status: status, Priority: template.IssuePriority, AssigneeType: template.AssigneeType, AssigneeID: template.AssigneeID, CreatorType: "member", CreatorID: parseUUID(creatorID), AllowDuplicate: true}
		createOpts = service.IssueCreateOpts{ActorID: creatorID, AnalyticsAgentID: templateAnalyticsAgent(template)}
	}
	record, err := qtx.UpsertExternalRecord(r.Context(), db.UpsertExternalRecordParams{WorkspaceID: workspaceID, SourceType: strings.TrimSpace(req.SourceType), ExternalID: strings.TrimSpace(req.ExternalID), Title: strings.TrimSpace(req.Title), SchemaVersion: ingestSchemaVersion(req.SchemaVersion), Labels: labels, Fields: fieldsJSON, ConnectorID: connectorID, ExternalKey: optionalText(req.ExternalKey), Summary: optionalText(req.Summary), SourceStatus: optionalText(req.SourceStatus), SourceUrl: optionalText(req.SourceURL), LastSeenAt: timestamptz(observedAt)})
	if err != nil {
		writeError(w, 500, "failed to store external record")
		return
	}
	var postCommit *service.IssueCreatePostCommit
	if req.CreateIssue != nil || connectorID.Valid {
		created, pc, err := h.IssueService.CreateInTx(r.Context(), tx, createParams, createOpts)
		if err != nil {
			writeError(w, 500, "failed to create issue")
			return
		}
		issueID = created.Issue.ID
		postCommit = pc
	}
	if issueID.Valid {
		if _, err := qtx.CreateIssueExternalRecordBinding(r.Context(), db.CreateIssueExternalRecordBindingParams{WorkspaceID: workspaceID, IssueID: issueID, ExternalRecordID: record.ID, BindingRole: "primary"}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			writeError(w, 500, "failed to bind external record to issue")
			return
		}
	}
	outcome := "updated"
	if record.Inserted {
		outcome = "created"
	}
	completed, err := qtx.CompleteIntegrationIngestAttempt(r.Context(), db.CompleteIntegrationIngestAttemptParams{ID: attempt.ID, ExternalRecordID: record.ID, Outcome: outcome, IssueID: issueID, ConnectorID: connectorID, IssueTemplateID: template.ID, IssueTemplateVersion: pgtype.Int4{Int32: template.Version, Valid: template.ID.Valid}})
	if err != nil {
		writeError(w, 500, "failed to complete ingest attempt")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, 500, "failed to commit ingest")
		return
	}
	if postCommit != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), workbenchPostCommitTimeout)
		postCommit.Run(ctx)
		cancel()
	}
	response := ingestResponse(externalRecordUpsertToResponse(record), completed.Outcome, completed.AttemptCount, issueID, connectorID, template.ID, pgtype.Int4{Int32: template.Version, Valid: template.ID.Valid})
	statusCode := http.StatusOK
	if outcome == "created" {
		statusCode = http.StatusCreated
	}
	writeJSON(w, statusCode, response)
}

func (h *Handler) explicitIngestIssueParams(w http.ResponseWriter, r *http.Request, req IngestExternalRecordRequest, workspaceID pgtype.UUID, creatorID string) (service.IssueCreateParams, service.IssueCreateOpts, bool) {
	status := req.CreateIssue.Status
	if status == "" {
		status = "todo"
	}
	priority := req.CreateIssue.Priority
	if priority == "" {
		priority = "none"
	}
	if !validateIssueEnum(w, "status", status, validIssueStatuses) || !validateIssueEnum(w, "priority", priority, validIssuePriorities) {
		return service.IssueCreateParams{}, service.IssueCreateOpts{}, false
	}
	var at pgtype.Text
	var aid pgtype.UUID
	if req.CreateIssue.AssigneeType != nil {
		at = pgtype.Text{String: *req.CreateIssue.AssigneeType, Valid: true}
	}
	if req.CreateIssue.AssigneeID != nil {
		id, ok := parseUUIDOrBadRequest(w, *req.CreateIssue.AssigneeID, "assignee_id")
		if !ok {
			return service.IssueCreateParams{}, service.IssueCreateOpts{}, false
		}
		aid = id
	}
	if code, msg := h.validateAssigneePair(r.Context(), r, h.resolveWorkspaceID(r), at, aid); code != 0 {
		writeError(w, code, msg)
		return service.IssueCreateParams{}, service.IssueCreateOpts{}, false
	}
	return service.IssueCreateParams{WorkspaceID: workspaceID, Title: strings.TrimSpace(req.Title), Description: ptrToText(req.CreateIssue.Description), Status: status, Priority: priority, AssigneeType: at, AssigneeID: aid, CreatorType: "member", CreatorID: parseUUID(creatorID), AllowDuplicate: true}, service.IssueCreateOpts{ActorID: creatorID, AnalyticsAgentID: func() string {
		if at.Valid && at.String == "agent" {
			return uuidToString(aid)
		}
		return ""
	}()}, true
}

func (h *Handler) respondDuplicateIngest(w http.ResponseWriter, r *http.Request, tx pgx.Tx, workspaceID pgtype.UUID, attempt db.RecordOrBumpIntegrationIngestAttemptRow, explicitCreate bool) {
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, 500, "failed to commit ingest attempt")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), workbenchPostCommitTimeout)
	defer cancel()
	if attempt.IssueID.Valid && (attempt.IssueTemplateID.Valid || explicitCreate) {
		if err := h.IssueService.ReconcileNeverEnqueuedIssue(ctx, workspaceID, attempt.IssueID); err != nil {
			writeError(w, 500, "failed to reconcile duplicate ingest")
			return
		}
	}
	record, err := h.Queries.GetExternalRecordInWorkspace(ctx, db.GetExternalRecordInWorkspaceParams{ID: attempt.ExternalRecordID, WorkspaceID: workspaceID})
	if err != nil {
		writeError(w, 409, "ingest is already processing")
		return
	}
	writeJSON(w, http.StatusOK, ingestResponse(externalRecordToResponse(record), "duplicate", attempt.AttemptCount, attempt.IssueID, attempt.ConnectorID, attempt.IssueTemplateID, attempt.IssueTemplateVersion))
}

func (h *Handler) ListIssueExternalRecords(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	rows, err := h.Queries.ListIssueExternalRecordBindings(r.Context(), db.ListIssueExternalRecordBindingsParams{WorkspaceID: issue.WorkspaceID, IssueID: issue.ID})
	if err != nil {
		writeError(w, 500, "failed to list external records")
		return
	}
	response := make([]IssueExternalRecordBindingResponse, len(rows))
	for i, row := range rows {
		response[i] = IssueExternalRecordBindingResponse{BindingID: uuidToString(row.BindingID), BindingRole: row.BindingRole, BoundAt: timestampToString(row.BoundAt), ExternalRecord: externalRecordParts(row.ExternalRecordID, row.ConnectorID, row.SourceType, row.ExternalID, row.ExternalKey, row.Title, row.Summary, row.SourceStatus, row.SourceUrl, row.SchemaVersion, row.Labels, row.Fields, row.LastSeenAt)}
	}
	writeJSON(w, 200, map[string]any{"external_records": response})
}

func decodeWorkbenchJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, 400, "invalid request body")
		return false
	}
	var extra any
	if decoder.Decode(&extra) == nil {
		writeError(w, 400, "invalid request body")
		return false
	}
	return true
}
func validateRequiredWorkbenchText(values map[string]string) error {
	for field, value := range values {
		if strings.TrimSpace(value) == "" {
			return errors.New(field + " is required")
		}
		if len(value) > maxWorkbenchTextLength {
			return errors.New(field + " exceeds the maximum length")
		}
	}
	return nil
}
func validateIngestExternalRecordRequest(req IngestExternalRecordRequest) error {
	if req.ConnectorID != nil && (req.IssueID != nil || req.CreateIssue != nil) {
		return errors.New("connector_id cannot be combined with issue_id or create_issue")
	}
	if req.IssueID != nil && req.CreateIssue != nil {
		return errors.New("issue_id and create_issue are mutually exclusive")
	}
	if err := validateRequiredWorkbenchText(map[string]string{"source_type": req.SourceType, "external_id": req.ExternalID, "title": req.Title, "idempotency_key": req.IdempotencyKey}); err != nil {
		return err
	}
	for field, value := range map[string]*string{"external_key": req.ExternalKey, "summary": req.Summary, "source_status": req.SourceStatus, "source_url": req.SourceURL, "schema_version": req.SchemaVersion} {
		if value != nil && len(*value) > maxWorkbenchTextLength {
			return errors.New(field + " exceeds the maximum length")
		}
	}
	if req.CreateIssue != nil && req.CreateIssue.Description != nil && len(*req.CreateIssue.Description) > maxWorkbenchTextLength {
		return errors.New("create_issue.description exceeds the maximum length")
	}
	return nil
}
func validateScalarObject(value map[string]any) error {
	for key, v := range value {
		if strings.TrimSpace(key) == "" || len(key) > maxWorkbenchTextLength {
			return errors.New("contains an invalid key")
		}
		if text, ok := v.(string); ok && len(text) > maxWorkbenchTextLength {
			return errors.New("contains an overlong string value")
		}
		switch v.(type) {
		case nil, string, float64, bool, json.Number:
		default:
			return errors.New("must contain scalar values only")
		}
	}
	return nil
}
func normalizeLabels(values []string) ([]string, error) {
	if len(values) > 256 {
		return nil, errors.New("exceed the maximum count")
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		v := strings.TrimSpace(value)
		if v == "" {
			continue
		}
		if len(v) > 256 {
			return nil, errors.New("contain an overlong value")
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out, nil
}
func ingestRequestFingerprint(req IngestExternalRecordRequest, connectorID pgtype.UUID, labels []string, fields map[string]any, observedAt time.Time, observedAtExplicit bool) (string, error) {
	identity := struct {
		ConnectorID   string                    `json:"connector_id"`
		SourceType    string                    `json:"source_type"`
		ExternalID    string                    `json:"external_id"`
		ExternalKey   *string                   `json:"external_key,omitempty"`
		Title         string                    `json:"title"`
		Summary       *string                   `json:"summary,omitempty"`
		SourceStatus  *string                   `json:"source_status,omitempty"`
		SourceURL     *string                   `json:"source_url,omitempty"`
		SchemaVersion string                    `json:"schema_version"`
		IssueID       *string                   `json:"issue_id,omitempty"`
		CreateIssue   *IngestCreateIssueRequest `json:"create_issue,omitempty"`
		Labels        []string                  `json:"labels"`
		Fields        map[string]any            `json:"fields"`
		ObservedAt    string                    `json:"observed_at"`
	}{
		ConnectorID:   uuidToString(connectorID),
		SourceType:    strings.TrimSpace(req.SourceType),
		ExternalID:    strings.TrimSpace(req.ExternalID),
		ExternalKey:   normalizedOptionalString(req.ExternalKey),
		Title:         strings.TrimSpace(req.Title),
		Summary:       normalizedOptionalString(req.Summary),
		SourceStatus:  normalizedOptionalString(req.SourceStatus),
		SourceURL:     normalizedOptionalString(req.SourceURL),
		SchemaVersion: ingestSchemaVersion(req.SchemaVersion),
		IssueID:       normalizedOptionalString(req.IssueID),
		CreateIssue:   req.CreateIssue,
		Labels:        labels,
		Fields:        fields,
	}
	if observedAtExplicit {
		identity.ObservedAt = observedAt.UTC().Format(time.RFC3339Nano)
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(encoded)), nil
}
func defaultObject(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	return value
}
func normalizedOptionalString(value *string) *string {
	if value == nil {
		return nil
	}
	normalized := strings.TrimSpace(*value)
	if normalized == "" {
		return nil
	}
	return &normalized
}
func parseIngestObservedAt(raw *string) (time.Time, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return time.Now().UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, *raw)
	if err != nil {
		return time.Time{}, errors.New("observed_at must be an RFC3339 timestamp")
	}
	return t.UTC(), nil
}
func ingestSchemaVersion(value *string) string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return "v1"
	}
	return strings.TrimSpace(*value)
}
func optionalText(value *string) pgtype.Text {
	if value == nil || strings.TrimSpace(*value) == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: strings.TrimSpace(*value), Valid: true}
}
func timestamptz(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}
func templateAnalyticsAgent(t db.IssueTemplate) string {
	if t.AssigneeType.Valid && t.AssigneeType.String == "agent" {
		return uuidToString(t.AssigneeID)
	}
	return ""
}
func ingestResponse(record ExternalRecordResponse, outcome string, count int32, issueID, connectorID, templateID pgtype.UUID, version pgtype.Int4) IngestExternalRecordResponse {
	r := IngestExternalRecordResponse{ExternalRecord: record, Outcome: outcome, AttemptCount: count}
	if issueID.Valid {
		v := uuidToString(issueID)
		r.IssueID = &v
	}
	if connectorID.Valid {
		v := uuidToString(connectorID)
		r.ConnectorID = &v
	}
	if templateID.Valid {
		v := uuidToString(templateID)
		r.IssueTemplateID = &v
	}
	if version.Valid {
		v := version.Int32
		r.IssueTemplateVersion = &v
	}
	return r
}
func externalRecordParts(id, connectorID pgtype.UUID, sourceType, externalID string, externalKey pgtype.Text, title string, summary, sourceStatus, sourceURL pgtype.Text, schemaVersion string, labels []string, fields []byte, lastSeen pgtype.Timestamptz) ExternalRecordResponse {
	result := ExternalRecordResponse{ID: uuidToString(id), SourceType: sourceType, ExternalID: externalID, ExternalKey: textToPtr(externalKey), Title: title, Summary: textToPtr(summary), SourceStatus: textToPtr(sourceStatus), SourceURL: textToPtr(sourceURL), SchemaVersion: schemaVersion, Labels: labels, Fields: map[string]any{}, LastSeenAt: timestampToString(lastSeen)}
	if connectorID.Valid {
		v := uuidToString(connectorID)
		result.ConnectorID = &v
	}
	_ = json.Unmarshal(fields, &result.Fields)
	if result.Labels == nil {
		result.Labels = []string{}
	}
	return result
}
func externalRecordToResponse(r db.ExternalRecord) ExternalRecordResponse {
	return externalRecordParts(r.ID, r.ConnectorID, r.SourceType, r.ExternalID, r.ExternalKey, r.Title, r.Summary, r.SourceStatus, r.SourceUrl, r.SchemaVersion, r.Labels, r.Fields, r.LastSeenAt)
}
func externalRecordUpsertToResponse(r db.UpsertExternalRecordRow) ExternalRecordResponse {
	return externalRecordParts(r.ID, r.ConnectorID, r.SourceType, r.ExternalID, r.ExternalKey, r.Title, r.Summary, r.SourceStatus, r.SourceUrl, r.SchemaVersion, r.Labels, r.Fields, r.LastSeenAt)
}
