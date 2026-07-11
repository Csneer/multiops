package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/auth"
	"github.com/multica-ai/multica/server/internal/middleware"
)

func TestIngestExternalRecordCreatesIssueAndBinding(t *testing.T) {
	body := workbenchIngestBody("ferry", "work-order-create-1", "Created from external record", "ingest-create-1", "")
	delete(body, "issue_id")
	body["create_issue"] = map[string]any{"description": "Normalized description", "priority": "high"}

	w := httptest.NewRecorder()
	testHandler.IngestExternalRecord(w, newRequest(http.MethodPost, "/api/integrations/ingest", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("create ingest: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var response IngestExternalRecordResponse
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.IssueID == nil {
		t.Fatal("created ingest issue_id is nil")
	}
	issueID := *response.IssueID
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID) })

	var title, description, status, priority, creatorType, creatorID string
	var bindings int
	if err := testPool.QueryRow(context.Background(), `
		SELECT i.title, COALESCE(i.description, ''), i.status, i.priority, i.creator_type, i.creator_id,
		       (SELECT count(*) FROM issue_external_record_binding b WHERE b.issue_id = i.id)
		FROM issue i WHERE i.id = $1
	`, issueID).Scan(&title, &description, &status, &priority, &creatorType, &creatorID, &bindings); err != nil {
		t.Fatalf("load created issue: %v", err)
	}
	if title != "Created from external record" || description != "Normalized description" || status != "todo" || priority != "high" || creatorType != "member" || creatorID != testUserID || bindings != 1 {
		t.Fatalf("created issue mismatch title=%q description=%q status=%q priority=%q creator=%s/%s bindings=%d", title, description, status, priority, creatorType, creatorID, bindings)
	}
}

func TestIngestExternalRecordCreateIssueEnqueuesAssignedAgentOnce(t *testing.T) {
	agentID := createHandlerTestAgent(t, "Workbench ingest agent", nil)
	body := workbenchIngestBody("ferry", "work-order-agent-1", "Assigned external issue", "ingest-agent-1", "")
	delete(body, "issue_id")
	body["create_issue"] = map[string]any{"assignee_type": "agent", "assignee_id": agentID}

	w := httptest.NewRecorder()
	testHandler.IngestExternalRecord(w, newRequest(http.MethodPost, "/api/integrations/ingest", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("assigned ingest: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var first IngestExternalRecordResponse
	if err := json.NewDecoder(w.Body).Decode(&first); err != nil || first.IssueID == nil {
		t.Fatalf("decode assigned response: %+v err=%v", first, err)
	}
	issueID := *first.IssueID
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID) })

	var tasks int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issueID, agentID).Scan(&tasks); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if tasks != 1 {
		t.Fatalf("task count = %d, want 1", tasks)
	}

	w = httptest.NewRecorder()
	testHandler.IngestExternalRecord(w, newRequest(http.MethodPost, "/api/integrations/ingest", body))
	if w.Code != http.StatusOK {
		t.Fatalf("duplicate assigned ingest: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var duplicate IngestExternalRecordResponse
	if err := json.NewDecoder(w.Body).Decode(&duplicate); err != nil {
		t.Fatalf("decode duplicate: %v", err)
	}
	if duplicate.Outcome != "duplicate" || duplicate.IssueID == nil || *duplicate.IssueID != issueID {
		t.Fatalf("duplicate response = %+v", duplicate)
	}
	var issues int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM issue WHERE workspace_id = $1 AND title = 'Assigned external issue'`, testWorkspaceID).Scan(&issues); err != nil {
		t.Fatalf("count issues: %v", err)
	}
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2`, issueID, agentID).Scan(&tasks); err != nil {
		t.Fatalf("recount tasks: %v", err)
	}
	if issues != 1 || tasks != 1 {
		t.Fatalf("duplicate created extra rows: issues=%d tasks=%d", issues, tasks)
	}
}

func TestIngestExternalRecordRejectsOverlongCreateIssueDescriptionWithoutWriting(t *testing.T) {
	body := workbenchIngestBody("ferry", "work-order-long-description", "Must not persist", "ingest-long-description", "")
	delete(body, "issue_id")
	body["create_issue"] = map[string]any{"description": string(make([]byte, maxWorkbenchTextLength+1))}

	w := httptest.NewRecorder()
	testHandler.IngestExternalRecord(w, newRequest(http.MethodPost, "/api/integrations/ingest", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("overlong description: expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var records, attempts, issues int
	if err := testPool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM external_record WHERE workspace_id=$1 AND external_id='work-order-long-description'),
		(SELECT count(*) FROM integration_ingest_attempt WHERE workspace_id=$1 AND idempotency_key='ingest-long-description'),
		(SELECT count(*) FROM issue WHERE workspace_id=$1 AND title='Must not persist')`, testWorkspaceID).Scan(&records, &attempts, &issues); err != nil {
		t.Fatalf("count rejected rows: %v", err)
	}
	if records != 0 || attempts != 0 || issues != 0 {
		t.Fatalf("overlong description wrote rows records=%d attempts=%d issues=%d", records, attempts, issues)
	}
}

func TestIngestExternalRecordDuplicateCompensatesMissingInitialTaskOnce(t *testing.T) {
	agentID := createHandlerTestAgent(t, "Workbench compensation agent", nil)
	body := workbenchIngestBody("ferry", "work-order-compensate", "Compensated external issue", "ingest-compensate", "")
	delete(body, "issue_id")
	body["create_issue"] = map[string]any{"assignee_type": "agent", "assignee_id": agentID}

	w := httptest.NewRecorder()
	testHandler.IngestExternalRecord(w, newRequest(http.MethodPost, "/api/integrations/ingest", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("initial ingest: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var first IngestExternalRecordResponse
	if err := json.NewDecoder(w.Body).Decode(&first); err != nil || first.IssueID == nil {
		t.Fatalf("decode initial response: %+v err=%v", first, err)
	}
	issueID := *first.IssueID
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID) })
	if _, err := testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID); err != nil {
		t.Fatalf("delete initial task: %v", err)
	}

	for retry := 1; retry <= 2; retry++ {
		w = httptest.NewRecorder()
		testHandler.IngestExternalRecord(w, newRequest(http.MethodPost, "/api/integrations/ingest", body))
		if w.Code != http.StatusOK {
			t.Fatalf("duplicate %d: expected 200, got %d: %s", retry, w.Code, w.Body.String())
		}
		var tasks int
		if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, issueID, agentID).Scan(&tasks); err != nil {
			t.Fatalf("count tasks after duplicate %d: %v", retry, err)
		}
		if tasks != 1 {
			t.Fatalf("task count after duplicate %d = %d, want 1", retry, tasks)
		}
	}
}

func TestIngestExternalRecordConcurrentDuplicateCompensationCreatesOneTask(t *testing.T) {
	agentID := createHandlerTestAgent(t, "Workbench concurrent compensation agent", nil)
	body := workbenchIngestBody("ferry", "work-order-concurrent-compensate", "Concurrent compensated issue", "ingest-concurrent-compensate", "")
	delete(body, "issue_id")
	body["create_issue"] = map[string]any{"assignee_type": "agent", "assignee_id": agentID}

	w := httptest.NewRecorder()
	testHandler.IngestExternalRecord(w, newRequest(http.MethodPost, "/api/integrations/ingest", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("initial ingest: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var first IngestExternalRecordResponse
	if err := json.NewDecoder(w.Body).Decode(&first); err != nil || first.IssueID == nil {
		t.Fatalf("decode initial response: %+v err=%v", first, err)
	}
	issueID := *first.IssueID
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID) })
	if _, err := testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID); err != nil {
		t.Fatalf("delete initial task: %v", err)
	}

	const retries = 4
	start := make(chan struct{})
	codes := make(chan int, retries)
	var wg sync.WaitGroup
	for i := 0; i < retries; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			response := httptest.NewRecorder()
			testHandler.IngestExternalRecord(response, newRequest(http.MethodPost, "/api/integrations/ingest", body))
			codes <- response.Code
		}()
	}
	close(start)
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Fatalf("concurrent duplicate status = %d, want 200", code)
		}
	}
	var tasks int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND agent_id=$2`, issueID, agentID).Scan(&tasks); err != nil {
		t.Fatalf("count concurrent compensation tasks: %v", err)
	}
	if tasks != 1 {
		t.Fatalf("concurrent compensation task count = %d, want 1", tasks)
	}
}

func TestIngestExternalRecordRejectsInvalidAssigneeWithoutWriting(t *testing.T) {
	body := workbenchIngestBody("ferry", "work-order-invalid-assignee", "Must roll back", "ingest-invalid-assignee", "")
	delete(body, "issue_id")
	body["create_issue"] = map[string]any{"assignee_type": "agent", "assignee_id": "11111111-1111-1111-1111-111111111111"}
	w := httptest.NewRecorder()
	testHandler.IngestExternalRecord(w, newRequest(http.MethodPost, "/api/integrations/ingest", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid assignee: expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var records, attempts, issues int
	if err := testPool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM external_record WHERE workspace_id=$1 AND external_id='work-order-invalid-assignee'),
		(SELECT count(*) FROM integration_ingest_attempt WHERE workspace_id=$1 AND idempotency_key='ingest-invalid-assignee'),
		(SELECT count(*) FROM issue WHERE workspace_id=$1 AND title='Must roll back')`, testWorkspaceID).Scan(&records, &attempts, &issues); err != nil {
		t.Fatalf("count rejected rows: %v", err)
	}
	if records != 0 || attempts != 0 || issues != 0 {
		t.Fatalf("invalid assignee wrote rows records=%d attempts=%d issues=%d", records, attempts, issues)
	}
}

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
	if _, err := testPool.Exec(context.Background(), `
		UPDATE integration_ingest_attempt
		SET request_fingerprint = ''
		WHERE workspace_id = $1 AND source_type = 'ferry' AND idempotency_key = 'ingest-5662-a'
	`, testWorkspaceID); err != nil {
		t.Fatalf("simulate legacy ingest attempt: %v", err)
	}
	w = httptest.NewRecorder()
	req = newRequest(http.MethodPost, "/api/integrations/ingest", first)
	testHandler.IngestExternalRecord(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("legacy duplicate ingest: expected 200, got %d: %s", w.Code, w.Body.String())
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
	if recordCount != 1 || bindingCount != 1 || attemptCount != 1 || retryCount != 3 {
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

func createWorkbenchConnector(t *testing.T, key, connectorType string) string {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.CreateConnector(w, newRequest(http.MethodPost, "/api/connectors", map[string]any{"key": key, "name": key, "type": connectorType}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create connector: %d %s", w.Code, w.Body.String())
	}
	var row struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(w.Body).Decode(&row); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM connector_instance WHERE id=$1`, row.ID) })
	return row.ID
}

func createWorkbenchTemplate(t *testing.T, connectorID, key string, priority int, output map[string]any, match map[string]any) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.CreateIssueTemplate(w, newRequest(http.MethodPost, "/api/issue-templates", map[string]any{"connector_id": connectorID, "template_key": key, "name": key, "priority": priority, "match": match, "output": output}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create template: %d %s", w.Code, w.Body.String())
	}
	var row map[string]any
	if err := json.NewDecoder(w.Body).Decode(&row); err != nil {
		t.Fatal(err)
	}
	return row
}

func TestWorkbenchConnectorAndTemplateVersions(t *testing.T) {
	connectorID := createWorkbenchConnector(t, "versions", "ferry")
	first := createWorkbenchTemplate(t, connectorID, "route", 1, map[string]any{"status": "todo"}, map[string]any{})
	second := createWorkbenchTemplate(t, connectorID, "route", 2, map[string]any{"status": "todo"}, map[string]any{})
	if first["version"] != float64(1) || second["version"] != float64(2) {
		t.Fatalf("versions: %#v %#v", first, second)
	}
	var enabled int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM issue_template WHERE connector_id=$1 AND template_key='route' AND enabled`, connectorID).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 {
		t.Fatalf("enabled versions=%d", enabled)
	}
}

func TestWorkbenchConnectorIngestRoutesAndDuplicates(t *testing.T) {
	agentID := createHandlerTestAgent(t, "Workbench routed agent", nil)
	connectorID := createWorkbenchConnector(t, "routing", "ferry")
	createWorkbenchTemplate(t, connectorID, "fallback", 1, map[string]any{"title_prefix": "Low: ", "status": "todo"}, map[string]any{})
	template := createWorkbenchTemplate(t, connectorID, "matched", 10, map[string]any{"title_prefix": "Routed: ", "description_source": "summary", "status": "in_progress", "priority": "high", "assignee_type": "agent", "assignee_id": agentID}, map[string]any{"source_status": "ready", "labels_any": []string{"urgent"}, "fields": map[string]any{"kind": "work"}})
	body := workbenchIngestBody("ferry", "routed-1", "External title", "routed-key", "")
	delete(body, "issue_id")
	body["connector_id"] = connectorID
	body["labels"] = []string{" urgent ", "urgent"}
	body["fields"] = map[string]any{"kind": "work", "count": 2}
	body["source_status"] = "ready"
	w := httptest.NewRecorder()
	testHandler.IngestExternalRecord(w, newRequest(http.MethodPost, "/api/integrations/ingest", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("ingest: %d %s", w.Code, w.Body.String())
	}
	var response IngestExternalRecordResponse
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.IssueID == nil || response.IssueTemplateID == nil || *response.IssueTemplateID != template["id"] || response.IssueTemplateVersion == nil || *response.IssueTemplateVersion != 1 {
		t.Fatalf("response=%+v template=%v", response, template)
	}
	issueID := *response.IssueID
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM issue WHERE id=$1`, issueID) })
	var title, description, status, priority string
	var tasks int
	if err := testPool.QueryRow(context.Background(), `SELECT title,description,status,priority,(SELECT count(*) FROM agent_task_queue WHERE issue_id=i.id) FROM issue i WHERE id=$1`, issueID).Scan(&title, &description, &status, &priority, &tasks); err != nil {
		t.Fatal(err)
	}
	if title != "Routed: External title" || description != "Synthetic Workbench test record" || status != "in_progress" || priority != "high" || tasks != 1 {
		t.Fatalf("issue=%q %q %q %q tasks=%d", title, description, status, priority, tasks)
	}
	w = httptest.NewRecorder()
	testHandler.IngestExternalRecord(w, newRequest(http.MethodPost, "/api/integrations/ingest", body))
	if w.Code != http.StatusOK {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body.String())
	}
	var duplicate IngestExternalRecordResponse
	json.NewDecoder(w.Body).Decode(&duplicate)
	if duplicate.IssueTemplateID == nil || *duplicate.IssueTemplateID != *response.IssueTemplateID {
		t.Fatalf("duplicate=%+v", duplicate)
	}
}

func TestWorkbenchConnectorIdentityIsolationAndIdempotencyConflict(t *testing.T) {
	firstConnector := createWorkbenchConnector(t, "identity-first", "ferry")
	secondConnector := createWorkbenchConnector(t, "identity-second", "ferry")
	createWorkbenchTemplate(t, firstConnector, "route", 1, map[string]any{"status": "todo"}, map[string]any{})
	createWorkbenchTemplate(t, secondConnector, "route", 1, map[string]any{"status": "todo"}, map[string]any{})

	ingest := func(connectorID, externalID string) *httptest.ResponseRecorder {
		body := workbenchIngestBody("ferry", externalID, "Connector identity", "shared-key", "")
		delete(body, "issue_id")
		body["connector_id"] = connectorID
		w := httptest.NewRecorder()
		testHandler.IngestExternalRecord(w, newRequest(http.MethodPost, "/api/integrations/ingest", body))
		return w
	}

	first := ingest(firstConnector, "shared-record")
	if first.Code != http.StatusCreated {
		t.Fatalf("first ingest: %d %s", first.Code, first.Body.String())
	}
	second := ingest(secondConnector, "shared-record")
	if second.Code != http.StatusCreated {
		t.Fatalf("second ingest: %d %s", second.Code, second.Body.String())
	}
	conflict := ingest(firstConnector, "different-record")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("idempotency conflict: %d %s", conflict.Code, conflict.Body.String())
	}
	observedBody := workbenchIngestBody("ferry", "shared-record", "Connector identity", "shared-key", "")
	delete(observedBody, "issue_id")
	observedBody["connector_id"] = firstConnector
	observedBody["observed_at"] = "2026-07-11T12:00:00Z"
	observedConflict := httptest.NewRecorder()
	testHandler.IngestExternalRecord(observedConflict, newRequest(http.MethodPost, "/api/integrations/ingest", observedBody))
	if observedConflict.Code != http.StatusConflict {
		t.Fatalf("observed_at conflict: %d %s", observedConflict.Code, observedConflict.Body.String())
	}

	var records, attempts int
	if err := testPool.QueryRow(context.Background(), `
		SELECT
			(SELECT count(*) FROM external_record WHERE source_type='ferry' AND external_id='shared-record' AND connector_id IN ($1, $2)),
			(SELECT count(*) FROM integration_ingest_attempt WHERE source_type='ferry' AND idempotency_key='shared-key' AND connector_id IN ($1, $2))
	`, firstConnector, secondConnector).Scan(&records, &attempts); err != nil {
		t.Fatal(err)
	}
	if records != 2 || attempts != 2 {
		t.Fatalf("records=%d attempts=%d", records, attempts)
	}
}

func createWorkbenchCredential(t *testing.T, connectorID string) ConnectorCredentialResponse {
	t.Helper()
	w := httptest.NewRecorder()
	req := newRequest(http.MethodPost, "/api/connectors/"+connectorID+"/credentials", map[string]any{"name": "inbound"})
	req = withURLParam(req, "connectorId", connectorID)
	testHandler.CreateConnectorCredential(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create credential: %d %s", w.Code, w.Body.String())
	}
	var response ConnectorCredentialResponse
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	return response
}

func connectorMachineRouter() http.Handler {
	r := chi.NewRouter()
	r.With(middleware.ConnectorAuth(testHandler.Queries)).Post("/api/integrations/connector-ingest", testHandler.ConnectorIngestExternalRecord)
	return r
}

func machineIngestRequest(token string, body map[string]any) *http.Request {
	req := newRequest(http.MethodPost, "/api/integrations/connector-ingest", body)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func TestConnectorCredentialStoresHashOnlyAndMachineIngestOverwritesIdentity(t *testing.T) {
	connectorID := createWorkbenchConnector(t, "machine-auth", "ferry")
	createWorkbenchTemplate(t, connectorID, "route", 1, map[string]any{"status": "todo"}, map[string]any{})
	credential := createWorkbenchCredential(t, connectorID)
	if !strings.HasPrefix(credential.Token, "mci_") || len(credential.Token) != 44 {
		t.Fatalf("raw token format = %q", credential.Token)
	}
	var storedHash, prefix string
	if err := testPool.QueryRow(context.Background(), `SELECT token_hash, token_prefix FROM connector_credential WHERE id=$1`, credential.ID).Scan(&storedHash, &prefix); err != nil {
		t.Fatal(err)
	}
	if storedHash != auth.HashToken(credential.Token) || storedHash == credential.Token || prefix != credential.Token[:12] {
		t.Fatalf("stored hash/prefix mismatch")
	}

	body := workbenchIngestBody("ferry", "machine-valid", "Machine valid", "machine-valid-key", "")
	delete(body, "issue_id")
	req := machineIngestRequest(credential.Token, body)
	req.Header.Set("X-User-ID", "forged-user")
	req.Header.Set("X-Workspace-ID", "forged-workspace")
	req.Header.Set("X-Connector-ID", "forged-connector")
	req.Header.Set("X-Actor-Source", "human")
	w := httptest.NewRecorder()
	connectorMachineRouter().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("machine ingest: %d %s", w.Code, w.Body.String())
	}
	var response IngestExternalRecordResponse
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil || response.ConnectorID == nil || *response.ConnectorID != connectorID {
		t.Fatalf("machine response=%+v err=%v", response, err)
	}
	var creatorType, creatorID string
	var lastUsedAt any
	if err := testPool.QueryRow(context.Background(), `SELECT creator_type, creator_id FROM issue WHERE id=$1`, *response.IssueID).Scan(&creatorType, &creatorID); err != nil {
		t.Fatal(err)
	}
	if creatorType != "member" || creatorID != workbenchSystemActorID {
		t.Fatalf("machine creator=%s/%s", creatorType, creatorID)
	}
	if err := testPool.QueryRow(context.Background(), `SELECT last_used_at FROM connector_credential WHERE id=$1`, credential.ID).Scan(&lastUsedAt); err != nil || lastUsedAt == nil {
		t.Fatalf("last_used_at=%v err=%v", lastUsedAt, err)
	}
}

func TestConnectorCredentialRevokeDisableAndRotation(t *testing.T) {
	connectorID := createWorkbenchConnector(t, "machine-lifecycle", "ferry")
	createWorkbenchTemplate(t, connectorID, "route", 1, map[string]any{"status": "todo"}, map[string]any{})
	old := createWorkbenchCredential(t, connectorID)

	rotate := httptest.NewRecorder()
	req := newRequest(http.MethodPost, "/api/connectors/"+connectorID+"/credentials/"+old.ID+"/rotate", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("connectorId", connectorID)
	rctx.URLParams.Add("credentialId", old.ID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	testHandler.RotateConnectorCredential(rotate, req)
	if rotate.Code != http.StatusCreated {
		t.Fatalf("rotate: %d %s", rotate.Code, rotate.Body.String())
	}
	var fresh ConnectorCredentialResponse
	if err := json.NewDecoder(rotate.Body).Decode(&fresh); err != nil {
		t.Fatal(err)
	}

	body := workbenchIngestBody("ferry", "rotated-old", "Old rejected", "rotated-old-key", "")
	delete(body, "issue_id")
	w := httptest.NewRecorder()
	connectorMachineRouter().ServeHTTP(w, machineIngestRequest(old.Token, body))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("old token status=%d", w.Code)
	}
	body["external_id"] = "rotated-new"
	body["idempotency_key"] = "rotated-new-key"
	w = httptest.NewRecorder()
	connectorMachineRouter().ServeHTTP(w, machineIngestRequest(fresh.Token, body))
	if w.Code != http.StatusCreated {
		t.Fatalf("new token status=%d body=%s", w.Code, w.Body.String())
	}

	if _, err := testPool.Exec(context.Background(), `UPDATE connector_instance SET enabled=false WHERE id=$1`, connectorID); err != nil {
		t.Fatal(err)
	}
	body["external_id"] = "disabled"
	body["idempotency_key"] = "disabled-key"
	w = httptest.NewRecorder()
	connectorMachineRouter().ServeHTTP(w, machineIngestRequest(fresh.Token, body))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("disabled status=%d", w.Code)
	}

	var records int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM external_record WHERE external_id IN ('rotated-old','disabled')`).Scan(&records); err != nil {
		t.Fatal(err)
	}
	if records != 0 {
		t.Fatalf("auth failures created %d records", records)
	}
}

func TestConnectorCredentialRejectsOverlongName(t *testing.T) {
	connectorID := createWorkbenchConnector(t, "credential-name-limit", "ferry")
	w := httptest.NewRecorder()
	req := newRequest(http.MethodPost, "/api/connectors/"+connectorID+"/credentials", map[string]any{"name": strings.Repeat("x", maxWorkbenchTextLength+1)})
	req = withURLParam(req, "connectorId", connectorID)
	testHandler.CreateConnectorCredential(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("overlong name status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestConnectorCredentialMemberCannotManage(t *testing.T) {
	connectorID := createWorkbenchConnector(t, "member-denied", "ferry")
	var userID string
	if err := testPool.QueryRow(context.Background(), `INSERT INTO "user"(name,email) VALUES ('Credential Member', 'credential-member-' || gen_random_uuid() || '@example.invalid') RETURNING id`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(context.Background(), `INSERT INTO member(workspace_id,user_id,role) VALUES ($1,$2,'member')`, testWorkspaceID, userID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id=$1`, userID) })

	w := httptest.NewRecorder()
	req := newRequestAs(userID, http.MethodPost, "/api/connectors/"+connectorID+"/credentials", map[string]any{"name": "forbidden"})
	req = withURLParam(req, "connectorId", connectorID)
	testHandler.CreateConnectorCredential(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member create status=%d body=%s", w.Code, w.Body.String())
	}
	var count int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM connector_credential WHERE connector_id=$1`, connectorID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("member created %d credentials", count)
	}
}

func TestConnectorCredentialCrossWorkspaceManagementDenied(t *testing.T) {
	connectorID := createWorkbenchConnector(t, "cross-workspace", "ferry")
	var foreignWorkspace string
	if err := testPool.QueryRow(context.Background(), `INSERT INTO workspace(name,slug,description,issue_prefix) VALUES ('Connector foreign','connector-foreign','','COF') RETURNING id`).Scan(&foreignWorkspace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id=$1`, foreignWorkspace) })
	w := httptest.NewRecorder()
	req := newRequest(http.MethodPost, "/api/connectors/"+connectorID+"/credentials", map[string]any{"name": "forbidden"})
	req.Header.Set("X-Workspace-ID", foreignWorkspace)
	req = withURLParam(req, "connectorId", connectorID)
	testHandler.CreateConnectorCredential(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross workspace status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestWorkbenchConnectorManagementAndRoutingPreview(t *testing.T) {
	agentID := createHandlerTestAgent(t, "Workbench preview agent", nil)
	connectorID := createWorkbenchConnector(t, "management-preview", "ferry")
	createWorkbenchTemplate(t, connectorID, "beta", 1, map[string]any{"status": "todo"}, map[string]any{})
	createWorkbenchTemplate(t, connectorID, "alpha", 5, map[string]any{"title_prefix": "Old: ", "status": "todo"}, map[string]any{})
	active := createWorkbenchTemplate(t, connectorID, "alpha", 10, map[string]any{"title_prefix": "Preview: ", "description_source": "summary", "status": "in_progress", "priority": "high", "assignee_type": "agent", "assignee_id": agentID, "auto_start": false}, map[string]any{"source_status": "ready", "labels_any": []string{"urgent"}, "fields": map[string]any{"kind": "work"}})

	list := httptest.NewRecorder()
	testHandler.ListConnectors(list, newRequest(http.MethodGet, "/api/connectors", nil))
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), "config") {
		t.Fatalf("list connectors: %d %s", list.Code, list.Body.String())
	}
	var listed struct {
		Connectors []ConnectorResponse `json:"connectors"`
	}
	if err := json.NewDecoder(list.Body).Decode(&listed); err != nil || len(listed.Connectors) == 0 {
		t.Fatalf("decode connectors: %+v err=%v", listed, err)
	}

	detail := httptest.NewRecorder()
	req := withURLParam(newRequest(http.MethodGet, "/api/connectors/"+connectorID, nil), "connectorId", connectorID)
	testHandler.GetConnector(detail, req)
	if detail.Code != http.StatusOK || strings.Contains(detail.Body.String(), "config") {
		t.Fatalf("connector detail: %d %s", detail.Code, detail.Body.String())
	}

	history := httptest.NewRecorder()
	req = withURLParam(newRequest(http.MethodGet, "/api/connectors/"+connectorID+"/issue-templates", nil), "connectorId", connectorID)
	testHandler.ListConnectorIssueTemplateHistory(history, req)
	var historyBody struct {
		Templates []IssueTemplateResponse `json:"issue_templates"`
	}
	if history.Code != http.StatusOK {
		t.Fatalf("history: %d %s", history.Code, history.Body.String())
	}
	if err := json.NewDecoder(history.Body).Decode(&historyBody); err != nil || len(historyBody.Templates) != 3 {
		t.Fatalf("history decode: %+v err=%v", historyBody, err)
	}
	if historyBody.Templates[0].TemplateKey != "alpha" || historyBody.Templates[0].Version != 2 || historyBody.Templates[1].Version != 1 || historyBody.Templates[2].TemplateKey != "beta" {
		t.Fatalf("history order: %+v", historyBody.Templates)
	}

	activeList := httptest.NewRecorder()
	req = withURLParam(newRequest(http.MethodGet, "/api/connectors/"+connectorID+"/issue-templates/active", nil), "connectorId", connectorID)
	testHandler.ListConnectorActiveIssueTemplates(activeList, req)
	var activeBody struct {
		Templates []IssueTemplateResponse `json:"issue_templates"`
	}
	json.NewDecoder(activeList.Body).Decode(&activeBody)
	if activeList.Code != http.StatusOK || len(activeBody.Templates) != 2 || activeBody.Templates[0].TemplateKey != "alpha" || activeBody.Templates[0].ID != active["id"] {
		t.Fatalf("active templates: %d %+v", activeList.Code, activeBody.Templates)
	}

	before := workbenchPreviewRowCounts(t, connectorID)
	preview := httptest.NewRecorder()
	req = withURLParam(newRequest(http.MethodPost, "/api/connectors/"+connectorID+"/routing-preview", map[string]any{"source_type": "ferry", "source_status": "ready", "labels": []string{" urgent ", "urgent"}, "fields": map[string]any{"kind": "work", "count": 2}, "title": "External title", "summary": "External summary"}), "connectorId", connectorID)
	testHandler.PreviewConnectorRouting(preview, req)
	if preview.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", preview.Code, preview.Body.String())
	}
	var result RoutingPreviewResponse
	if err := json.NewDecoder(preview.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Connector.ID != connectorID || result.IssueTemplate.ID != active["id"] || result.Issue.Title != "Preview: External title" || result.Issue.Description == nil || *result.Issue.Description != "External summary" || result.Issue.Status != "backlog" || result.Issue.Priority != "high" || result.Issue.AssigneeID == nil || *result.Issue.AssigneeID != agentID || result.Issue.AutoStart {
		t.Fatalf("preview result: %+v", result)
	}
	after := workbenchPreviewRowCounts(t, connectorID)
	if before != after {
		t.Fatalf("preview wrote rows: before=%v after=%v", before, after)
	}
}

func TestWorkbenchRoutingPreviewValidationIsolationAndDisabled(t *testing.T) {
	connectorID := createWorkbenchConnector(t, "preview-validation", "ferry")
	createWorkbenchTemplate(t, connectorID, "only", 1, map[string]any{"status": "todo"}, map[string]any{"source_status": "ready"})

	cases := []struct {
		name string
		body map[string]any
		code int
	}{
		{"no match", map[string]any{"source_type": "ferry", "source_status": "other", "title": "x"}, http.StatusUnprocessableEntity},
		{"wrong source", map[string]any{"source_type": "other", "title": "x"}, http.StatusBadRequest},
		{"nested fields", map[string]any{"source_type": "ferry", "title": "x", "fields": map[string]any{"nested": map[string]any{"x": 1}}}, http.StatusBadRequest},
		{"unknown field", map[string]any{"source_type": "ferry", "title": "x", "external_id": "forbidden"}, http.StatusBadRequest},
		{"missing title", map[string]any{"source_type": "ferry"}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := withURLParam(newRequest(http.MethodPost, "/api/connectors/"+connectorID+"/routing-preview", tc.body), "connectorId", connectorID)
			testHandler.PreviewConnectorRouting(w, req)
			if w.Code != tc.code {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}

	var foreignWorkspace string
	if err := testPool.QueryRow(context.Background(), `INSERT INTO workspace(name,slug,description,issue_prefix) VALUES ('Preview foreign','preview-foreign-' || gen_random_uuid(),'','PVF') RETURNING id`).Scan(&foreignWorkspace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id=$1`, foreignWorkspace) })
	w := httptest.NewRecorder()
	req := withURLParam(newRequest(http.MethodGet, "/api/connectors/"+connectorID, nil), "connectorId", connectorID)
	req.Header.Set("X-Workspace-ID", foreignWorkspace)
	testHandler.GetConnector(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("foreign detail status=%d body=%s", w.Code, w.Body.String())
	}

	if _, err := testPool.Exec(context.Background(), `UPDATE connector_instance SET enabled=false WHERE id=$1`, connectorID); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	req = withURLParam(newRequest(http.MethodPost, "/api/connectors/"+connectorID+"/routing-preview", map[string]any{"source_type": "ferry", "source_status": "ready", "title": "x"}), "connectorId", connectorID)
	testHandler.PreviewConnectorRouting(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled preview status=%d body=%s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	req = withURLParam(newRequest(http.MethodGet, "/api/connectors/"+connectorID, nil), "connectorId", connectorID)
	testHandler.GetConnector(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("disabled detail status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestWorkbenchDisableConnectorPermissionsAndIdempotency(t *testing.T) {
	connectorID := createWorkbenchConnector(t, "disable-permissions", "ferry")
	var memberID string
	if err := testPool.QueryRow(context.Background(), `INSERT INTO "user"(name,email) VALUES ('Disable Member','disable-member-' || gen_random_uuid() || '@example.invalid') RETURNING id`).Scan(&memberID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(context.Background(), `INSERT INTO member(workspace_id,user_id,role) VALUES ($1,$2,'member')`, testWorkspaceID, memberID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id=$1`, memberID) })

	w := httptest.NewRecorder()
	req := withURLParam(newRequestAs(memberID, http.MethodPost, "/api/connectors/"+connectorID+"/disable", nil), "connectorId", connectorID)
	testHandler.DisableConnector(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member disable status=%d body=%s", w.Code, w.Body.String())
	}
	var enabled bool
	testPool.QueryRow(context.Background(), `SELECT enabled FROM connector_instance WHERE id=$1`, connectorID).Scan(&enabled)
	if !enabled {
		t.Fatal("member disabled connector")
	}
	for i := 0; i < 2; i++ {
		w = httptest.NewRecorder()
		req = withURLParam(newRequest(http.MethodPost, "/api/connectors/"+connectorID+"/disable", nil), "connectorId", connectorID)
		testHandler.DisableConnector(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("owner disable %d status=%d body=%s", i, w.Code, w.Body.String())
		}
		var response ConnectorResponse
		json.NewDecoder(w.Body).Decode(&response)
		if response.Enabled {
			t.Fatalf("disable %d returned enabled", i)
		}
	}
}

type workbenchRowCounts struct{ Attempts, Records, Issues, Bindings, Tasks int }

func workbenchPreviewRowCounts(t *testing.T, connectorID string) workbenchRowCounts {
	t.Helper()
	var result workbenchRowCounts
	if err := testPool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM integration_ingest_attempt WHERE connector_id=$1),
		(SELECT count(*) FROM external_record WHERE connector_id=$1),
		(SELECT count(*) FROM issue WHERE workspace_id=$2),
		(SELECT count(*) FROM issue_external_record_binding WHERE workspace_id=$2),
		(SELECT count(*) FROM agent_task_queue t JOIN issue i ON i.id=t.issue_id WHERE i.workspace_id=$2)`, connectorID, testWorkspaceID).Scan(&result.Attempts, &result.Records, &result.Issues, &result.Bindings, &result.Tasks); err != nil {
		t.Fatal(err)
	}
	return result
}

func createWorkbenchMember(t *testing.T, role string) string {
	t.Helper()
	var userID string
	if err := testPool.QueryRow(context.Background(), `INSERT INTO "user"(name,email) VALUES ('Workbench Member', 'workbench-member-' || gen_random_uuid() || '@example.invalid') RETURNING id`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(context.Background(), `INSERT INTO member(workspace_id,user_id,role) VALUES ($1,$2,$3)`, testWorkspaceID, userID, role); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id=$1`, userID) })
	return userID
}

func TestWorkbenchMemberCannotCreateConnectorOrTemplate(t *testing.T) {
	memberID := createWorkbenchMember(t, "member")
	connectorKey := "member-create-denied"
	w := httptest.NewRecorder()
	testHandler.CreateConnector(w, newRequestAs(memberID, http.MethodPost, "/api/connectors", map[string]any{"key": connectorKey, "name": connectorKey, "type": "ferry", "config": map[string]any{"secret": "must-not-leak"}}))
	if w.Code != http.StatusForbidden {
		t.Fatalf("member connector create status=%d body=%s", w.Code, w.Body.String())
	}
	var connectorCount int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM connector_instance WHERE workspace_id=$1 AND key=$2`, testWorkspaceID, connectorKey).Scan(&connectorCount); err != nil {
		t.Fatal(err)
	}
	if connectorCount != 0 {
		t.Fatalf("member created %d connectors", connectorCount)
	}

	connectorID := createWorkbenchConnector(t, "member-template-denied", "ferry")
	w = httptest.NewRecorder()
	testHandler.CreateIssueTemplate(w, newRequestAs(memberID, http.MethodPost, "/api/issue-templates", map[string]any{"connector_id": connectorID, "template_key": "forbidden", "name": "forbidden", "output": map[string]any{"status": "todo"}}))
	if w.Code != http.StatusForbidden {
		t.Fatalf("member template create status=%d body=%s", w.Code, w.Body.String())
	}
	var templateCount int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM issue_template WHERE connector_id=$1 AND template_key='forbidden'`, connectorID).Scan(&templateCount); err != nil {
		t.Fatal(err)
	}
	if templateCount != 0 {
		t.Fatalf("member created %d templates", templateCount)
	}
}

func TestWebhookConnectorConfigurationRotationEncryptionAndPermissions(t *testing.T) {
	wCreate := httptest.NewRecorder()
	testHandler.CreateConnector(wCreate, newRequest(http.MethodPost, "/api/connectors", map[string]any{"key": "webhook-config", "name": "webhook-config", "type": "webhook", "capabilities": map[string]any{"result_delivery": true}}))
	if wCreate.Code != http.StatusCreated {
		t.Fatalf("create webhook: %d %s", wCreate.Code, wCreate.Body.String())
	}
	var created ConnectorResponse
	if err := json.NewDecoder(wCreate.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	connectorID := created.ID
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM connector_instance WHERE id=$1`, connectorID) })
	configure := func(userID, workspaceID string, body any) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := newRequestAs(userID, http.MethodPut, "/api/connectors/"+connectorID+"/webhook", body)
		req.Header.Set("X-Workspace-ID", workspaceID)
		req = withURLParam(req, "connectorId", connectorID)
		testHandler.ConfigureWebhookConnector(w, req)
		return w
	}
	w := configure(testUserID, testWorkspaceID, map[string]any{"version": 1, "url": "https://hooks.example.com/result", "timeout_ms": 5000})
	if w.Code != http.StatusOK {
		t.Fatalf("configure: %d %s", w.Code, w.Body.String())
	}
	var first WebhookConfigurationResponse
	if err := json.NewDecoder(w.Body).Decode(&first); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first.Secret, "mws_") || first.SecretPrefix != first.Secret[:12] {
		t.Fatalf("secret response=%+v", first)
	}
	var encrypted []byte
	var prefix string
	var config string
	if err := testPool.QueryRow(context.Background(), `SELECT wc.signing_secret_encrypted,wc.signing_secret_prefix,ci.config::text FROM workbench_webhook_connector wc JOIN connector_instance ci ON ci.id=wc.connector_id WHERE wc.connector_id=$1`, connectorID).Scan(&encrypted, &prefix, &config); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encrypted), first.Secret) || prefix != first.SecretPrefix || config != "{}" {
		t.Fatal("plaintext secret/config leaked to storage")
	}
	memberID := createWorkbenchMember(t, "member")
	denied := configure(memberID, testWorkspaceID, map[string]any{"version": 1, "url": "https://hooks.example.com/result", "timeout_ms": 5000})
	if denied.Code != http.StatusForbidden {
		t.Fatalf("member status=%d", denied.Code)
	}
	var foreign string
	if err := testPool.QueryRow(context.Background(), `INSERT INTO workspace(name,slug,description,issue_prefix) VALUES ('Webhook foreign','webhook-foreign-'||gen_random_uuid(),'','WHF') RETURNING id`).Scan(&foreign); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM workspace WHERE id=$1`, foreign) })
	cross := configure(testUserID, foreign, map[string]any{"version": 1, "url": "https://hooks.example.com/result", "timeout_ms": 5000})
	if cross.Code != http.StatusNotFound {
		t.Fatalf("cross status=%d", cross.Code)
	}
	rotate := httptest.NewRecorder()
	req := newRequest(http.MethodPost, "/api/connectors/"+connectorID+"/webhook/rotate-secret", map[string]any{})
	req = withURLParam(req, "connectorId", connectorID)
	testHandler.RotateWebhookConnectorSecret(rotate, req)
	if rotate.Code != http.StatusOK {
		t.Fatalf("rotate=%d %s", rotate.Code, rotate.Body.String())
	}
	var second WebhookConfigurationResponse
	json.NewDecoder(rotate.Body).Decode(&second)
	if second.Secret == first.Secret || strings.Contains(rotate.Body.String(), first.Secret) {
		t.Fatal("rotation did not replace secret safely")
	}
	list := httptest.NewRecorder()
	testHandler.ListConnectors(list, newRequest(http.MethodGet, "/api/connectors", nil))
	if strings.Contains(list.Body.String(), first.Secret) || strings.Contains(list.Body.String(), second.Secret) || strings.Contains(list.Body.String(), "endpoint_url") {
		t.Fatal("public DTO leaked webhook configuration")
	}
}

func TestWorkbenchWebhookConnectorDatabaseConstraints(t *testing.T) {
	w := httptest.NewRecorder()
	testHandler.CreateConnector(w, newRequest(http.MethodPost, "/api/connectors", map[string]any{"key": "webhook-constraints", "name": "webhook-constraints", "type": "webhook", "capabilities": map[string]any{"result_delivery": true}}))
	if w.Code != http.StatusCreated {
		t.Fatalf("create connector: %d %s", w.Code, w.Body.String())
	}
	var connector ConnectorResponse
	if err := json.NewDecoder(w.Body).Decode(&connector); err != nil {
		t.Fatal(err)
	}
	connectorID := connector.ID
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM connector_instance WHERE id=$1`, connectorID) })
	createdBy := parseUUID(testUserID)
	workspaceID := parseUUID(testWorkspaceID)
	connectorUUID := parseUUID(connectorID)
	for _, tc := range []struct {
		name       string
		version    int
		timeout    int
		ciphertext []byte
		prefix     string
	}{
		{name: "empty ciphertext", version: 1, timeout: 5000, ciphertext: []byte{}, prefix: "mws_0123abcd"},
		{name: "short ciphertext", version: 1, timeout: 5000, ciphertext: make([]byte, 28), prefix: "mws_0123abcd"},
		{name: "malformed prefix", version: 1, timeout: 5000, ciphertext: make([]byte, 29), prefix: "mws_0123ABCD"},
		{name: "invalid timeout", version: 1, timeout: 999, ciphertext: make([]byte, 29), prefix: "mws_0123abcd"},
		{name: "invalid version", version: 2, timeout: 5000, ciphertext: make([]byte, 29), prefix: "mws_0123abcd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := testPool.Exec(context.Background(), `INSERT INTO workbench_webhook_connector (connector_id,workspace_id,config_version,endpoint_url,timeout_ms,signing_secret_encrypted,signing_secret_prefix,created_by,updated_by) VALUES ($1,$2,$3,'https://hooks.example.com/result',$4,$5,$6,$7,$7)`, connectorUUID, workspaceID, tc.version, tc.timeout, tc.ciphertext, tc.prefix, createdBy)
			if err == nil {
				t.Fatal("invalid webhook connector insert succeeded")
			}
		})
	}
}

func TestWebhookConfigurationFailsClosedWithoutSecretbox(t *testing.T) {
	wCreate := httptest.NewRecorder()
	testHandler.CreateConnector(wCreate, newRequest(http.MethodPost, "/api/connectors", map[string]any{"key": "webhook-no-box", "name": "webhook-no-box", "type": "webhook", "capabilities": map[string]any{"result_delivery": true}}))
	if wCreate.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", wCreate.Code, wCreate.Body.String())
	}
	var created ConnectorResponse
	json.NewDecoder(wCreate.Body).Decode(&created)
	connectorID := created.ID
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM connector_instance WHERE id=$1`, connectorID) })
	old := testHandler.WorkbenchSecretBox
	testHandler.WorkbenchSecretBox = nil
	t.Cleanup(func() { testHandler.WorkbenchSecretBox = old })
	w := httptest.NewRecorder()
	req := withURLParam(newRequest(http.MethodPut, "/api/connectors/"+connectorID+"/webhook", map[string]any{"version": 1, "url": "https://hooks.example.com/result", "timeout_ms": 5000}), "connectorId", connectorID)
	testHandler.ConfigureWebhookConnector(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestCreateConnectorReturnsSafeDTOAndRejectsTrailingJSON(t *testing.T) {
	body := `{"key":"safe-dto","name":"Safe DTO","type":"ferry","config":{"secret":"hidden"}} trailing`
	req := newRequest(http.MethodPost, "/api/connectors", nil)
	req.Body = io.NopCloser(strings.NewReader(body))
	w := httptest.NewRecorder()
	testHandler.CreateConnector(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("trailing JSON status=%d body=%s", w.Code, w.Body.String())
	}
	var count int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM connector_instance WHERE workspace_id=$1 AND key='safe-dto'`, testWorkspaceID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("trailing JSON created %d connectors", count)
	}

	w = httptest.NewRecorder()
	testHandler.CreateConnector(w, newRequest(http.MethodPost, "/api/connectors", map[string]any{"key": "safe-dto", "name": "Safe DTO", "type": "ferry", "config": map[string]any{"secret": "hidden"}}))
	if w.Code != http.StatusCreated || strings.Contains(w.Body.String(), "config") || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "created_by") {
		t.Fatalf("unsafe connector response status=%d body=%s", w.Code, w.Body.String())
	}
	var response ConnectorResponse
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(context.Background(), `DELETE FROM connector_instance WHERE id=$1`, response.ID) })
}

func TestWorkbenchRenderedTitleLimitMatchesPreviewAndIngestWithoutWrites(t *testing.T) {
	connectorID := createWorkbenchConnector(t, "rendered-title-limit", "ferry")
	createWorkbenchTemplate(t, connectorID, "route", 1, map[string]any{"title_prefix": "prefix", "status": "todo"}, map[string]any{})
	title := strings.Repeat("x", maxWorkbenchTextLength)
	before := workbenchPreviewRowCounts(t, connectorID)
	preview := httptest.NewRecorder()
	req := withURLParam(newRequest(http.MethodPost, "/api/connectors/"+connectorID+"/routing-preview", map[string]any{"source_type": "ferry", "title": title}), "connectorId", connectorID)
	testHandler.PreviewConnectorRouting(preview, req)
	if preview.Code != http.StatusBadRequest {
		t.Fatalf("preview title limit status=%d body=%s", preview.Code, preview.Body.String())
	}
	body := workbenchIngestBody("ferry", "rendered-title-limit", title, "rendered-title-limit", "")
	delete(body, "issue_id")
	body["connector_id"] = connectorID
	ingest := httptest.NewRecorder()
	testHandler.IngestExternalRecord(ingest, newRequest(http.MethodPost, "/api/integrations/ingest", body))
	if ingest.Code != http.StatusBadRequest {
		t.Fatalf("ingest title limit status=%d body=%s", ingest.Code, ingest.Body.String())
	}
	if after := workbenchPreviewRowCounts(t, connectorID); before != after {
		t.Fatalf("rendered title rejection wrote rows before=%v after=%v", before, after)
	}
}

func TestWorkbenchPrivateAgentTemplatePreviewAndMachineIngest(t *testing.T) {
	agentID := createHandlerTestAgent(t, "Workbench private routed agent", nil)
	if _, err := testPool.Exec(context.Background(), `UPDATE agent SET permission_mode='private' WHERE id=$1`, agentID); err != nil {
		t.Fatal(err)
	}
	connectorID := createWorkbenchConnector(t, "private-agent-route", "ferry")
	createWorkbenchTemplate(t, connectorID, "route", 1, map[string]any{"title_prefix": "Private: ", "status": "todo", "assignee_type": "agent", "assignee_id": agentID}, map[string]any{})
	memberID := createWorkbenchMember(t, "member")
	preview := httptest.NewRecorder()
	req := withURLParam(newRequestAs(memberID, http.MethodPost, "/api/connectors/"+connectorID+"/routing-preview", map[string]any{"source_type": "ferry", "title": "same"}), "connectorId", connectorID)
	testHandler.PreviewConnectorRouting(preview, req)
	if preview.Code != http.StatusOK {
		t.Fatalf("member private-agent preview status=%d body=%s", preview.Code, preview.Body.String())
	}
	var previewResponse RoutingPreviewResponse
	if err := json.NewDecoder(preview.Body).Decode(&previewResponse); err != nil {
		t.Fatal(err)
	}
	credential := createWorkbenchCredential(t, connectorID)
	body := workbenchIngestBody("ferry", "private-agent-machine", "same", "private-agent-machine", "")
	delete(body, "issue_id")
	ingest := httptest.NewRecorder()
	connectorMachineRouter().ServeHTTP(ingest, machineIngestRequest(credential.Token, body))
	if ingest.Code != http.StatusCreated {
		t.Fatalf("machine private-agent ingest status=%d body=%s", ingest.Code, ingest.Body.String())
	}
	var ingestResponse IngestExternalRecordResponse
	if err := json.NewDecoder(ingest.Body).Decode(&ingestResponse); err != nil || ingestResponse.IssueID == nil {
		t.Fatalf("machine response=%+v err=%v", ingestResponse, err)
	}
	var title, assignee string
	if err := testPool.QueryRow(context.Background(), `SELECT title,assignee_id FROM issue WHERE id=$1`, *ingestResponse.IssueID).Scan(&title, &assignee); err != nil {
		t.Fatal(err)
	}
	if title != previewResponse.Issue.Title || assignee != agentID {
		t.Fatalf("preview/ingest mismatch preview=%+v title=%q assignee=%q", previewResponse.Issue, title, assignee)
	}
}

func TestWorkbenchConnectorIngestAutoStartFalseAndNoMatchRollback(t *testing.T) {
	agentID := createHandlerTestAgent(t, "Workbench parked agent", nil)
	connectorID := createWorkbenchConnector(t, "parked", "ferry")
	createWorkbenchTemplate(t, connectorID, "park", 1, map[string]any{"status": "in_progress", "auto_start": false, "assignee_type": "agent", "assignee_id": agentID}, map[string]any{"source_status": "park"})
	body := workbenchIngestBody("ferry", "parked-1", "Parked issue", "parked-key", "")
	delete(body, "issue_id")
	body["connector_id"] = connectorID
	body["source_status"] = "park"
	w := httptest.NewRecorder()
	testHandler.IngestExternalRecord(w, newRequest(http.MethodPost, "/api/integrations/ingest", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("park: %d %s", w.Code, w.Body.String())
	}
	var response IngestExternalRecordResponse
	json.NewDecoder(w.Body).Decode(&response)
	var status string
	var tasks int
	if err := testPool.QueryRow(context.Background(), `SELECT status,(SELECT count(*) FROM agent_task_queue WHERE issue_id=i.id) FROM issue i WHERE id=$1`, *response.IssueID).Scan(&status, &tasks); err != nil {
		t.Fatal(err)
	}
	if status != "backlog" || tasks != 0 {
		t.Fatalf("status=%s tasks=%d", status, tasks)
	}
	body = workbenchIngestBody("ferry", "no-match", "No match", "no-match-key", "")
	delete(body, "issue_id")
	body["connector_id"] = connectorID
	body["source_status"] = "other"
	w = httptest.NewRecorder()
	testHandler.IngestExternalRecord(w, newRequest(http.MethodPost, "/api/integrations/ingest", body))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no match: %d %s", w.Code, w.Body.String())
	}
	var records, attempts, issues int
	testPool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM external_record WHERE external_id='no-match'),(SELECT count(*) FROM integration_ingest_attempt WHERE idempotency_key='no-match-key'),(SELECT count(*) FROM issue WHERE title='No match')`).Scan(&records, &attempts, &issues)
	if records+attempts+issues != 0 {
		t.Fatalf("rollback records=%d attempts=%d issues=%d", records, attempts, issues)
	}
}
