package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
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
