package app

import (
	"context"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// StandingOrders are scheduled payments (T24: STANDING.ORDER): the customer instructs once,
// and Close of Business pays every due date straight through.
//   - The order goes through the same limits, risk and funds checks as the transfer or payout it
//     makes. A declined payment is recorded and the order moves on to its next date.
//   - MaxStandingOrderFailures declines in a row suspend the order, and the customer is told.
//   - Each due date is executed at most once: the execution row is unique per order and date, and
//     it commits with the payment.
type StandingOrders struct{ a *App }

// CreateStandingOrder pays either another account (DestinationWalletID) or a beneficiary
// (Destination), never both.
type CreateStandingOrder struct {
	WalletID            uuid.UUID
	Amount              int64
	Frequency           domain.Frequency
	StartDate           domain.Date
	EndDate             *domain.Date
	Narration           *string
	DestinationWalletID *uuid.UUID
	Destination         *domain.PayoutDestination
}

func (s *StandingOrders) Create(ctx context.Context, cmd CreateStandingOrder) (*domain.StandingOrder, error) {
	var order *domain.StandingOrder
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		if (cmd.DestinationWalletID == nil) == (cmd.Destination == nil) {
			return domain.Rule("invalid_destination", "Give exactly one destination: another account or a payout beneficiary.")
		}
		wallet, err := s.a.Accounts.loadWallet(tx, cmd.WalletID)
		if err != nil {
			return err
		}
		if err := wallet.EnsureCanDebit(); err != nil {
			return err
		}
		dates, err := s.a.Dates.Get(tx)
		if err != nil {
			return err
		}
		if order, err = domain.NewStandingOrder(wallet.CustomerID, wallet.ID, wallet.Currency, cmd.Amount, cmd.Frequency, cmd.StartDate, cmd.EndDate,
			cmd.Narration, dates.Today, tx.Now()); err != nil {
			return err
		}
		if cmd.DestinationWalletID != nil {
			destination, err := s.a.Accounts.loadWallet(tx, *cmd.DestinationWalletID)
			if err != nil {
				return err
			}
			if destination.ID == wallet.ID {
				return domain.Rule("invalid_destination", "A standing order cannot pay the account it debits.")
			}
			if destination.Currency != wallet.Currency {
				return domain.Rule("currency_mismatch", "Account %s is %s; the debit account is %s.", destination.AccountNumber, destination.Currency, wallet.Currency)
			}
			order.ToAccount(destination.ID)
		} else {
			if len(s.a.Router.PayoutRoute(cmd.Destination.Rail, wallet.Currency)) == 0 {
				return domain.Rule("rail_not_supported", "No %s payout rail supports %s.", cmd.Destination.Rail, wallet.Currency)
			}
			order.ToBeneficiary(*cmd.Destination)
		}
		if err := tx.Insert("standing_orders", order); err != nil {
			return err
		}
		if err := tx.Audit("standing_order.created", "standing_order", order.ID.String(), map[string]any{
			"kind": order.Kind, "amount": order.Amount, "currency": order.Currency, "frequency": order.Frequency, "startDate": order.StartDate}); err != nil {
			return err
		}
		return tx.Enqueue("standing_order.created", order.CustomerID, standingOrderEvent(order))
	})
	return order, err
}

func (s *StandingOrders) load(q store.Q, id uuid.UUID) (*domain.StandingOrder, error) {
	o, err := store.Get[domain.StandingOrder](q, "SELECT * FROM standing_orders WHERE id = ?", id)
	if err == nil && o == nil {
		err = domain.NotFound("standing_order", id)
	}
	return o, err
}

// Get returns an order with its executions, most recent first.
func (s *StandingOrders) Get(ctx context.Context, id uuid.UUID) (*domain.StandingOrder, error) {
	q := s.a.read(ctx)
	order, err := s.load(q, id)
	if err != nil {
		return nil, err
	}
	order.Executions, err = store.Select[domain.StandingOrderExecution](q,
		"SELECT * FROM standing_order_executions WHERE standing_order_id = ? ORDER BY due_date DESC LIMIT 100", id)
	return order, err
}

func (s *StandingOrders) List(ctx context.Context, walletID uuid.UUID) ([]domain.StandingOrder, error) {
	return store.Select[domain.StandingOrder](s.a.read(ctx), "SELECT * FROM standing_orders WHERE wallet_id = ? ORDER BY created_at", walletID)
}

func (s *StandingOrders) Cancel(ctx context.Context, id uuid.UUID) (*domain.StandingOrder, error) {
	var order *domain.StandingOrder
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if order, err = s.load(tx, id); err != nil {
			return err
		}
		if err := order.Cancel(tx.Now()); err != nil {
			return err
		}
		if err := tx.Update("standing_orders", order); err != nil {
			return err
		}
		if err := tx.Audit("standing_order.cancelled", "standing_order", id.String(), nil); err != nil {
			return err
		}
		return tx.Enqueue("standing_order.cancelled", order.CustomerID, standingOrderEvent(order))
	})
	return order, err
}

// Execute pays every active order due on or before through, one due date at a time, and
// returns how many payments completed and how many were declined.
func (s *StandingOrders) Execute(ctx context.Context, through domain.Date) (completed, declined int, err error) {
	stp := store.WithActor(ctx, STPActor)
	for {
		due, err := store.Select[struct {
			ID uuid.UUID `db:"id"`
		}](s.a.read(ctx), "SELECT id FROM standing_orders WHERE status = ? AND next_run_date <= ? ORDER BY next_run_date, id LIMIT 200",
			domain.StandingActive, through)
		if err != nil || len(due) == 0 {
			return completed, declined, err
		}
		for _, row := range due {
			for {
				execution, err := s.executeNext(stp, row.ID, through)
				if err != nil {
					return completed, declined, err
				}
				if execution == nil {
					break // paid up to date, suspended or no longer active
				}
				if execution.Status == domain.ExecutionCompleted {
					completed++
				} else {
					declined++
				}
			}
		}
	}
}

// executeNext pays an order's next due date if it falls on or before through. A rule that
// refuses the payment (funds, limits, a blocked account) rolls the attempt back and records a
// declined execution instead, so one customer's problem never halts Close of Business.
func (s *StandingOrders) executeNext(ctx context.Context, id uuid.UUID, through domain.Date) (*domain.StandingOrderExecution, error) {
	var execution *domain.StandingOrderExecution
	var queued *uuid.UUID
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		execution, queued = nil, nil
		order, err := s.load(tx, id)
		if err != nil || !order.IsDue(through) {
			return err
		}
		e := domain.NewStandingOrderExecution(order, tx.Now())
		narration := derefOr(order.Narration, "Standing order "+order.Reference())
		if err := s.a.Transactions.bindStandingOrder(tx, order, narration); err != nil {
			return err
		}
		switch order.Kind {
		case domain.StandingTransfer:
			transfer, err := s.a.Transfers.create(tx, CreateTransfer{order.WalletID, *order.DestinationWalletID, order.Amount, &narration})
			if err != nil {
				return err
			}
			e.TransferID = &transfer.ID
			if transfer.Status != domain.TransferCompleted {
				e.Decline(derefOr(transfer.DeclineCode, "declined"))
			}
		default:
			payout, err := s.a.Payouts.create(tx, CreatePayout{order.WalletID, order.Amount, order.Destination(), &narration})
			if err != nil {
				return err
			}
			e.PayoutID = &payout.ID
			switch payout.Status {
			case domain.PayoutRejected:
				e.Decline(derefOr(payout.FailureCode, "declined"))
			case domain.PayoutQueued:
				queued = &payout.ID
			}
		}
		execution = e
		return s.record(tx, order, e)
	})
	if refusal, ok := domain.AsError(err); ok {
		err = s.a.db.Atomic(ctx, func(tx *store.Tx) error {
			execution, queued = nil, nil
			order, err := s.load(tx, id)
			if err != nil || !order.IsDue(through) {
				return err
			}
			execution = domain.NewStandingOrderExecution(order, tx.Now())
			execution.Decline(refusal.Code)
			return s.record(tx, order, execution)
		})
	}
	if err != nil {
		return nil, err
	}
	if queued != nil {
		// Sent now, like any payout; a rail outage leaves it queued for the retry sweep.
		if _, err := s.a.Payouts.Submit(ctx, *queued); err != nil {
			s.a.Logger.Error("standing order payout could not be submitted; the retry sweep will send it", "payout", *queued, "error", err)
		}
	}
	return execution, nil
}

// record saves an execution and moves the order to its next due date.
func (s *StandingOrders) record(tx *store.Tx, order *domain.StandingOrder, e *domain.StandingOrderExecution) error {
	if err := tx.Insert("standing_order_executions", e); err != nil {
		return err
	}
	suspended := order.Record(e, tx.Now())
	if err := tx.Update("standing_orders", order); err != nil {
		return err
	}
	if e.Status == domain.ExecutionCompleted {
		return nil
	}
	details := map[string]any{"dueDate": e.DueDate, "declineCode": e.DeclineCode, "consecutiveFailures": order.ConsecutiveFailures}
	if err := tx.Audit("standing_order.declined", "standing_order", order.ID.String(), details); err != nil {
		return err
	}
	event := standingOrderEvent(order)
	event["due_date"], event["decline_code"] = e.DueDate, e.DeclineCode
	if err := tx.Enqueue("standing_order.declined", order.CustomerID, event); err != nil {
		return err
	}
	if !suspended {
		return nil
	}
	if err := tx.Audit("standing_order.suspended", "standing_order", order.ID.String(), details); err != nil {
		return err
	}
	return tx.Enqueue("standing_order.suspended", order.CustomerID, standingOrderEvent(order))
}

func standingOrderEvent(o *domain.StandingOrder) map[string]any {
	return map[string]any{
		"id": o.ID, "reference": o.Reference(), "status": snake(string(o.Status)), "kind": snake(string(o.Kind)), "amount": o.Amount,
		"currency": o.Currency, "frequency": snake(string(o.Frequency)), "next_run_date": o.NextRunDate, "account_id": o.WalletID,
	}
}
