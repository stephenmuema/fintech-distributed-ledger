package httpapi

import (
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/stephenmuema/finnie/internal/app"
	"github.com/stephenmuema/finnie/internal/domain"
)

// ---- Requests (validated by `validate` tags) ----

type createCustomerRequest struct {
	Type           string  `json:"type" validate:"oneof=Individual|Business|FinancialInstitution"`
	LegalName      string  `json:"legalName" validate:"required,min=2,max=200"`
	Email          string  `json:"email" validate:"required,email"`
	Phone          *string `json:"phone" validate:"phone"`
	Country        string  `json:"country" validate:"required,len=2"`
	AccountOfficer *int    `json:"accountOfficer" validate:"min=1,max=9999"`
}

// onboardingRequest is a customer signing up in the customer app: who they are, their KYC
// submission (mandatory when the onboarding policy says so) and the currencies of their accounts.
type onboardingRequest struct {
	Customer   *createCustomerRequest `json:"customer" validate:"required"`
	Kyc        *kycRequest            `json:"kyc"`
	Currencies []string               `json:"currencies" validate:"max=10"`
}

type kycRequest struct {
	DocumentType       string       `json:"documentType" validate:"required"`
	DocumentNumber     string       `json:"documentNumber" validate:"required,min=4,max=40"`
	DateOfBirth        *domain.Date `json:"dateOfBirth"`
	RegistrationNumber *string      `json:"registrationNumber"`
	Directors          []string     `json:"directors"`
}

type webhookEndpointRequest struct {
	URL string `json:"url" validate:"required,url"`
}

type setTierRequest struct {
	Tier   int    `json:"tier" validate:"min=0,max=3"`
	Reason string `json:"reason" validate:"required"`
}

type openAccountRequest struct {
	CustomerID uuid.UUID `json:"customerId"`
	Currency   string    `json:"currency" validate:"required,len=3"`
}

type restrictionRequest struct {
	Restriction string `json:"restriction" validate:"oneof=None|DebitBlock|CreditBlock|FullBlock"`
	Reason      string `json:"reason" validate:"required"`
}

type createPaymentRequest struct {
	AccountID   uuid.UUID `json:"accountId"`
	Amount      int64     `json:"amount" validate:"min=1"`
	Currency    string    `json:"currency" validate:"required,len=3"`
	Method      string    `json:"method" validate:"oneof=Card|MobileMoney"`
	Source      string    `json:"source" validate:"required"` // card token, MSISDN, or "hosted_checkout"
	Description *string   `json:"description" validate:"max=200"`
	// Hosted card checkout (source "hosted_checkout"): the payer, and where their browser returns.
	Customer  *checkoutCustomerRequest `json:"customer"`
	ReturnURL *string                  `json:"returnUrl" validate:"max=500"`
}

// checkoutCustomerRequest is the payer on a hosted checkout. It is passed to the provider's page
// and never stored.
type checkoutCustomerRequest struct {
	FirstName  string `json:"firstName" validate:"required,max=60"`
	LastName   string `json:"lastName" validate:"required,max=60"`
	Email      string `json:"email" validate:"required,max=120"`
	Phone      string `json:"phone" validate:"required,max=20"`
	Address    string `json:"address" validate:"required,max=120"`
	PostalCode string `json:"postalCode" validate:"required,max=20"`
}

func (c *checkoutCustomerRequest) toApp() *app.CheckoutCustomer {
	if c == nil {
		return nil
	}
	return &app.CheckoutCustomer{FirstName: c.FirstName, LastName: c.LastName, Email: c.Email, Phone: c.Phone, Address: c.Address, PostalCode: c.PostalCode}
}

type refundRequest struct {
	Amount *int64  `json:"amount" validate:"min=1"`
	Reason *string `json:"reason" validate:"max=200"`
}

type createPaymentLinkRequest struct {
	AccountID   uuid.UUID  `json:"accountId"`
	Amount      *int64     `json:"amount" validate:"min=1"`
	Description string     `json:"description" validate:"required,max=200"`
	ExpiresAt   *time.Time `json:"expiresAt"`
	MaxUses     *int       `json:"maxUses" validate:"min=1,max=100000"`
}

type payLinkRequest struct {
	Amount   *int64                   `json:"amount" validate:"min=1"`
	Method   string                   `json:"method" validate:"oneof=Card|MobileMoney"`
	Source   string                   `json:"source" validate:"required"`
	Customer *checkoutCustomerRequest `json:"customer"`
}

type createPayoutRequest struct {
	AccountID          uuid.UUID `json:"accountId"`
	Amount             int64     `json:"amount" validate:"min=1"`
	Rail               string    `json:"rail" validate:"oneof=MobileMoney|Bank"`
	DestinationAccount string    `json:"destinationAccount" validate:"required"`
	DestinationName    string    `json:"destinationName" validate:"required"`
	BankCode           *string   `json:"bankCode"`
	Narration          *string   `json:"narration" validate:"max=140"`
}

// customerDetailsRequest changes a customer's details; a field left out is left as it is.
type customerDetailsRequest struct {
	LegalName *string `json:"legalName" validate:"min=2,max=200"`
	Email     *string `json:"email" validate:"email"`
	Phone     *string `json:"phone" validate:"phone"`
	Country   *string `json:"country" validate:"len=2"`
	Reason    string  `json:"reason" validate:"max=200"`
}

type subAccountRequest struct {
	HolderName   string `json:"holderName" validate:"required,min=2,max=100"`
	Relationship string `json:"relationship" validate:"required"`
}

type closeAccountRequest struct {
	Reason string `json:"reason" validate:"required,max=200"`
}

type reviewRequest struct {
	Reason *string `json:"reason"`
}

type createTransferRequest struct {
	SourceAccountID      uuid.UUID `json:"sourceAccountId"`
	DestinationAccountID uuid.UUID `json:"destinationAccountId"`
	Amount               int64     `json:"amount" validate:"min=1"`
	Narration            *string   `json:"narration" validate:"max=140"`
}

type fundsTransferRequest struct {
	DebitAccount      string   `json:"debitAccount" validate:"required,len=10"`
	CreditAccount     string   `json:"creditAccount" validate:"required,len=10"`
	Amount            int64    `json:"amount" validate:"min=1"`
	Narrative         *string  `json:"narrative" validate:"max=140"`
	AcceptedOverrides []string `json:"acceptedOverrides"`
}

// createStandingOrderRequest pays either another account or a payout beneficiary, never both.
type createStandingOrderRequest struct {
	AccountID            uuid.UUID              `json:"accountId"`
	Amount               int64                  `json:"amount" validate:"min=1"`
	Frequency            string                 `json:"frequency" validate:"required,oneof=Daily|Weekly|Monthly"`
	StartDate            domain.Date            `json:"startDate"`
	EndDate              *domain.Date           `json:"endDate"`
	Narration            *string                `json:"narration" validate:"max=140"`
	DestinationAccountID *uuid.UUID             `json:"destinationAccountId"`
	Payout               *standingPayoutRequest `json:"payout"`
}

type standingPayoutRequest struct {
	Rail               string  `json:"rail" validate:"required,oneof=MobileMoney|Bank"`
	DestinationAccount string  `json:"destinationAccount" validate:"required"`
	DestinationName    string  `json:"destinationName" validate:"required"`
	BankCode           *string `json:"bankCode"`
}

type fxQuoteRequest struct {
	CustomerID uuid.UUID `json:"customerId"`
	From       string    `json:"from" validate:"required"`
	To         string    `json:"to" validate:"required"`
	Amount     int64     `json:"amount" validate:"min=1"`
}

type fxConversionRequest struct {
	QuoteID         uuid.UUID `json:"quoteId"`
	SourceAccountID uuid.UUID `json:"sourceAccountId"`
	TargetAccountID uuid.UUID `json:"targetAccountId"`
}

type issueCardRequest struct {
	AccountID           uuid.UUID `json:"accountId"`
	PerTransactionLimit *int64    `json:"perTransactionLimit" validate:"min=1"`
}

type captureRequest struct {
	Amount int64 `json:"amount" validate:"min=1"`
}

type networkAuthorizationRequest struct {
	NetworkReference     string `json:"networkReference" validate:"required"`
	CardToken            string `json:"cardToken" validate:"required"`
	Amount               int64  `json:"amount" validate:"min=1"`
	Currency             string `json:"currency" validate:"required"`
	MerchantName         string `json:"merchantName" validate:"required"`
	MerchantCategoryCode string `json:"merchantCategoryCode" validate:"required,len=4"`
}

func (r networkAuthorizationRequest) command() app.AuthorizationRequest {
	return app.AuthorizationRequest{NetworkReference: r.NetworkReference, CardToken: r.CardToken, Amount: r.Amount,
		Currency: strings.ToUpper(r.Currency), MerchantName: r.MerchantName, MerchantCategoryCode: r.MerchantCategoryCode}
}

type openDisputeRequest struct {
	PaymentID uuid.UUID `json:"paymentId"`
	Amount    *int64    `json:"amount" validate:"min=1"`
	Reason    string    `json:"reason" validate:"required"`
}

type evidenceRequest struct {
	Evidence string `json:"evidence" validate:"required,min=10,max=5000"`
}

type resolveDisputeRequest struct {
	MerchantWon bool `json:"merchantWon"`
}

type blocklistRequest struct {
	Value  string `json:"value" validate:"required"`
	Reason string `json:"reason" validate:"required"`
}

type resolveItemRequest struct {
	Note string `json:"note" validate:"required,min=5,max=500"`
}

type outageRequest struct {
	Down bool `json:"down"`
}

type faultsRequest struct {
	LatencyMs        int     `json:"latencyMs" validate:"min=0,max=60000"`
	JitterMs         int     `json:"jitterMs" validate:"min=0,max=60000"`
	ErrorRate        float64 `json:"errorRate"`
	ResponseLossRate float64 `json:"responseLossRate"`
}

type chargeChangeRequest struct {
	Description string `json:"description" validate:"required"`
	BasisPoints int    `json:"basisPoints" validate:"min=0,max=2000"`
}

type interestChangeRequest struct {
	Category   int             `json:"category"`
	Currency   string          `json:"currency" validate:"required"`
	AnnualRate decimal.Decimal `json:"annualRate"`
}

type adjustmentRequest struct {
	Currency  string                  `json:"currency" validate:"required"`
	Narrative string                  `json:"narrative" validate:"required,min=5,max=140"`
	Lines     []domain.AdjustmentLine `json:"lines" validate:"required,min=2"`
}

type sandboxEventRequest struct {
	Type        string  `json:"type" validate:"required"`
	Reference   string  `json:"reference" validate:"required"`
	Amount      *int64  `json:"amount"`
	Currency    *string `json:"currency"`
	FailureCode *string `json:"failureCode"`
	Reason      *string `json:"reason"`
	// deposit.received: the account number the payer quoted, and who paid.
	AccountReference *string `json:"accountReference"`
	Payer            *string `json:"payer"`
	PayerName        *string `json:"payerName"`
}

// ---- Responses ----

type customerResponse struct {
	ID                 uuid.UUID           `json:"id"`
	CustomerNumber     int64               `json:"customerNumber"`
	Mnemonic           string              `json:"mnemonic"`
	Type               domain.CustomerType `json:"type"`
	Sector             int                 `json:"sector"`
	AccountOfficer     int                 `json:"accountOfficer"`
	LegalName          string              `json:"legalName"`
	Email              string              `json:"email"`
	Phone              *string             `json:"phone"`
	Country            string              `json:"country"`
	KycStatus          domain.KycStatus    `json:"kycStatus"`
	Tier               int                 `json:"tier"`
	KycRejectionReason *string             `json:"kycRejectionReason"`
	Version            int64               `json:"version"`
	CreatedAt          time.Time           `json:"createdAt"`
	ContactChangedAt   *time.Time          `json:"contactChangedAt"`
}

func customerFrom(c *domain.Customer) customerResponse {
	return customerResponse{c.ID, c.CustomerNumber, c.Mnemonic, c.Type, c.Sector, c.AccountOfficer, c.LegalName, c.Email, c.Phone,
		c.Country, c.KycStatus, c.Tier, c.KycRejectionReason, c.Version, c.CreatedAt, c.ContactChangedAt}
}

// accountResponse uses T24 balance semantics: working (available), locked (held), online actual (total).
type accountResponse struct {
	ID                  uuid.UUID   `json:"id"`
	AccountNumber       string      `json:"accountNumber"`
	CustomerID          uuid.UUID   `json:"customerId"`
	Title               string      `json:"title"`
	Category            int         `json:"category"`
	Currency            string      `json:"currency"`
	WorkingBalance      int64       `json:"workingBalance"`
	LockedAmount        int64       `json:"lockedAmount"`
	OnlineActualBalance int64       `json:"onlineActualBalance"`
	Restriction         string      `json:"restriction"`
	RestrictionReason   *string     `json:"restrictionReason"`
	OpeningDate         domain.Date `json:"openingDate"`
	ClosedAt            *time.Time  `json:"closedAt"`
	ClosureReason       *string     `json:"closureReason"`
	ParentAccountID     *uuid.UUID  `json:"parentAccountId"`
	HolderName          *string     `json:"holderName"`
	HolderRelationship  *string     `json:"holderRelationship"`
}

func accountFrom(v app.AccountView) accountResponse {
	w := v.Wallet
	return accountResponse{w.ID, w.AccountNumber, w.CustomerID, w.Title, w.Category, w.Currency, v.Available, v.Held, v.Total(),
		string(w.Restriction), w.RestrictionReason, w.OpeningDate, w.ClosedAt, w.ClosureReason, w.ParentWalletID, w.HolderName, w.HolderRelationship}
}

func accountsFrom(views []app.AccountView) []accountResponse {
	out := make([]accountResponse, len(views))
	for i, v := range views {
		out[i] = accountFrom(v)
	}
	return out
}

type paymentResponse struct {
	ID                uuid.UUID               `json:"id"`
	AccountID         uuid.UUID               `json:"accountId"`
	Amount            int64                   `json:"amount"`
	Currency          string                  `json:"currency"`
	Method            domain.PaymentMethod    `json:"method"`
	PaymentSource     string                  `json:"paymentSource"`
	Status            domain.PaymentStatus    `json:"status"`
	Provider          *string                 `json:"provider"`
	ProviderReference *string                 `json:"providerReference"`
	Fee               int64                   `json:"fee"`
	RefundedAmount    int64                   `json:"refundedAmount"`
	DisputedAmount    int64                   `json:"disputedAmount"`
	HeldAmount        int64                   `json:"heldAmount"`
	ReleaseOn         *domain.Date            `json:"releaseOn"`
	FailureCode       *string                 `json:"failureCode"`
	FailureMessage    *string                 `json:"failureMessage"`
	PaymentLinkID     *uuid.UUID              `json:"paymentLinkId"`
	CreatedAt         time.Time               `json:"createdAt"`
	SucceededAt       *time.Time              `json:"succeededAt"`
	Attempts          []domain.PaymentAttempt `json:"attempts"`
	// NextAction (hosted checkout, creation response only): the form the payer's browser posts.
	NextAction *domain.NextAction `json:"nextAction,omitempty"`
}

func paymentFrom(p *domain.PaymentIntent) paymentResponse {
	attempts := p.Attempts
	if attempts == nil {
		attempts = []domain.PaymentAttempt{}
	}
	return paymentResponse{p.ID, p.MerchantWalletID, p.Amount, p.Currency, p.Method, maskSource(p.PaymentSource), p.Status, p.Provider,
		p.ProviderReference, p.Fee, p.RefundedAmount, p.DisputedAmount, p.HeldAmount, p.ReleaseOn, p.FailureCode, p.FailureMessage, p.PaymentLinkID, p.CreatedAt,
		p.SucceededAt, attempts, p.NextAction}
}

// maskSource hides the middle of an MSISDN; card tokens are already non-sensitive.
func maskSource(source string) string {
	if strings.HasPrefix(source, "+") && len(source) > 8 {
		return source[:5] + "****" + source[len(source)-3:]
	}
	return source
}

type payoutResponse struct {
	ID                 uuid.UUID           `json:"id"`
	AccountID          uuid.UUID           `json:"accountId"`
	Amount             int64               `json:"amount"`
	Fee                int64               `json:"fee"`
	Currency           string              `json:"currency"`
	Rail               domain.PayoutRail   `json:"rail"`
	DestinationAccount string              `json:"destinationAccount"`
	DestinationName    string              `json:"destinationName"`
	BankCode           *string             `json:"bankCode"`
	Status             domain.PayoutStatus `json:"status"`
	Provider           *string             `json:"provider"`
	ProviderReference  *string             `json:"providerReference"`
	SubmissionAttempts int                 `json:"submissionAttempts"`
	NextAttemptAt      *time.Time          `json:"nextAttemptAt"`
	FailureCode        *string             `json:"failureCode"`
	ReviewedBy         *string             `json:"reviewedBy"`
	// ValidationRequestedAt: a result arrived and a status check must confirm it.
	ValidationRequestedAt *time.Time `json:"validationRequestedAt"`
	// EscalatedAt: no provider confirmed the outcome, so operations were asked to settle it.
	EscalatedAt *time.Time `json:"escalatedAt"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	CompletedAt *time.Time `json:"completedAt"`
}

func payoutFrom(p *domain.Payout) payoutResponse {
	return payoutResponse{p.ID, p.WalletID, p.Amount, p.Fee, p.Currency, p.Rail, p.AccountNumber, p.AccountName, p.BankCode, p.Status,
		p.Provider, p.ProviderReference, p.SubmissionAttempts, p.NextAttemptAt, p.FailureCode, p.ReviewedBy, p.ValidationRequestedAt, p.EscalatedAt,
		p.CreatedAt, p.UpdatedAt, p.CompletedAt}
}

// cardResponse never includes the network token except at issuance.
type cardResponse struct {
	ID                  uuid.UUID         `json:"id"`
	AccountID           uuid.UUID         `json:"accountId"`
	Brand               string            `json:"brand"`
	Last4               string            `json:"last4"`
	ExpiryMonth         int               `json:"expiryMonth"`
	ExpiryYear          int               `json:"expiryYear"`
	Currency            string            `json:"currency"`
	PerTransactionLimit *int64            `json:"perTransactionLimit"`
	Status              domain.CardStatus `json:"status"`
	Token               *string           `json:"token"`
}

func cardFrom(c *domain.Card, includeToken bool) cardResponse {
	var token *string
	if includeToken {
		token = &c.Token
	}
	return cardResponse{c.ID, c.WalletID, c.Brand, c.Last4, c.ExpiryMonth, c.ExpiryYear, c.Currency, c.PerTransactionLimit, c.Status, token}
}

type networkAuthorizationResponse struct {
	AuthorizationID uuid.UUID                      `json:"authorizationId"`
	Approved        bool                           `json:"approved"`
	DeclineCode     *string                        `json:"declineCode"`
	Status          domain.CardAuthorizationStatus `json:"status"`
}

func networkAuthorizationFrom(a *domain.CardAuthorization) networkAuthorizationResponse {
	return networkAuthorizationResponse{a.ID, a.Status != domain.AuthDeclined, a.DeclineCode, a.Status}
}
