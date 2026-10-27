package app

import (
	"context"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// ---------------------------------------------------------------------------------------------
// ST — customers (CIF), KYC / KYB and tiering

type Customers struct{ a *App }

const DefaultAccountOfficer = 1

type CreateCustomer struct {
	Type           domain.CustomerType
	LegalName      string
	Email          string
	Phone          *string
	Country        string
	AccountOfficer *int
}

func (s *Customers) Create(ctx context.Context, cmd CreateCustomer) (*domain.Customer, error) {
	var customer *domain.Customer
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		customer, err = s.create(tx, cmd, domain.NewID())
		return err
	})
	return customer, err
}

func (s *Customers) create(tx *store.Tx, cmd CreateCustomer, id uuid.UUID) (*domain.Customer, error) {
	email := strings.ToLower(strings.TrimSpace(cmd.Email))
	exists, err := store.Exists(tx, "SELECT 1 FROM customers WHERE email = ?", email)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, domain.Conflict("customer_exists", "A customer with email '%s' already exists.", email)
	}
	number, err := s.a.Numbers.Next(tx, CustomerSequence)
	if err != nil {
		return nil, err
	}
	officer := DefaultAccountOfficer
	if cmd.AccountOfficer != nil {
		officer = *cmd.AccountOfficer
	}
	customer, err := domain.NewCustomer(number, cmd.Type, cmd.LegalName, cmd.Email, cmd.Phone, cmd.Country, officer, tx.Now())
	if err != nil {
		return nil, err
	}
	customer.ID = id
	if err := tx.Insert("customers", customer); err != nil {
		return nil, err
	}
	return customer, tx.Audit("customer.created", "customer", customer.ID.String(),
		map[string]any{"customerNumber": customer.CustomerNumber, "mnemonic": customer.Mnemonic, "type": customer.Type})
}

func (s *Customers) load(q store.Q, id uuid.UUID) (*domain.Customer, error) {
	c, err := store.Get[domain.Customer](q, "SELECT * FROM customers WHERE id = ?", id)
	if err == nil && c == nil {
		err = domain.NotFound("customer", id)
	}
	return c, err
}

func (s *Customers) Get(ctx context.Context, id uuid.UUID) (*domain.Customer, error) {
	return s.load(s.a.read(ctx), id)
}

func (s *Customers) GetByNumber(ctx context.Context, number int64) (*domain.Customer, error) {
	c, err := store.Get[domain.Customer](s.a.read(ctx), "SELECT * FROM customers WHERE customer_number = ?", number)
	if err == nil && c == nil {
		err = domain.NotFound("customer", number)
	}
	return c, err
}

func (s *Customers) List(ctx context.Context, limit int) ([]domain.Customer, error) {
	return s.Search(ctx, "", 0, limit)
}

// Search finds customers by CIF number, or by a part of their name, email or mnemonic, newest
// first. Paging is by CIF number (before): stable however many customers are added meanwhile.
func (s *Customers) Search(ctx context.Context, q string, before int64, limit int) ([]domain.Customer, error) {
	where, args := []string{"1 = 1"}, []any{}
	if q = strings.TrimSpace(q); q != "" {
		if number, err := strconv.ParseInt(q, 10, 64); err == nil {
			where, args = append(where, "customer_number = ?"), append(args, number)
		} else {
			like := "%" + strings.NewReplacer("%", "", "_", "").Replace(strings.ToLower(q)) + "%"
			where = append(where, "(LOWER(legal_name) LIKE ? OR LOWER(email) LIKE ? OR LOWER(mnemonic) LIKE ?)")
			args = append(args, like, like, like)
		}
	}
	if before > 0 {
		where, args = append(where, "customer_number < ?"), append(args, before)
	}
	args = append(args, clamp(limit, 1, 200))
	return store.Select[domain.Customer](s.a.read(ctx), "SELECT * FROM customers WHERE "+strings.Join(where, " AND ")+" ORDER BY customer_number DESC LIMIT ?", args...)
}

// SubmitKyc runs KYC (individuals) or KYB (businesses) with the verification vendor. The vendor
// call happens outside the database transaction; only its outcome is persisted.
func (s *Customers) SubmitKyc(ctx context.Context, id uuid.UUID, submission KycSubmission) (*domain.Customer, error) {
	snapshot, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if snapshot.Type.IsCorporate() && (submission.RegistrationNumber == nil || strings.TrimSpace(*submission.RegistrationNumber) == "") {
		return nil, domain.Rule("registration_number_required", "Business verification (KYB) requires a registration number.")
	}
	result, err := s.a.KYC.Verify(ctx, *snapshot, submission)
	if err != nil {
		return nil, err
	}

	var customer *domain.Customer
	err = s.a.db.Atomic(ctx, func(tx *store.Tx) (err error) {
		if customer, err = s.load(tx, id); err != nil {
			return err
		}
		return s.recordSubmission(tx, customer, submission, result)
	})
	return customer, err
}

// recordSubmission records a KYC submission and the vendor's answer: a decision, or a review
// by operations under maker-checker (Parameters.ProposeKycDecision).
func (s *Customers) recordSubmission(tx *store.Tx, customer *domain.Customer, submission KycSubmission, result KycResult) error {
	if err := customer.SubmitKyc(); err != nil {
		return err
	}
	if !result.Pending {
		return s.recordKyc(tx, customer, result.Approved, result.Reference, derefOr(result.RejectionReason, "rejected"))
	}
	if err := tx.Insert("kyc_submissions", &domain.KycSubmissionRecord{ID: domain.NewID(), CustomerID: customer.ID, DocumentType: submission.DocumentType,
		DocumentNumber: submission.DocumentNumber, DateOfBirth: submission.DateOfBirth, RegistrationNumber: submission.RegistrationNumber,
		Directors: domain.StringList(submission.Directors), SubmittedAt: tx.Now()}); err != nil {
		return err
	}
	if err := tx.Update("customers", customer); err != nil {
		return err
	}
	return tx.Audit("kyc.submitted_for_review", "customer", customer.ID.String(), map[string]any{"documentType": submission.DocumentType})
}

// recordKyc applies a verification decision — the vendor's, or a reviewer's once authorised.
func (s *Customers) recordKyc(tx *store.Tx, customer *domain.Customer, approved bool, reference, reason string) error {
	action, event := "kyc.approved", "customer.kyc_verified"
	if approved {
		customer.ApproveKyc(reference)
	} else {
		action, event = "kyc.rejected", "customer.kyc_rejected"
		customer.RejectKyc(reason)
	}
	if err := tx.Update("customers", customer); err != nil {
		return err
	}
	data := map[string]any{"reference": reference, "tier": customer.Tier}
	if !approved {
		data["rejectionReason"] = reason
	}
	if err := tx.Audit(action, "customer", customer.ID.String(), data); err != nil {
		return err
	}
	return tx.Enqueue(event, customer.ID, map[string]any{"customer_id": customer.ID, "status": snake(string(customer.KycStatus)), "tier": customer.Tier})
}

// setTier overrides a customer's limits tier; operators do it under maker-checker
// (Parameters.ProposeTier).
func (s *Customers) setTier(tx *store.Tx, id uuid.UUID, tier int, reason string) (*domain.Customer, error) {
	customer, err := s.load(tx, id)
	if err != nil {
		return nil, err
	}
	previous := customer.Tier
	if err := customer.SetTier(tier); err != nil {
		return nil, err
	}
	if err := tx.Update("customers", customer); err != nil {
		return nil, err
	}
	return customer, tx.Audit("customer.tier_changed", "customer", id.String(), map[string]any{"previous": previous, "tier": tier, "reason": reason})
}

// SetWebhookEndpoint configures where events are delivered and returns the signing secret.
func (s *Customers) SetWebhookEndpoint(ctx context.Context, customerID uuid.UUID, rawURL string) (*domain.WebhookEndpoint, error) {
	u, err := url.Parse(rawURL)
	if err != nil || !u.IsAbs() {
		return nil, domain.Rule("invalid_webhook_url", "Webhook URL must be absolute.")
	}
	if u.Scheme != "https" && !isLoopback(u.Hostname()) {
		return nil, domain.Rule("insecure_webhook_url", "Webhook URLs must use HTTPS.")
	}
	var endpoint *domain.WebhookEndpoint
	err = s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		if _, err := s.load(tx, customerID); err != nil {
			return err
		}
		existing, err := store.Get[domain.WebhookEndpoint](tx, "SELECT * FROM webhook_endpoints WHERE customer_id = ?", customerID)
		if err != nil {
			return err
		}
		if existing == nil {
			endpoint = domain.NewWebhookEndpoint(customerID, u.String(), tx.Now())
			err = tx.Insert("webhook_endpoints", endpoint)
		} else {
			endpoint = existing
			endpoint.URL = u.String()
			err = tx.Update("webhook_endpoints", endpoint)
		}
		if err != nil {
			return err
		}
		return tx.Audit("webhook_endpoint.configured", "customer", customerID.String(), map[string]any{"url": endpoint.URL})
	})
	return endpoint, err
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---------------------------------------------------------------------------------------------
// AC — accounts: opening, balances, statements and posting restrictions

type Accounts struct{ a *App }

// AccountView is an account with T24 balance semantics.
type AccountView struct {
	Wallet domain.Wallet
	// Available is the working balance — what the customer can spend.
	Available int64
	// Held is locked amounts (card authorisations, disputes).
	Held int64
}

// Total is the online actual balance.
func (v AccountView) Total() int64 { return v.Available + v.Held }

type StatementLine struct {
	EntryID              uuid.UUID             `json:"entryId"`
	TransactionReference string                `json:"transactionReference"`
	Category             domain.EntryCategory  `json:"category"`
	Description          string                `json:"description"`
	Direction            domain.EntryDirection `json:"direction"`
	Amount               int64                 `json:"amount"`
	BalanceAfter         int64                 `json:"balanceAfter"`
	BookingDate          domain.Date           `json:"bookingDate"`
	ValueDate            domain.Date           `json:"valueDate"`
	PostedAt             time.Time             `json:"postedAt"`
}

func (s *Accounts) Open(ctx context.Context, customerID uuid.UUID, currency string) (*domain.Wallet, error) {
	var wallet *domain.Wallet
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		customer, err := s.a.Customers.load(tx, customerID)
		if err != nil {
			return err
		}
		wallet, err = s.open(tx, customer, currency)
		return err
	})
	return wallet, err
}

func (s *Accounts) open(tx *store.Tx, customer *domain.Customer, currency string) (*domain.Wallet, error) {
	policy, err := s.a.Customers.onboardingPolicy(tx)
	if err != nil {
		return nil, err
	}
	if err := policy.EnsureKyc(customer); err != nil {
		return nil, err
	}
	ccy, err := domain.CurrencyOf(strings.ToUpper(currency))
	if err != nil {
		return nil, err
	}
	if err := tx.Lock("customer:" + customer.ID.String()); err != nil { // one opening at a time per customer
		return nil, err
	}
	exists, err := store.Exists(tx, "SELECT 1 FROM wallets WHERE customer_id = ? AND currency = ? AND closed_at IS NULL AND parent_wallet_id IS NULL", customer.ID, ccy.Code)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, domain.Conflict("wallet_exists", "Customer already has a %s account.", ccy.Code)
	}
	sequence, err := s.a.Numbers.Next(tx, AccountSequence)
	if err != nil {
		return nil, err
	}
	businessDate, err := s.a.Dates.OnlineBookingDate(tx)
	if err != nil {
		return nil, err
	}
	w, available, held, err := domain.OpenWallet(customer, ccy.Code, domain.CustomerAccountNumber(sequence), businessDate, tx.Now())
	if err != nil {
		return nil, err
	}
	for _, acct := range []*domain.LedgerAccount{available, held} {
		if err := tx.Insert("ledger_accounts", acct); err != nil {
			return nil, err
		}
	}
	if err := tx.Insert("wallets", w); err != nil {
		return nil, err
	}
	return w, tx.Audit("account.opened", "wallet", w.ID.String(), map[string]any{
		"accountNumber": w.AccountNumber, "category": w.Category, "currency": w.Currency, "customerNumber": customer.CustomerNumber})
}

// OpenSubAccount opens an account under a main account for someone the customer names — a
// spouse or child travelling with them — in the main account's currency.
func (s *Accounts) OpenSubAccount(ctx context.Context, parentID uuid.UUID, holderName, relationship string) (*AccountView, error) {
	var id uuid.UUID
	err := s.a.db.Atomic(ctx, func(tx *store.Tx) error {
		parent, err := s.loadWallet(tx, parentID)
		if err != nil {
			return err
		}
		customer, err := s.a.Customers.load(tx, parent.CustomerID)
		if err != nil {
			return err
		}
		if err := tx.Lock("customer:" + customer.ID.String()); err != nil { // one opening at a time per customer
			return err
		}
		policy, err := s.a.Customers.onboardingPolicy(tx)
		if err != nil {
			return err
		}
		if err := policy.EnsureKyc(customer); err != nil {
			return err
		}
		open, err := store.Scalar[int](tx, "SELECT COUNT(*) FROM wallets WHERE parent_wallet_id = ? AND closed_at IS NULL", parent.ID)
		if err != nil {
			return err
		}
		if open >= domain.MaxSubAccounts {
			return domain.Rule("too_many_sub_accounts", "Account %s already has %d sub-accounts.", parent.AccountNumber, open)
		}
		sequence, err := s.a.Numbers.Next(tx, AccountSequence)
		if err != nil {
			return err
		}
		businessDate, err := s.a.Dates.OnlineBookingDate(tx)
		if err != nil {
			return err
		}
		w, available, held, err := domain.OpenSubAccount(parent, customer, holderName, relationship, domain.CustomerAccountNumber(sequence), businessDate, tx.Now())
		if err != nil {
			return err
		}
		for _, acct := range []*domain.LedgerAccount{available, held} {
			if err := tx.Insert("ledger_accounts", acct); err != nil {
				return err
			}
		}
		if err := tx.Insert("wallets", w); err != nil {
			return err
		}
		id = w.ID
		return tx.Audit("account.sub_account_opened", "wallet", w.ID.String(), map[string]any{"accountNumber": w.AccountNumber, "parent": parent.AccountNumber,
			"currency": w.Currency, "relationship": relationship, "customerNumber": customer.CustomerNumber})
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

func (s *Accounts) loadWallet(q store.Q, id uuid.UUID) (*domain.Wallet, error) {
	w, err := store.Get[domain.Wallet](q, "SELECT * FROM wallets WHERE id = ?", id)
	if err == nil && w == nil {
		err = domain.NotFound("wallet", id)
	}
	return w, err
}

func (s *Accounts) loadByNumber(q store.Q, number string) (*domain.Wallet, error) {
	w, err := store.Get[domain.Wallet](q, "SELECT * FROM wallets WHERE account_number = ?", number)
	if err == nil && w == nil {
		err = domain.NotFound("account", number)
	}
	return w, err
}

func (s *Accounts) Get(ctx context.Context, id uuid.UUID) (*AccountView, error) {
	q := s.a.read(ctx)
	w, err := s.loadWallet(q, id)
	if err != nil {
		return nil, err
	}
	return s.view(q, w)
}

func (s *Accounts) GetByAccountNumber(ctx context.Context, number string) (*AccountView, error) {
	q := s.a.read(ctx)
	w, err := s.loadByNumber(q, number)
	if err != nil {
		return nil, err
	}
	return s.view(q, w)
}

func (s *Accounts) view(q store.Q, w *domain.Wallet) (*AccountView, error) {
	views, err := s.views(q, []domain.Wallet{*w})
	if err != nil {
		return nil, err
	}
	return &views[0], nil
}

func (s *Accounts) views(q store.Q, wallets []domain.Wallet) ([]AccountView, error) {
	ids := make([]uuid.UUID, 0, 2*len(wallets))
	for _, w := range wallets {
		ids = append(ids, w.AvailableAccountID, w.HeldAccountID)
	}
	marks, args := store.In(ids)
	rows, err := store.Select[struct {
		ID      uuid.UUID `db:"id"`
		Balance int64     `db:"balance"`
	}](q, "SELECT id, balance FROM ledger_accounts WHERE id IN ("+marks+")", args...)
	if err != nil {
		return nil, err
	}
	balances := map[uuid.UUID]int64{}
	for _, r := range rows {
		balances[r.ID] = r.Balance
	}
	views := make([]AccountView, len(wallets))
	for i, w := range wallets {
		views[i] = AccountView{Wallet: w, Available: balances[w.AvailableAccountID], Held: balances[w.HeldAccountID]}
	}
	return views, nil
}

func (s *Accounts) ListForCustomer(ctx context.Context, customerID uuid.UUID) ([]AccountView, error) {
	q := s.a.read(ctx)
	wallets, err := store.Select[domain.Wallet](q, "SELECT * FROM wallets WHERE customer_id = ? ORDER BY created_at", customerID)
	if err != nil || len(wallets) == 0 {
		return []AccountView{}, err
	}
	return s.views(q, wallets)
}

// Statement is the running-balance statement of the account (T24: STMT.ENTRY enquiry), newest first.
func (s *Accounts) Statement(ctx context.Context, walletID uuid.UUID, from, to *domain.Date, limit int) ([]StatementLine, error) {
	q := s.a.read(ctx)
	w, err := s.loadWallet(q, walletID)
	if err != nil {
		return nil, err
	}
	query := `SELECT e.id AS entry_id, e.transaction_reference, e.category, e.description, l.direction, l.amount, l.balance_after,
	                 e.booking_date, e.value_date, e.posted_at
	          FROM journal_lines l JOIN journal_entries e ON e.id = l.journal_entry_id
	          WHERE l.account_id = ?`
	args := []any{w.AvailableAccountID}
	if from != nil {
		query += " AND l.booking_date >= ?"
		args = append(args, *from)
	}
	if to != nil {
		query += " AND l.booking_date <= ?"
		args = append(args, *to)
	}
	query += " ORDER BY e.posted_at DESC, l.id DESC LIMIT ?"
	args = append(args, clamp(limit, 1, 500))

	rows, err := store.Select[struct {
		EntryID              uuid.UUID             `db:"entry_id"`
		TransactionReference string                `db:"transaction_reference"`
		Category             domain.EntryCategory  `db:"category"`
		Description          string                `db:"description"`
		Direction            domain.EntryDirection `db:"direction"`
		Amount               int64                 `db:"amount"`
		BalanceAfter         int64                 `db:"balance_after"`
		BookingDate          domain.Date           `db:"booking_date"`
		ValueDate            domain.Date           `db:"value_date"`
		PostedAt             time.Time             `db:"posted_at"`
	}](q, query, args...)
	if err != nil {
		return nil, err
	}
	lines := make([]StatementLine, len(rows))
	for i, r := range rows {
		lines[i] = StatementLine(r)
	}
	return lines, nil
}

// restrict sets a posting restriction; operators do it under maker-checker
// (Parameters.ProposeRestriction).
// fence makes a change to an account's state (closed, restricted) conflict with any posting to it
// still in flight: the wallet's ledger accounts are written under the versions this unit of work
// read. A posting that read the old state then fails its version check and runs again, seeing
// the new one; one that committed first makes this unit of work run again, seeing its money.
func (s *Accounts) fence(tx *store.Tx, w *domain.Wallet) error {
	available, held, err := s.a.Ledger.WalletAccounts(tx, w)
	if err != nil {
		return err
	}
	for _, a := range []*domain.LedgerAccount{available, held} {
		if err := tx.Update("ledger_accounts", a); err != nil {
			return err
		}
	}
	return nil
}

func (s *Accounts) restrict(tx *store.Tx, id uuid.UUID, restriction domain.PostingRestriction, reason string) error {
	w, err := s.loadWallet(tx, id)
	if err != nil {
		return err
	}
	if err := w.EnsureOpen(); err != nil {
		return err
	}
	previous := w.Restriction
	w.Restrict(restriction, reason)
	if err := tx.Update("wallets", w); err != nil {
		return err
	}
	if err := s.fence(tx, w); err != nil {
		return err
	}
	return tx.Audit("account.posting_restriction_changed", "wallet", id.String(), map[string]any{
		"accountNumber": w.AccountNumber, "previous": previous, "restriction": restriction, "reason": reason})
}
