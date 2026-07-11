package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type resultFixture struct {
	pool        *pgxpool.Pool
	queries     *db.Queries
	service     *TaskService
	workspaceID string
	connectorID string
	recordID    string
	issueID     string
	taskID      string
	agentID     string
}

func newResultFixture(t *testing.T, bound, chat bool) resultFixture {
	t.Helper()
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	queries := db.New(pool)
	suffix := time.Now().UnixNano()
	var userID, workspaceID, runtimeID, agentID, connectorID, recordID, issueID, taskID string
	mustScan := func(label, sql string, args []any, dest ...any) {
		t.Helper()
		if err := pool.QueryRow(ctx, sql, args...).Scan(dest...); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}
	mustScan("user", `INSERT INTO "user" (name,email) VALUES ('Result Test',$1) RETURNING id`, []any{fmt.Sprintf("result-%d@multica.ai", suffix)}, &userID)
	mustScan("workspace", `INSERT INTO workspace (name,slug,description,issue_prefix) VALUES ('Result Test',$1,'','RTO') RETURNING id`, []any{fmt.Sprintf("result-%d", suffix)}, &workspaceID)
	if _, err := pool.Exec(ctx, `INSERT INTO member (workspace_id,user_id,role) VALUES ($1,$2,'owner')`, workspaceID, userID); err != nil {
		t.Fatal(err)
	}
	mustScan("runtime", `INSERT INTO agent_runtime (workspace_id,name,runtime_mode,provider,status,device_info,metadata,last_seen_at,visibility,owner_id) VALUES ($1,'Result Runtime','cloud','test','online','','{}',now(),'private',$2) RETURNING id`, []any{workspaceID, userID}, &runtimeID)
	mustScan("agent", `INSERT INTO agent (workspace_id,name,description,runtime_mode,runtime_config,runtime_id,visibility,owner_id) VALUES ($1,'Result Agent','','cloud','{}',$2,'private',$3) RETURNING id`, []any{workspaceID, runtimeID, userID}, &agentID)
	mustScan("connector", `INSERT INTO connector_instance (workspace_id,key,name,connector_type,capabilities,created_by) VALUES ($1,$2,'Result Connector','webhook','{"result_delivery":true}',$3) RETURNING id`, []any{workspaceID, fmt.Sprintf("result-%d", suffix), userID}, &connectorID)
	if _, err := pool.Exec(ctx, `INSERT INTO workbench_webhook_connector (connector_id,workspace_id,config_version,endpoint_url,timeout_ms,signing_secret_encrypted,signing_secret_prefix,created_by,updated_by) VALUES ($1,$2,1,'https://hooks.example.com/result',5000,$3,'mws_0123abcd',$4,$4)`, connectorID, workspaceID, make([]byte, 29), userID); err != nil {
		t.Fatal(err)
	}
	mustScan("record", `INSERT INTO external_record (workspace_id,source_type,external_id,title,connector_id) VALUES ($1,'test',$2,'External',$3) RETURNING id`, []any{workspaceID, fmt.Sprintf("ext-%d", suffix), connectorID}, &recordID)
	mustScan("issue", `INSERT INTO issue (workspace_id,title,status,priority,creator_id,creator_type,number,position) VALUES ($1,'Result Issue','in_progress','none',$2,'member',$3,0) RETURNING id`, []any{workspaceID, userID, suffix % 1000000000}, &issueID)
	if bound {
		if _, err := pool.Exec(ctx, `INSERT INTO issue_external_record_binding (workspace_id,issue_id,external_record_id,binding_role) VALUES ($1,$2,$3,'primary')`, workspaceID, issueID, recordID); err != nil {
			t.Fatal(err)
		}
	}
	if chat {
		var chatID string
		mustScan("chat", `INSERT INTO chat_session (workspace_id,agent_id,creator_id,title) VALUES ($1,$2,$3,'Result Chat') RETURNING id`, []any{workspaceID, agentID, userID}, &chatID)
		mustScan("task", `INSERT INTO agent_task_queue (agent_id,chat_session_id,status,priority,runtime_id,started_at) VALUES ($1,$2,'running',0,$3,now()) RETURNING id`, []any{agentID, chatID, runtimeID}, &taskID)
	} else {
		mustScan("task", `INSERT INTO agent_task_queue (agent_id,issue_id,status,priority,runtime_id,started_at) VALUES ($1,$2,'running',0,$3,now()) RETURNING id`, []any{agentID, issueID, runtimeID}, &taskID)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM workbench_result_outbox WHERE workspace_id=$1`, workspaceID)
		pool.Exec(context.Background(), `DELETE FROM workspace WHERE id=$1`, workspaceID)
		pool.Exec(context.Background(), `DELETE FROM "user" WHERE id=$1`, userID)
	})
	return resultFixture{pool: pool, queries: queries, service: NewTaskService(queries, pool, nil, events.New()), workspaceID: workspaceID, connectorID: connectorID, recordID: recordID, issueID: issueID, taskID: taskID, agentID: agentID}
}

func TestCompleteTaskTransactionallyEnqueuesWorkbenchResult(t *testing.T) {
	f := newResultFixture(t, true, false)
	result := []byte(`{"output":""}`)
	if _, err := f.service.CompleteTask(context.Background(), util.MustParseUUID(f.taskID), result, "", ""); err != nil {
		t.Fatal(err)
	}
	var status, key string
	var payload []byte
	if err := f.pool.QueryRow(context.Background(), `SELECT status,idempotency_key,payload FROM workbench_result_outbox WHERE task_id=$1`, f.taskID).Scan(&status, &key, &payload); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || key != "workbench-result:v1:"+f.taskID+":completed" {
		t.Fatalf("unexpected outbox status/key: %s %s", status, key)
	}
	if len(payload) == 0 {
		t.Fatal("empty payload")
	}
	if _, err := f.service.CompleteTask(context.Background(), util.MustParseUUID(f.taskID), result, "", ""); err != nil {
		t.Fatal(err)
	}
	var count int
	f.pool.QueryRow(context.Background(), `SELECT count(*) FROM workbench_result_outbox WHERE task_id=$1`, f.taskID).Scan(&count)
	if count != 1 {
		t.Fatalf("duplicate completion created %d rows", count)
	}
}

func TestCompleteTaskRollsBackWhenOutboxEnqueueFails(t *testing.T) {
	f := newResultFixture(t, true, false)
	_, err := f.pool.Exec(context.Background(), `CREATE FUNCTION fail_result_outbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced outbox failure'; END $$`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.pool.Exec(context.Background(), `CREATE TRIGGER fail_result_outbox BEFORE INSERT ON workbench_result_outbox FOR EACH ROW EXECUTE FUNCTION fail_result_outbox()`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS fail_result_outbox ON workbench_result_outbox`)
		f.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS fail_result_outbox()`)
	})
	if _, err := f.service.CompleteTask(context.Background(), util.MustParseUUID(f.taskID), []byte(`{}`), "", ""); err == nil {
		t.Fatal("expected enqueue failure")
	}
	var status string
	f.pool.QueryRow(context.Background(), `SELECT status FROM agent_task_queue WHERE id=$1`, f.taskID).Scan(&status)
	if status != "running" {
		t.Fatalf("task update did not roll back: %s", status)
	}
}

func TestFailTaskRollsBackWhenOutboxEnqueueFails(t *testing.T) {
	f := newResultFixture(t, true, false)
	if _, err := f.pool.Exec(context.Background(), `CREATE FUNCTION fail_result_outbox_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced failure outbox error'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `CREATE TRIGGER fail_result_outbox_failure BEFORE INSERT ON workbench_result_outbox FOR EACH ROW EXECUTE FUNCTION fail_result_outbox_failure()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS fail_result_outbox_failure ON workbench_result_outbox`)
		f.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS fail_result_outbox_failure()`)
	})
	if _, err := f.service.FailTask(context.Background(), util.MustParseUUID(f.taskID), "bad request", "", "", "api_invalid_request"); err == nil {
		t.Fatal("expected failure enqueue error")
	}
	var status string
	if err := f.pool.QueryRow(context.Background(), `SELECT status FROM agent_task_queue WHERE id=$1`, f.taskID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "running" {
		t.Fatalf("task failure did not roll back: %s", status)
	}
}

func TestWorkbenchResultFailureEligibilityAndTaskKinds(t *testing.T) {
	t.Run("non retryable", func(t *testing.T) {
		f := newResultFixture(t, true, false)
		if _, err := f.service.FailTask(context.Background(), util.MustParseUUID(f.taskID), "bad request", "", "", "api_invalid_request"); err != nil {
			t.Fatal(err)
		}
		assertOutboxCount(t, f, 1)
	})
	t.Run("retryable intermediate", func(t *testing.T) {
		f := newResultFixture(t, true, false)
		if _, err := f.service.FailTask(context.Background(), util.MustParseUUID(f.taskID), "timeout", "", "", "timeout"); err != nil {
			t.Fatal(err)
		}
		assertOutboxCount(t, f, 0)
	})
	t.Run("retryable exhausted", func(t *testing.T) {
		f := newResultFixture(t, true, false)
		if _, err := f.pool.Exec(context.Background(), `UPDATE agent_task_queue SET attempt=max_attempts WHERE id=$1`, f.taskID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.FailTask(context.Background(), util.MustParseUUID(f.taskID), "timeout", "", "", "timeout"); err != nil {
			t.Fatal(err)
		}
		assertOutboxCount(t, f, 1)
	})
	t.Run("unbound", func(t *testing.T) {
		f := newResultFixture(t, false, false)
		if _, err := f.service.CompleteTask(context.Background(), util.MustParseUUID(f.taskID), []byte(`{}`), "", ""); err != nil {
			t.Fatal(err)
		}
		assertOutboxCount(t, f, 0)
	})
	t.Run("chat", func(t *testing.T) {
		f := newResultFixture(t, true, true)
		if _, err := f.service.CompleteTask(context.Background(), util.MustParseUUID(f.taskID), []byte(`{}`), "", ""); err != nil {
			t.Fatal(err)
		}
		assertOutboxCount(t, f, 0)
	})
}

func assertOutboxCount(t *testing.T, f resultFixture, want int) {
	t.Helper()
	var got int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM workbench_result_outbox WHERE task_id=$1`, f.taskID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("outbox count=%d want %d", got, want)
	}
}

func enqueueFixtureResult(t *testing.T, f resultFixture) db.WorkbenchResultOutbox {
	t.Helper()
	row, err := f.queries.EnqueueWorkbenchResult(context.Background(), db.EnqueueWorkbenchResultParams{Outcome: "completed", TaskID: util.MustParseUUID(f.taskID)})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func TestWorkbenchResultClaimsLeaseRecoveryAndCAS(t *testing.T) {
	f1 := newResultFixture(t, true, false)
	f2 := newResultFixture(t, true, false)
	for _, f := range []resultFixture{f1, f2} {
		if _, err := f.pool.Exec(context.Background(), `UPDATE agent_task_queue SET status='completed',completed_at=now(),result='{}' WHERE id=$1`, f.taskID); err != nil {
			t.Fatal(err)
		}
		enqueueFixtureResult(t, f)
	}
	var wg sync.WaitGroup
	got := make(chan string, 2)
	for i, f := range []resultFixture{f1, f2} {
		wg.Add(1)
		go func(i int, f resultFixture) {
			defer wg.Done()
			rows, err := f.queries.ClaimWorkbenchResults(context.Background(), db.ClaimWorkbenchResultsParams{LeaseOwner: pgtype.Text{String: fmt.Sprintf("owner-%d", i), Valid: true}, LeaseSeconds: 60, BatchSize: 1})
			if err != nil {
				t.Error(err)
				return
			}
			if len(rows) == 1 {
				got <- util.UUIDToString(rows[0].ID)
			}
		}(i, f)
	}
	wg.Wait()
	close(got)
	seen := map[string]bool{}
	for id := range got {
		seen[id] = true
	}
	if len(seen) != 2 {
		t.Fatalf("claims were not disjoint: %v", seen)
	}
	row := enqueueFixtureResultForRecovery(t, newResultFixture(t, true, false))
	var staleToken pgtype.UUID
	if err := f1.pool.QueryRow(context.Background(), `UPDATE workbench_result_outbox SET status='leased',lease_owner='old',lease_token=gen_random_uuid(),lease_expires_at=now()-interval '1 second' WHERE id=$1 RETURNING lease_token`, row.ID).Scan(&staleToken); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := f1.queries.ClaimWorkbenchResults(context.Background(), db.ClaimWorkbenchResultsParams{LeaseOwner: pgtype.Text{String: "new", Valid: true}, LeaseSeconds: 60, BatchSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 1 || reclaimed[0].ID != row.ID {
		t.Fatal("expired lease not reclaimed")
	}
	n, err := f1.queries.AcknowledgeWorkbenchResultDelivered(context.Background(), db.AcknowledgeWorkbenchResultDeliveredParams{ID: row.ID, LeaseToken: staleToken})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("stale token mutated row")
	}

	// Reusing the same worker identity must not make an earlier claim reusable.
	if _, err := f1.pool.Exec(context.Background(), `UPDATE workbench_result_outbox SET lease_expires_at=now()-interval '1 second' WHERE id=$1`, row.ID); err != nil {
		t.Fatal(err)
	}
	sameOwner, err := f1.queries.ClaimWorkbenchResults(context.Background(), db.ClaimWorkbenchResultsParams{LeaseOwner: pgtype.Text{String: "new", Valid: true}, LeaseSeconds: 60, BatchSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(sameOwner) != 1 || sameOwner[0].LeaseToken == reclaimed[0].LeaseToken {
		t.Fatal("owner reuse did not issue a fresh lease token")
	}
	n, err = f1.queries.AcknowledgeWorkbenchResultDelivered(context.Background(), db.AcknowledgeWorkbenchResultDeliveredParams{ID: row.ID, LeaseToken: reclaimed[0].LeaseToken})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("old token mutated a claim held by the same owner")
	}
}

func enqueueFixtureResultForRecovery(t *testing.T, f resultFixture) db.WorkbenchResultOutbox {
	if _, err := f.pool.Exec(context.Background(), `UPDATE agent_task_queue SET status='completed',completed_at=now(),result='{}' WHERE id=$1`, f.taskID); err != nil {
		t.Fatal(err)
	}
	return enqueueFixtureResult(t, f)
}

type fakeResultDeliverer struct {
	mu        sync.Mutex
	responses []ResultDeliveryResponse
	errors    []error
	calls     []ResultDelivery
}

func (f *fakeResultDeliverer) DeliverResult(_ context.Context, d ResultDelivery) (ResultDeliveryResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, d)
	i := len(f.calls) - 1
	var r ResultDeliveryResponse
	var err error
	if i < len(f.responses) {
		r = f.responses[i]
	}
	if i < len(f.errors) {
		err = f.errors[i]
	}
	return r, err
}

type blockingResultDeliverer struct {
	started chan ResultDelivery
	release chan struct{}
}

func (d *blockingResultDeliverer) DeliverResult(ctx context.Context, result ResultDelivery) (ResultDeliveryResponse, error) {
	d.started <- result
	select {
	case <-ctx.Done():
		return ResultDeliveryResponse{}, ctx.Err()
	case <-d.release:
		return ResultDeliveryResponse{StatusCode: 204}, nil
	}
}

func TestResultOutboxWorkerClaimsEachRowImmediatelyBeforeDelivery(t *testing.T) {
	f1 := newResultFixture(t, true, false)
	f2 := newResultFixture(t, true, false)
	enqueueFixtureResultForRecovery(t, f1)
	enqueueFixtureResultForRecovery(t, f2)

	slow := &blockingResultDeliverer{started: make(chan ResultDelivery, 2), release: make(chan struct{})}
	firstWorker := ResultOutboxWorker{
		Queries: f1.queries, Deliverer: slow, LeaseOwner: "slow-worker", BatchSize: 2,
		LeaseDuration: time.Second, DeliveryTimeout: 900 * time.Millisecond,
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- firstWorker.ProcessBatch(context.Background()) }()

	firstDelivery := <-slow.started
	var leased int
	if err := f1.pool.QueryRow(context.Background(), `SELECT count(*) FROM workbench_result_outbox WHERE status='leased'`).Scan(&leased); err != nil {
		t.Fatal(err)
	}
	if leased != 1 {
		t.Fatalf("slow worker pre-leased %d rows, want 1", leased)
	}

	fast := &fakeResultDeliverer{responses: []ResultDeliveryResponse{{StatusCode: 204}}}
	secondWorker := ResultOutboxWorker{
		Queries: f1.queries, Deliverer: fast, LeaseOwner: "fast-worker", BatchSize: 1,
		LeaseDuration: time.Second, DeliveryTimeout: 900 * time.Millisecond,
	}
	if err := secondWorker.ProcessBatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fast.calls) != 1 || fast.calls[0].TaskID == firstDelivery.TaskID {
		t.Fatalf("second worker did not deliver the unleased later row: calls=%v", fast.calls)
	}

	close(slow.release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if len(slow.started) != 0 {
		t.Fatal("slow worker delivered a row already claimed by the second worker")
	}
}

func TestWorkbenchResultOutboxDatabaseIntegrity(t *testing.T) {
	t.Run("rejects cross connector external record", func(t *testing.T) {
		f := newResultFixture(t, true, false)
		row := enqueueFixtureResultForRecovery(t, f)
		var otherConnectorID string
		if err := f.pool.QueryRow(context.Background(), `
			INSERT INTO connector_instance (workspace_id,key,name,connector_type,created_by)
			SELECT workspace_id, key || '-other', 'Other Connector', connector_type, created_by
			FROM connector_instance WHERE id=$1 RETURNING id`, f.connectorID).Scan(&otherConnectorID); err != nil {
			t.Fatal(err)
		}
		_, err := f.pool.Exec(context.Background(), `
			INSERT INTO workbench_result_outbox (
				workspace_id, connector_id, external_record_id, issue_id, task_id,
				outcome, payload, idempotency_key
			) VALUES ($1,$2,$3,$4,$5,'failed','{}',$6)`,
			f.workspaceID, otherConnectorID, f.recordID, f.issueID, f.taskID,
			"workbench-result:v1:"+f.taskID+":failed")
		if err == nil {
			t.Fatal("cross-connector outbox insert succeeded")
		}
		if _, err := f.pool.Exec(context.Background(), `DELETE FROM workbench_result_outbox WHERE id=$1`, row.ID); err != nil {
			t.Fatal(err)
		}
	})

	for _, tc := range []struct {
		name string
		sql  string
		id   func(resultFixture) string
	}{
		{name: "connector", sql: `DELETE FROM connector_instance WHERE id=$1`, id: func(f resultFixture) string { return f.connectorID }},
		{name: "external record", sql: `DELETE FROM external_record WHERE id=$1`, id: func(f resultFixture) string { return f.recordID }},
		{name: "issue", sql: `DELETE FROM issue WHERE id=$1`, id: func(f resultFixture) string { return f.issueID }},
	} {
		t.Run("protects "+tc.name+" delete", func(t *testing.T) {
			f := newResultFixture(t, true, false)
			enqueueFixtureResultForRecovery(t, f)
			if _, err := f.pool.Exec(context.Background(), tc.sql, tc.id(f)); err == nil {
				t.Fatalf("%s delete succeeded while outbox row exists", tc.name)
			}
		})
	}

	t.Run("protects agent task queue delete", func(t *testing.T) {
		f := newResultFixture(t, true, false)
		row := enqueueFixtureResultForRecovery(t, f)
		if _, err := f.pool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id=$1`, f.taskID); err == nil {
			t.Fatal("agent task queue delete succeeded while outbox row exists")
		}
		if _, err := f.pool.Exec(context.Background(), `DELETE FROM workbench_result_outbox WHERE id=$1`, row.ID); err != nil {
			t.Fatal(err)
		}
	})
}

func TestResultOutboxWorkerRunRetriesTransientFailureAndCancelsBackoff(t *testing.T) {
	t.Run("retries transient process failure", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := make(chan int, 3)
		worker := ResultOutboxWorker{
			Queries: &db.Queries{}, Deliverer: &fakeResultDeliverer{}, LeaseOwner: "worker",
			LeaseDuration: time.Second, DeliveryTimeout: 500 * time.Millisecond,
			BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond,
		}
		count := 0
		worker.processBatch = func(context.Context) error {
			count++
			calls <- count
			if count == 1 {
				return errors.New("transient finalization failure")
			}
			cancel()
			return nil
		}
		done := make(chan error, 1)
		go func() { done <- worker.Run(ctx) }()
		if got := <-calls; got != 1 {
			t.Fatalf("first call=%d", got)
		}
		if got := <-calls; got != 2 {
			t.Fatalf("retry call=%d", got)
		}
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error=%v", err)
		}
	})

	t.Run("cancellation interrupts backoff", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		worker := ResultOutboxWorker{
			Queries: &db.Queries{}, Deliverer: &fakeResultDeliverer{}, LeaseOwner: "worker",
			LeaseDuration: time.Second, DeliveryTimeout: 500 * time.Millisecond,
			BaseBackoff: time.Minute, MaxBackoff: time.Minute,
			processBatch: func(context.Context) error {
				close(started)
				return errors.New("database unavailable")
			},
		}
		done := make(chan error, 1)
		go func() { done <- worker.Run(ctx) }()
		<-started
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Run error=%v", err)
			}
		case <-time.After(250 * time.Millisecond):
			t.Fatal("Run did not exit promptly during backoff")
		}
	})
}

func TestConnectorResultDispatcherRoutesMixedConnectorTypes(t *testing.T) {
	f := newResultFixture(t, true, false)
	if _, err := f.pool.Exec(context.Background(), `UPDATE connector_instance SET connector_type='webhook', capabilities='{"result_delivery":true}' WHERE id=$1`, f.connectorID); err != nil {
		t.Fatal(err)
	}
	webhook := &fakeResultDeliverer{responses: []ResultDeliveryResponse{{StatusCode: 204}}}
	dispatcher := NewConnectorResultDispatcher(f.queries)
	dispatcher.Register(WebhookConnectorType, webhook)
	delivery := ResultDelivery{WorkspaceID: util.MustParseUUID(f.workspaceID), ConnectorID: util.MustParseUUID(f.connectorID)}
	if response, err := dispatcher.DeliverResult(context.Background(), delivery); err != nil || response.StatusCode != 204 || len(webhook.calls) != 1 {
		t.Fatalf("webhook route response=%+v err=%v calls=%d", response, err, len(webhook.calls))
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE connector_instance SET connector_type='ferry' WHERE id=$1`, f.connectorID); err != nil {
		t.Fatal(err)
	}
	response, err := dispatcher.DeliverResult(context.Background(), delivery)
	if err == nil || response.Retryable == nil || *response.Retryable {
		t.Fatalf("unsupported capable connector response=%+v err=%v", response, err)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE connector_instance SET capabilities='{}' WHERE id=$1`, f.connectorID); err != nil {
		t.Fatal(err)
	}
	response, err = dispatcher.DeliverResult(context.Background(), delivery)
	if err == nil || response.Retryable == nil || !*response.Retryable {
		t.Fatalf("non-capable connector must remain pending: response=%+v err=%v", response, err)
	}
}

func TestResultOutboxWorkerRetrySuccessAndTerminalFailure(t *testing.T) {
	t.Run("retry then success", func(t *testing.T) {
		f := newResultFixture(t, true, false)
		row := enqueueFixtureResultForRecovery(t, f)
		fake := &fakeResultDeliverer{responses: []ResultDeliveryResponse{{StatusCode: 503}, {StatusCode: 204}}}
		w := ResultOutboxWorker{Queries: f.queries, Deliverer: fake, LeaseOwner: "worker", BatchSize: 1, LeaseDuration: time.Minute, BaseBackoff: time.Nanosecond, MaxBackoff: time.Nanosecond}
		if err := w.ProcessBatch(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.pool.Exec(context.Background(), `UPDATE workbench_result_outbox SET next_attempt_at=now() WHERE id=$1`, row.ID)
		if err := w.ProcessBatch(context.Background()); err != nil {
			t.Fatal(err)
		}
		var status string
		var attempts int
		f.pool.QueryRow(context.Background(), `SELECT status,attempt_count FROM workbench_result_outbox WHERE id=$1`, row.ID).Scan(&status, &attempts)
		if status != "delivered" || attempts != 2 {
			t.Fatalf("status=%s attempts=%d", status, attempts)
		}
		if len(fake.calls) != 2 || fake.calls[0].IdempotencyKey != fake.calls[1].IdempotencyKey || string(fake.calls[0].Payload) != string(fake.calls[1].Payload) {
			t.Fatal("payload or idempotency changed across retry")
		}
	})
	t.Run("terminal failure", func(t *testing.T) {
		f := newResultFixture(t, true, false)
		row := enqueueFixtureResultForRecovery(t, f)
		fake := &fakeResultDeliverer{responses: []ResultDeliveryResponse{{StatusCode: 400}}, errors: []error{nil}}
		w := ResultOutboxWorker{Queries: f.queries, Deliverer: fake, LeaseOwner: "worker", BatchSize: 1}
		if err := w.ProcessBatch(context.Background()); err != nil {
			t.Fatal(err)
		}
		var status string
		f.pool.QueryRow(context.Background(), `SELECT status FROM workbench_result_outbox WHERE id=$1`, row.ID).Scan(&status)
		if status != "terminal_failed" {
			t.Fatalf("status=%s", status)
		}
	})
	t.Run("network error retries", func(t *testing.T) {
		if !retryableDelivery(ResultDeliveryResponse{}, errors.New("network")) {
			t.Fatal("network error must retry")
		}
	})
}
