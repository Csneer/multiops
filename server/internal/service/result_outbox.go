package service

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// ResultDeliverer is the trusted connector-specific delivery boundary. It does
// not expose URLs or transport configuration to the generic outbox worker.
// Implementations must honor ctx cancellation and return promptly when it ends;
// the worker bounds every call to less than the active lease duration.
type ResultDeliverer interface {
	DeliverResult(ctx context.Context, result ResultDelivery) (ResultDeliveryResponse, error)
}

type ConnectorResultDispatcher struct {
	Queries  *db.Queries
	registry map[string]ResultDeliverer
}

func NewConnectorResultDispatcher(queries *db.Queries) *ConnectorResultDispatcher {
	return &ConnectorResultDispatcher{Queries: queries, registry: make(map[string]ResultDeliverer)}
}

func (d *ConnectorResultDispatcher) Register(connectorType string, deliverer ResultDeliverer) {
	if d.registry == nil {
		d.registry = make(map[string]ResultDeliverer)
	}
	d.registry[connectorType] = deliverer
}

func (d *ConnectorResultDispatcher) DeliverResult(ctx context.Context, result ResultDelivery) (ResultDeliveryResponse, error) {
	if d.Queries == nil {
		return ResultDeliveryResponse{}, errors.New("result dispatcher: queries are required")
	}
	connector, err := d.Queries.GetConnectorInWorkspace(ctx, db.GetConnectorInWorkspaceParams{ID: result.ConnectorID, WorkspaceID: result.WorkspaceID})
	if err != nil {
		return ResultDeliveryResponse{}, fmt.Errorf("load result connector: %w", err)
	}
	if !hasResultCapability(connector.Capabilities) {
		retry := true
		return ResultDeliveryResponse{Retryable: &retry}, errors.New("connector is not configured for result delivery")
	}
	deliverer := d.registry[connector.ConnectorType]
	if deliverer == nil {
		retry := false
		return ResultDeliveryResponse{Retryable: &retry}, fmt.Errorf("unsupported result delivery connector type %q", connector.ConnectorType)
	}
	return deliverer.DeliverResult(ctx, result)
}

type ResultDelivery struct {
	WorkspaceID      pgtype.UUID
	ConnectorID      pgtype.UUID
	ExternalRecordID pgtype.UUID
	IssueID          pgtype.UUID
	TaskID           pgtype.UUID
	Outcome          string
	Payload          []byte
	IdempotencyKey   string
}

type ResultDeliveryResponse struct {
	StatusCode int
	// Retryable overrides the worker's default HTTP/error classification when
	// non-nil. Connector routing uses this to make invalid or unavailable
	// connector configuration terminal while preserving network retries.
	Retryable *bool
}

type ResultOutboxWorker struct {
	Queries         *db.Queries
	Deliverer       ResultDeliverer
	LeaseOwner      string
	BatchSize       int32
	LeaseDuration   time.Duration
	DeliveryTimeout time.Duration
	PollInterval    time.Duration
	BaseBackoff     time.Duration
	MaxBackoff      time.Duration

	processBatch func(context.Context) error
}

func (w *ResultOutboxWorker) Run(ctx context.Context) error {
	if err := w.validate(); err != nil {
		return err
	}
	interval := w.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	processBatch := w.ProcessBatch
	if w.processBatch != nil {
		processBatch = w.processBatch
	}
	consecutiveFailures := int32(0)
	for {
		if err := processBatch(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			consecutiveFailures++
			if err := waitForResultOutbox(ctx, w.retryDelay(consecutiveFailures)); err != nil {
				return err
			}
			continue
		}
		consecutiveFailures = 0
		if err := waitForResultOutbox(ctx, interval); err != nil {
			return err
		}
	}
}

func waitForResultOutbox(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (w *ResultOutboxWorker) validate() error {
	if w.Queries == nil || w.Deliverer == nil {
		return errors.New("result outbox worker: queries and deliverer are required")
	}
	if w.LeaseOwner == "" {
		return errors.New("result outbox worker: lease owner is required")
	}
	leaseDuration := w.LeaseDuration
	if leaseDuration <= 0 {
		leaseDuration = 30 * time.Second
	}
	deliveryTimeout := w.DeliveryTimeout
	if deliveryTimeout <= 0 {
		deliveryTimeout = 20 * time.Second
	}
	if deliveryTimeout >= leaseDuration {
		return errors.New("result outbox worker: delivery timeout must be shorter than lease duration")
	}
	return nil
}

func (w *ResultOutboxWorker) ProcessBatch(ctx context.Context) error {
	if err := w.validate(); err != nil {
		return err
	}
	batchSize := w.BatchSize
	if batchSize <= 0 {
		batchSize = 20
	}
	leaseDuration := w.LeaseDuration
	if leaseDuration <= 0 {
		leaseDuration = 30 * time.Second
	}
	deliveryTimeout := w.DeliveryTimeout
	if deliveryTimeout <= 0 {
		deliveryTimeout = 20 * time.Second
	}
	if deliveryTimeout >= leaseDuration {
		return errors.New("result outbox worker: delivery timeout must be shorter than lease duration")
	}
	for range batchSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := w.Queries.ClaimWorkbenchResults(ctx, db.ClaimWorkbenchResultsParams{
			LeaseOwner:   pgtype.Text{String: w.LeaseOwner, Valid: true},
			LeaseSeconds: leaseDuration.Seconds(),
			BatchSize:    1,
		})
		if err != nil {
			return fmt.Errorf("claim workbench result: %w", err)
		}
		if len(rows) == 0 {
			return nil
		}
		row := rows[0]
		deliveryCtx, cancel := context.WithTimeout(ctx, deliveryTimeout)
		response, deliverErr := w.Deliverer.DeliverResult(deliveryCtx, ResultDelivery{
			WorkspaceID: row.WorkspaceID, ConnectorID: row.ConnectorID,
			ExternalRecordID: row.ExternalRecordID, IssueID: row.IssueID,
			TaskID: row.TaskID, Outcome: row.Outcome,
			Payload: append([]byte(nil), row.Payload...), IdempotencyKey: row.IdempotencyKey,
		})
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		status := pgtype.Int4{Int32: int32(response.StatusCode), Valid: response.StatusCode != 0}
		token := row.LeaseToken
		switch {
		case deliverErr == nil && response.StatusCode >= 200 && response.StatusCode < 300:
			var affected int64
			affected, err = w.Queries.AcknowledgeWorkbenchResultDelivered(ctx, db.AcknowledgeWorkbenchResultDeliveredParams{ID: row.ID, LeaseToken: token, LastStatus: status})
			if err == nil && affected == 0 {
				err = errors.New("lease ownership lost")
			}
		case retryableDelivery(response, deliverErr):
			next := time.Now().Add(w.retryDelay(row.AttemptCount))
			lastError := pgtype.Text{String: deliveryError(response.StatusCode, deliverErr), Valid: true}
			var affected int64
			affected, err = w.Queries.RetryWorkbenchResult(ctx, db.RetryWorkbenchResultParams{ID: row.ID, LeaseToken: token, NextAttemptAt: pgtype.Timestamptz{Time: next, Valid: true}, LastStatus: status, LastError: lastError})
			if err == nil && affected == 0 {
				err = errors.New("lease ownership lost")
			}
		default:
			lastError := pgtype.Text{String: deliveryError(response.StatusCode, deliverErr), Valid: true}
			var affected int64
			affected, err = w.Queries.TerminalFailWorkbenchResult(ctx, db.TerminalFailWorkbenchResultParams{ID: row.ID, LeaseToken: token, LastStatus: status, LastError: lastError})
			if err == nil && affected == 0 {
				err = errors.New("lease ownership lost")
			}
		}
		if err != nil {
			return fmt.Errorf("finalize workbench result %s: %w", row.IdempotencyKey, err)
		}
	}
	return nil
}

func retryableDelivery(response ResultDeliveryResponse, err error) bool {
	if response.Retryable != nil {
		return *response.Retryable
	}
	if err != nil {
		return true
	}
	status := response.StatusCode
	return status == 408 || status == 425 || status == 429 || status >= 500
}

func deliveryError(status int, err error) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("delivery returned HTTP %d", status)
}

func (w *ResultOutboxWorker) retryDelay(attempt int32) time.Duration {
	base := w.BaseBackoff
	if base <= 0 {
		base = time.Second
	}
	maxDelay := w.MaxBackoff
	if maxDelay <= 0 {
		maxDelay = 15 * time.Minute
	}
	delay := base
	for i := int32(1); i < attempt && delay < maxDelay/2; i++ {
		delay *= 2
	}
	if delay > maxDelay {
		delay = maxDelay
	}
	// Full jitter avoids synchronized retries while preserving the cap.
	return time.Duration(rand.Int64N(int64(delay) + 1))
}
