package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const WorkbenchSystemActorID = "00000000-0000-0000-0000-000000000000"

type WorkbenchIngestTxStarter interface {
	Begin(context.Context) (pgx.Tx, error)
}

type WorkbenchIngestInput struct {
	WorkspaceID, ConnectorID, CredentialID, IssueID, CreatorID   pgtype.UUID
	Machine                                                      bool
	ActorID                                                      string
	SourceType, ExternalID, Title, IdempotencyKey, SchemaVersion string
	ExternalKey, Summary, SourceStatus, SourceURL                pgtype.Text
	Labels                                                       []string
	Fields                                                       map[string]any
	ObservedAt                                                   time.Time
	ObservedAtExplicit                                           bool
	ValidatedTemplate                                            db.IssueTemplate
	CreateParams                                                 IssueCreateParams
	CreateOpts                                                   IssueCreateOpts
	CreateIssue                                                  bool
}

type WorkbenchIngestResult struct {
	Record                           db.UpsertExternalRecordRow
	ExistingRecord                   db.ExternalRecord
	Completed                        db.IntegrationIngestAttempt
	Attempt                          db.RecordOrBumpIntegrationIngestAttemptRow
	IssueID, ConnectorID, TemplateID pgtype.UUID
	TemplateVersion                  pgtype.Int4
	Outcome                          string
	Duplicate                        bool
}

type WorkbenchIngestError struct {
	Status  int
	Message string
}

func (e *WorkbenchIngestError) Error() string { return e.Message }

func ingestErr(status int, message string) error {
	return &WorkbenchIngestError{Status: status, Message: message}
}

type WorkbenchIngestService struct {
	Queries           *db.Queries
	TxStarter         WorkbenchIngestTxStarter
	Issues            *IssueService
	PostCommitTimeout time.Duration
}

func NewWorkbenchIngestService(q *db.Queries, tx WorkbenchIngestTxStarter, issues *IssueService) *WorkbenchIngestService {
	return &WorkbenchIngestService{Queries: q, TxStarter: tx, Issues: issues, PostCommitTimeout: 10 * time.Second}
}

func (s *WorkbenchIngestService) Ingest(ctx context.Context, in WorkbenchIngestInput) (WorkbenchIngestResult, error) {
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return WorkbenchIngestResult{}, ingestErr(500, "failed to start transaction")
	}
	defer tx.Rollback(ctx)
	q := s.Queries.WithTx(tx)
	var template db.IssueTemplate
	if in.ConnectorID.Valid {
		connector, err := q.LockEnabledConnectorForRouting(ctx, db.LockEnabledConnectorForRoutingParams{ID: in.ConnectorID, WorkspaceID: in.WorkspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return WorkbenchIngestResult{}, ingestErr(409, "connector changed during ingest")
		}
		if err != nil {
			return WorkbenchIngestResult{}, ingestErr(500, "failed to lock connector")
		}
		if connector.ConnectorType != strings.TrimSpace(in.SourceType) {
			return WorkbenchIngestResult{}, ingestErr(409, "connector changed during ingest")
		}
		if in.Machine {
			if _, err := q.GetActiveConnectorCredentialForUpdate(ctx, db.GetActiveConnectorCredentialForUpdateParams{ID: in.CredentialID, ConnectorID: in.ConnectorID, WorkspaceID: in.WorkspaceID}); errors.Is(err, pgx.ErrNoRows) {
				return WorkbenchIngestResult{}, ingestErr(401, "invalid connector credential")
			} else if err != nil {
				return WorkbenchIngestResult{}, ingestErr(500, "failed to revalidate connector credential")
			}
		}
	}
	if in.Machine {
		if err := q.UpdateConnectorCredentialLastUsed(ctx, db.UpdateConnectorCredentialLastUsedParams{ID: in.CredentialID, ConnectorID: in.ConnectorID, WorkspaceID: in.WorkspaceID}); err != nil {
			return WorkbenchIngestResult{}, ingestErr(500, "failed to update connector credential usage")
		}
	}
	fingerprint, err := workbenchFingerprint(in)
	if err != nil {
		return WorkbenchIngestResult{}, ingestErr(500, "failed to fingerprint ingest request")
	}
	attempt, err := q.RecordOrBumpIntegrationIngestAttempt(ctx, db.RecordOrBumpIntegrationIngestAttemptParams{WorkspaceID: in.WorkspaceID, SourceType: strings.TrimSpace(in.SourceType), IdempotencyKey: strings.TrimSpace(in.IdempotencyKey), RequestFingerprint: fingerprint, ConnectorID: in.ConnectorID, ObservedAt: pgtype.Timestamptz{Time: in.ObservedAt, Valid: true}})
	if err != nil {
		return WorkbenchIngestResult{}, ingestErr(500, "failed to record ingest attempt")
	}
	if !attempt.Inserted {
		if attempt.RequestFingerprint != "" && attempt.RequestFingerprint != fingerprint {
			return WorkbenchIngestResult{}, ingestErr(409, "idempotency key was already used for a different request")
		}
		if err := tx.Commit(ctx); err != nil {
			return WorkbenchIngestResult{}, ingestErr(500, "failed to commit ingest attempt")
		}
		postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.PostCommitTimeout)
		defer cancel()
		if attempt.IssueID.Valid && (attempt.IssueTemplateID.Valid || in.CreateIssue) {
			if err := s.Issues.ReconcileNeverEnqueuedIssue(postCtx, in.WorkspaceID, attempt.IssueID); err != nil {
				return WorkbenchIngestResult{}, ingestErr(500, "failed to reconcile duplicate ingest")
			}
		}
		record, err := s.Queries.GetExternalRecordInWorkspace(postCtx, db.GetExternalRecordInWorkspaceParams{ID: attempt.ExternalRecordID, WorkspaceID: in.WorkspaceID})
		if err != nil {
			return WorkbenchIngestResult{}, ingestErr(409, "ingest is already processing")
		}
		return WorkbenchIngestResult{ExistingRecord: record, Attempt: attempt, IssueID: attempt.IssueID, ConnectorID: attempt.ConnectorID, TemplateID: attempt.IssueTemplateID, TemplateVersion: attempt.IssueTemplateVersion, Outcome: "duplicate", Duplicate: true}, nil
	}
	createParams, createOpts := in.CreateParams, in.CreateOpts
	if in.ConnectorID.Valid {
		template, err = q.SelectMatchingIssueTemplate(ctx, db.SelectMatchingIssueTemplateParams{WorkspaceID: in.WorkspaceID, ConnectorID: in.ConnectorID, SourceStatus: in.SourceStatus, Labels: in.Labels, Fields: mustJSON(in.Fields)})
		if errors.Is(err, pgx.ErrNoRows) {
			return WorkbenchIngestResult{}, ingestErr(409, "matched issue template changed during ingest")
		}
		if err != nil {
			return WorkbenchIngestResult{}, ingestErr(500, "failed to select issue template")
		}
		if template.ID != in.ValidatedTemplate.ID {
			return WorkbenchIngestResult{}, ingestErr(409, "matched issue template changed during ingest")
		}
		status := template.Status
		if !template.AutoStart {
			status = "backlog"
		}
		description := pgtype.Text{}
		if template.DescriptionSource == "summary" {
			description = in.Summary
		} else if template.DescriptionSource == "title" {
			description = pgtype.Text{String: strings.TrimSpace(in.Title), Valid: true}
		}
		createParams = IssueCreateParams{WorkspaceID: in.WorkspaceID, Title: template.TitlePrefix + strings.TrimSpace(in.Title), Description: description, Status: status, Priority: template.IssuePriority, AssigneeType: template.AssigneeType, AssigneeID: template.AssigneeID, CreatorType: "member", CreatorID: in.CreatorID, AllowDuplicate: true}
		createOpts = IssueCreateOpts{ActorID: in.ActorID, SuppressActorFallback: in.Machine, AnalyticsAgentID: templateAnalyticsAgentID(template)}
	}
	record, err := q.UpsertExternalRecord(ctx, db.UpsertExternalRecordParams{WorkspaceID: in.WorkspaceID, SourceType: strings.TrimSpace(in.SourceType), ExternalID: strings.TrimSpace(in.ExternalID), Title: strings.TrimSpace(in.Title), SchemaVersion: in.SchemaVersion, Labels: in.Labels, Fields: mustJSON(in.Fields), ConnectorID: in.ConnectorID, ExternalKey: in.ExternalKey, Summary: in.Summary, SourceStatus: in.SourceStatus, SourceUrl: in.SourceURL, LastSeenAt: pgtype.Timestamptz{Time: in.ObservedAt, Valid: true}})
	if err != nil {
		return WorkbenchIngestResult{}, ingestErr(500, "failed to store external record")
	}
	issueID := in.IssueID
	templateID := template.ID
	templateVersion := pgtype.Int4{Int32: template.Version, Valid: template.ID.Valid}
	var postCommit *IssueCreatePostCommit
	var existingBinding bool
	if !record.Inserted {
		binding, bindingErr := q.GetPrimaryIssueBindingForExternalRecord(ctx, db.GetPrimaryIssueBindingForExternalRecordParams{WorkspaceID: in.WorkspaceID, ExternalRecordID: record.ID})
		if bindingErr == nil && !in.IssueID.Valid {
			issueID = binding.IssueID
			existingBinding = true
			audit, auditErr := q.GetOriginalIngestAuditForExternalRecord(ctx, db.GetOriginalIngestAuditForExternalRecordParams{WorkspaceID: in.WorkspaceID, ExternalRecordID: record.ID})
			if auditErr == nil {
				templateID = audit.IssueTemplateID
				templateVersion = audit.IssueTemplateVersion
			} else if !errors.Is(auditErr, pgx.ErrNoRows) {
				return WorkbenchIngestResult{}, ingestErr(500, "failed to load existing ingest audit")
			}
		} else if !errors.Is(bindingErr, pgx.ErrNoRows) {
			return WorkbenchIngestResult{}, ingestErr(500, "failed to load existing issue binding")
		}
	}
	if !existingBinding && !issueID.Valid && (in.CreateIssue || in.ConnectorID.Valid) {
		created, pc, err := s.Issues.CreateInTx(ctx, tx, createParams, createOpts)
		if err != nil {
			return WorkbenchIngestResult{}, ingestErr(500, "failed to create issue")
		}
		issueID, postCommit = created.Issue.ID, pc
	}
	if issueID.Valid {
		if _, err := q.CreateIssueExternalRecordBinding(ctx, db.CreateIssueExternalRecordBindingParams{WorkspaceID: in.WorkspaceID, IssueID: issueID, ExternalRecordID: record.ID, BindingRole: "primary"}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return WorkbenchIngestResult{}, ingestErr(500, "failed to bind external record to issue")
		}
	}
	outcome := "updated"
	if record.Inserted {
		outcome = "created"
	}
	completed, err := q.CompleteIntegrationIngestAttempt(ctx, db.CompleteIntegrationIngestAttemptParams{ID: attempt.ID, ExternalRecordID: record.ID, Outcome: outcome, IssueID: issueID, ConnectorID: in.ConnectorID, IssueTemplateID: templateID, IssueTemplateVersion: templateVersion})
	if err != nil {
		return WorkbenchIngestResult{}, ingestErr(500, "failed to complete ingest attempt")
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkbenchIngestResult{}, ingestErr(500, "failed to commit ingest")
	}
	if postCommit != nil {
		postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.PostCommitTimeout)
		postCommit.Run(postCtx)
		cancel()
	}
	return WorkbenchIngestResult{Record: record, Completed: completed, IssueID: issueID, ConnectorID: in.ConnectorID, TemplateID: templateID, TemplateVersion: templateVersion, Outcome: outcome}, nil
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func templateAnalyticsAgentID(t db.IssueTemplate) string {
	if t.AssigneeType.Valid && t.AssigneeType.String == "agent" {
		return uuidString(t.AssigneeID)
	}
	return ""
}
func uuidString(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	b := id.Bytes
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
func workbenchFingerprint(in WorkbenchIngestInput) (string, error) {
	identity := struct {
		ConnectorID, SourceType, ExternalID, Title, SchemaVersion string
		ExternalKey, Summary, SourceStatus, SourceURL             pgtype.Text
		IssueID                                                   string
		CreateIssue                                               bool
		CreateParams                                              IssueCreateParams
		Labels                                                    []string
		Fields                                                    map[string]any
		ObservedAt                                                string
	}{uuidString(in.ConnectorID), strings.TrimSpace(in.SourceType), strings.TrimSpace(in.ExternalID), strings.TrimSpace(in.Title), in.SchemaVersion, in.ExternalKey, in.Summary, in.SourceStatus, in.SourceURL, uuidString(in.IssueID), in.CreateIssue, in.CreateParams, in.Labels, in.Fields, ""}
	if in.ObservedAtExplicit {
		identity.ObservedAt = in.ObservedAt.UTC().Format(time.RFC3339Nano)
	}
	b, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
