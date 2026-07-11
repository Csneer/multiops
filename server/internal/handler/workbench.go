package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const maxWorkbenchTextLength = 16 * 1024

type IngestExternalRecordRequest struct {
	SourceType     string  `json:"source_type"`
	ExternalID     string  `json:"external_id"`
	ExternalKey    *string `json:"external_key"`
	Title          string  `json:"title"`
	Summary        *string `json:"summary"`
	SourceStatus   *string `json:"source_status"`
	SourceURL      *string `json:"source_url"`
	SchemaVersion  *string `json:"schema_version"`
	IdempotencyKey string  `json:"idempotency_key"`
	IssueID        *string `json:"issue_id"`
	ObservedAt     *string `json:"observed_at"`
}

type ExternalRecordResponse struct {
	ID            string  `json:"id"`
	SourceType    string  `json:"source_type"`
	ExternalID    string  `json:"external_id"`
	ExternalKey   *string `json:"external_key,omitempty"`
	Title         string  `json:"title"`
	Summary       *string `json:"summary,omitempty"`
	SourceStatus  *string `json:"source_status,omitempty"`
	SourceURL     *string `json:"source_url,omitempty"`
	SchemaVersion string  `json:"schema_version"`
	LastSeenAt    string  `json:"last_seen_at"`
}

type IngestExternalRecordResponse struct {
	ExternalRecord ExternalRecordResponse `json:"external_record"`
	IssueID        *string                `json:"issue_id,omitempty"`
	Outcome        string                 `json:"outcome"`
	AttemptCount   int32                  `json:"attempt_count"`
}

type IssueExternalRecordBindingResponse struct {
	BindingID      string                 `json:"binding_id"`
	BindingRole    string                 `json:"binding_role"`
	BoundAt        string                 `json:"bound_at"`
	ExternalRecord ExternalRecordResponse `json:"external_record"`
}

func (h *Handler) IngestExternalRecord(w http.ResponseWriter, r *http.Request) {
	var req IngestExternalRecordRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := validateIngestExternalRecordRequest(req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	workspaceID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace id")
	if !ok {
		return
	}

	var issueID pgtype.UUID
	if req.IssueID != nil {
		issue, ok := h.loadIssueForUser(w, r, *req.IssueID)
		if !ok {
			return
		}
		issueID = issue.ID
	}

	observedAt, err := parseIngestObservedAt(req.ObservedAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start transaction")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)

	attempt, err := qtx.RecordOrBumpIntegrationIngestAttempt(r.Context(), db.RecordOrBumpIntegrationIngestAttemptParams{
		WorkspaceID:    workspaceID,
		SourceType:     strings.TrimSpace(req.SourceType),
		IdempotencyKey: strings.TrimSpace(req.IdempotencyKey),
		ObservedAt:     timestamptz(observedAt),
	})
	if err != nil {
		slog.Warn("record integration ingest attempt failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to record ingest attempt")
		return
	}
	if !attempt.Inserted {
		if err := tx.Commit(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to commit ingest attempt")
			return
		}
		record, err := h.Queries.GetExternalRecordInWorkspace(r.Context(), db.GetExternalRecordInWorkspaceParams{
			ID:          attempt.ExternalRecordID,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			slog.Warn("load duplicate ingest external record failed", "error", err, "attempt_id", uuidToString(attempt.ID))
			writeError(w, http.StatusConflict, "ingest is already processing")
			return
		}
		response := IngestExternalRecordResponse{
			ExternalRecord: externalRecordToResponse(record),
			Outcome:        "duplicate",
			AttemptCount:   attempt.AttemptCount,
		}
		if attempt.IssueID.Valid {
			id := uuidToString(attempt.IssueID)
			response.IssueID = &id
		}
		writeJSON(w, http.StatusOK, response)
		return
	}

	record, err := qtx.UpsertExternalRecord(r.Context(), db.UpsertExternalRecordParams{
		WorkspaceID:   workspaceID,
		SourceType:    strings.TrimSpace(req.SourceType),
		ExternalID:    strings.TrimSpace(req.ExternalID),
		Title:         strings.TrimSpace(req.Title),
		SchemaVersion: ingestSchemaVersion(req.SchemaVersion),
		ExternalKey:   optionalText(req.ExternalKey),
		Summary:       optionalText(req.Summary),
		SourceStatus:  optionalText(req.SourceStatus),
		SourceUrl:     optionalText(req.SourceURL),
		LastSeenAt:    timestamptz(observedAt),
	})
	if err != nil {
		slog.Warn("upsert external record failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to store external record")
		return
	}

	if issueID.Valid {
		if _, err := qtx.CreateIssueExternalRecordBinding(r.Context(), db.CreateIssueExternalRecordBindingParams{
			WorkspaceID:      workspaceID,
			IssueID:          issueID,
			ExternalRecordID: record.ID,
			BindingRole:      "primary",
		}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("create issue external record binding failed", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to bind external record to issue")
			return
		}
	}

	outcome := "updated"
	if record.Inserted {
		outcome = "created"
	}
	completed, err := qtx.CompleteIntegrationIngestAttempt(r.Context(), db.CompleteIntegrationIngestAttemptParams{
		ID:               attempt.ID,
		ExternalRecordID: record.ID,
		Outcome:          outcome,
		IssueID:          issueID,
	})
	if err != nil {
		slog.Warn("complete integration ingest attempt failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to complete ingest attempt")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit ingest")
		return
	}

	response := IngestExternalRecordResponse{
		ExternalRecord: externalRecordUpsertToResponse(record),
		Outcome:        completed.Outcome,
		AttemptCount:   completed.AttemptCount,
	}
	if issueID.Valid {
		id := uuidToString(issueID)
		response.IssueID = &id
	}
	status := http.StatusOK
	if outcome == "created" {
		status = http.StatusCreated
	}
	writeJSON(w, status, response)
}

func (h *Handler) ListIssueExternalRecords(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	rows, err := h.Queries.ListIssueExternalRecordBindings(r.Context(), db.ListIssueExternalRecordBindingsParams{
		WorkspaceID: issue.WorkspaceID,
		IssueID:     issue.ID,
	})
	if err != nil {
		slog.Warn("list issue external records failed", "error", err, "issue_id", uuidToString(issue.ID))
		writeError(w, http.StatusInternalServerError, "failed to list external records")
		return
	}
	response := make([]IssueExternalRecordBindingResponse, len(rows))
	for i, row := range rows {
		response[i] = IssueExternalRecordBindingResponse{
			BindingID:   uuidToString(row.BindingID),
			BindingRole: row.BindingRole,
			BoundAt:     timestampToString(row.BoundAt),
			ExternalRecord: ExternalRecordResponse{
				ID:            uuidToString(row.ExternalRecordID),
				SourceType:    row.SourceType,
				ExternalID:    row.ExternalID,
				ExternalKey:   textToPtr(row.ExternalKey),
				Title:         row.Title,
				Summary:       textToPtr(row.Summary),
				SourceStatus:  textToPtr(row.SourceStatus),
				SourceURL:     textToPtr(row.SourceUrl),
				SchemaVersion: row.SchemaVersion,
				LastSeenAt:    timestampToString(row.LastSeenAt),
			},
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"external_records": response})
}

func validateIngestExternalRecordRequest(req IngestExternalRecordRequest) error {
	for field, value := range map[string]string{
		"source_type":     req.SourceType,
		"external_id":     req.ExternalID,
		"title":           req.Title,
		"idempotency_key": req.IdempotencyKey,
	} {
		if strings.TrimSpace(value) == "" {
			return errors.New(field + " is required")
		}
		if len(value) > maxWorkbenchTextLength {
			return errors.New(field + " exceeds the maximum length")
		}
	}
	for field, value := range map[string]*string{
		"external_key":   req.ExternalKey,
		"summary":        req.Summary,
		"source_status":  req.SourceStatus,
		"source_url":     req.SourceURL,
		"schema_version": req.SchemaVersion,
	} {
		if value != nil && len(*value) > maxWorkbenchTextLength {
			return errors.New(field + " exceeds the maximum length")
		}
	}
	return nil
}

func parseIngestObservedAt(raw *string) (time.Time, error) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return time.Now().UTC(), nil
	}
	observedAt, err := time.Parse(time.RFC3339, *raw)
	if err != nil {
		return time.Time{}, errors.New("observed_at must be an RFC3339 timestamp")
	}
	return observedAt.UTC(), nil
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

func externalRecordToResponse(record db.ExternalRecord) ExternalRecordResponse {
	return ExternalRecordResponse{
		ID:            uuidToString(record.ID),
		SourceType:    record.SourceType,
		ExternalID:    record.ExternalID,
		ExternalKey:   textToPtr(record.ExternalKey),
		Title:         record.Title,
		Summary:       textToPtr(record.Summary),
		SourceStatus:  textToPtr(record.SourceStatus),
		SourceURL:     textToPtr(record.SourceUrl),
		SchemaVersion: record.SchemaVersion,
		LastSeenAt:    timestampToString(record.LastSeenAt),
	}
}

func externalRecordUpsertToResponse(record db.UpsertExternalRecordRow) ExternalRecordResponse {
	return ExternalRecordResponse{
		ID:            uuidToString(record.ID),
		SourceType:    record.SourceType,
		ExternalID:    record.ExternalID,
		ExternalKey:   textToPtr(record.ExternalKey),
		Title:         record.Title,
		Summary:       textToPtr(record.Summary),
		SourceStatus:  textToPtr(record.SourceStatus),
		SourceURL:     textToPtr(record.SourceUrl),
		SchemaVersion: record.SchemaVersion,
		LastSeenAt:    timestampToString(record.LastSeenAt),
	}
}
