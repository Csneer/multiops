package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIngestExternalRecordCreatesBindingAndAttempt(t *testing.T) {
	issueID := createWorkbenchTestIssue(t, "Workbench ingest target")
	body := workbenchIngestBody("ferry", "work-order-5661", "Ferry work order 5661", "ingest-5661", issueID)

	w := httptest.NewRecorder()
	req := newRequest(http.MethodPost, "/api/integrations/ingest", body)
	testHandler.IngestExternalRecord(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("IngestExternalRecord: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var response IngestExternalRecordResponse
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("decode ingest response: %v", err)
	}
	if response.Outcome != "created" {
		t.Fatalf("outcome = %q, want created", response.Outcome)
	}
	if response.ExternalRecord.ExternalID != "work-order-5661" {
		t.Fatalf("external_id = %q", response.ExternalRecord.ExternalID)
	}
	if response.IssueID == nil || *response.IssueID != issueID {
		t.Fatalf("issue_id = %v, want %q", response.IssueID, issueID)
	}
	if response.AttemptCount != 1 {
		t.Fatalf("attempt_count = %d, want 1", response.AttemptCount)
	}

	w = httptest.NewRecorder()
	req = newRequest(http.MethodGet, "/api/issues/"+issueID+"/external-records", nil)
	req = withURLParam(req, "id", issueID)
	testHandler.ListIssueExternalRecords(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("ListIssueExternalRecords: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var bindings struct {
		ExternalRecords []IssueExternalRecordBindingResponse `json:"external_records"`
	}
	if err := json.NewDecoder(w.Body).Decode(&bindings); err != nil {
		t.Fatalf("decode binding response: %v", err)
	}
	if len(bindings.ExternalRecords) != 1 {
		t.Fatalf("binding count = %d, want 1", len(bindings.ExternalRecords))
	}
	if bindings.ExternalRecords[0].ExternalRecord.ExternalID != "work-order-5661" {
		t.Fatalf("binding external_id = %q", bindings.ExternalRecords[0].ExternalRecord.ExternalID)
	}
	if bindings.ExternalRecords[0].ExternalRecord.SchemaVersion != "v1" {
		t.Fatalf("binding schema_version = %q, want v1", bindings.ExternalRecords[0].ExternalRecord.SchemaVersion)
	}
}

func TestIngestExternalRecordUpdatesIdentityAndAuditsDuplicateReceipt(t *testing.T) {
	issueID := createWorkbenchTestIssue(t, "Workbench idempotency target")
	first := workbenchIngestBody("ferry", "work-order-5662", "Original title", "ingest-5662-a", issueID)

	w := httptest.NewRecorder()
	req := newRequest(http.MethodPost, "/api/integrations/ingest", first)
	testHandler.IngestExternalRecord(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("first ingest: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	req = newRequest(http.MethodPost, "/api/integrations/ingest", first)
	testHandler.IngestExternalRecord(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("duplicate ingest: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var duplicate IngestExternalRecordResponse
	if err := json.NewDecoder(w.Body).Decode(&duplicate); err != nil {
		t.Fatalf("decode duplicate response: %v", err)
	}
	if duplicate.Outcome != "duplicate" || duplicate.AttemptCount != 2 {
		t.Fatalf("duplicate result = %+v, want duplicate with attempt_count 2", duplicate)
	}

	updated := workbenchIngestBody("ferry", "work-order-5662", "Updated title", "ingest-5662-b", issueID)
	updated["summary"] = "Updated snapshot"
	w = httptest.NewRecorder()
	req = newRequest(http.MethodPost, "/api/integrations/ingest", updated)
	testHandler.IngestExternalRecord(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update ingest: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var result IngestExternalRecordResponse
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	if result.Outcome != "updated" || result.ExternalRecord.Title != "Updated title" {
		t.Fatalf("update result = %+v", result)
	}

	var recordCount, bindingCount, attemptCount, retryCount int
	if err := testPool.QueryRow(context.Background(), `
		SELECT
			(SELECT count(*) FROM external_record WHERE workspace_id = $1 AND source_type = 'ferry' AND external_id = 'work-order-5662'),
			(SELECT count(*) FROM issue_external_record_binding WHERE workspace_id = $1 AND issue_id = $2),
			(SELECT count(*) FROM integration_ingest_attempt WHERE workspace_id = $1 AND source_type = 'ferry' AND idempotency_key = 'ingest-5662-a'),
			(SELECT attempt_count FROM integration_ingest_attempt WHERE workspace_id = $1 AND source_type = 'ferry' AND idempotency_key = 'ingest-5662-a')
	`, testWorkspaceID, issueID).Scan(&recordCount, &bindingCount, &attemptCount, &retryCount); err != nil {
		t.Fatalf("load ingest audit rows: %v", err)
	}
	if recordCount != 1 || bindingCount != 1 || attemptCount != 1 || retryCount != 2 {
		t.Fatalf("counts record=%d binding=%d attempt=%d retry=%d", recordCount, bindingCount, attemptCount, retryCount)
	}
}

func TestIngestExternalRecordRejectsForeignIssueWithoutWriting(t *testing.T) {
	ctx := context.Background()
	var foreignWorkspaceID, foreignIssueID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, description, issue_prefix)
		VALUES ('Workbench foreign workspace', 'workbench-foreign', '', 'WBF')
		RETURNING id
	`).Scan(&foreignWorkspaceID); err != nil {
		t.Fatalf("create foreign workspace: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, foreignWorkspaceID)
	})
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, creator_type, creator_id, number)
		VALUES ($1, 'Foreign workbench issue', 'member', $2, 1)
		RETURNING id
	`, foreignWorkspaceID, testUserID).Scan(&foreignIssueID); err != nil {
		t.Fatalf("create foreign issue: %v", err)
	}

	w := httptest.NewRecorder()
	req := newRequest(http.MethodPost, "/api/integrations/ingest", workbenchIngestBody("ferry", "foreign-5663", "Must not persist", "ingest-foreign-5663", foreignIssueID))
	testHandler.IngestExternalRecord(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("foreign issue ingest: expected 404, got %d: %s", w.Code, w.Body.String())
	}

	var records, attempts int
	if err := testPool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM external_record WHERE workspace_id = $1 AND external_id = 'foreign-5663'),
			(SELECT count(*) FROM integration_ingest_attempt WHERE workspace_id = $1 AND idempotency_key = 'ingest-foreign-5663')
	`, testWorkspaceID).Scan(&records, &attempts); err != nil {
		t.Fatalf("count rejected rows: %v", err)
	}
	if records != 0 || attempts != 0 {
		t.Fatalf("foreign issue request wrote rows: records=%d attempts=%d", records, attempts)
	}
}

func createWorkbenchTestIssue(t *testing.T, title string) string {
	t.Helper()
	w := httptest.NewRecorder()
	req := newRequest(http.MethodPost, "/api/issues", map[string]any{"title": title})
	testHandler.CreateIssue(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create workbench issue: %d %s", w.Code, w.Body.String())
	}
	var issue IssueResponse
	if err := json.NewDecoder(w.Body).Decode(&issue); err != nil {
		t.Fatalf("decode workbench issue: %v", err)
	}
	return issue.ID
}

func workbenchIngestBody(sourceType, externalID, title, idempotencyKey, issueID string) map[string]any {
	return map[string]any{
		"source_type":     sourceType,
		"external_id":     externalID,
		"external_key":    externalID,
		"title":           title,
		"summary":         "Synthetic Workbench test record",
		"source_status":   "handled",
		"source_url":      "https://example.invalid/work-orders/" + externalID,
		"schema_version":  "v1",
		"idempotency_key": idempotencyKey,
		"issue_id":        issueID,
	}
}
