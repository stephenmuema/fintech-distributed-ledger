// Package stablecoin collects USDC through ZirnisPayments on one explicitly configured EVM
// chain. The payer signs; this module never holds keys or broadcasts transactions. Only state
// at a finalized, canonical block can credit the ledger. Run one provider per chain/contract.
package stablecoin

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/sha3"

	"github.com/stephenmuema/finnie/modules/kit"
	"github.com/stephenmuema/finnie/pkg/provapi"
)

const Currency = "USDC"
const SourceWallet = "wallet"

var addressPattern = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
var wordPattern = regexp.MustCompile(`^0x[0-9a-fA-F]{64}$`)

type Config struct {
	RPCURL           string
	ChainID          uint64
	Token            string
	PaymentsContract string
	Treasury         string
}

type Provider struct {
	cfg    Config
	client *kit.Client
}

func New(cfg Config, client *http.Client) (*Provider, error) {
	u, err := url.Parse(cfg.RPCURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Fragment != "" {
		return nil, fmt.Errorf("a valid HTTP(S) RPC URL without userinfo or fragment is required")
	}
	if cfg.ChainID == 0 {
		return nil, fmt.Errorf("chain ID must be positive")
	}
	for name, value := range map[string]string{"token": cfg.Token, "payments contract": cfg.PaymentsContract, "treasury": cfg.Treasury} {
		if !addressPattern.MatchString(value) || strings.EqualFold(value, "0x"+strings.Repeat("0", 40)) {
			return nil, fmt.Errorf("%s must be a nonzero EVM address", name)
		}
	}
	if strings.EqualFold(cfg.Token, cfg.PaymentsContract) || strings.EqualFold(cfg.Treasury, cfg.PaymentsContract) || strings.EqualFold(cfg.Token, cfg.Treasury) {
		return nil, fmt.Errorf("token, payments contract and treasury must be distinct")
	}
	cfg.Token, cfg.PaymentsContract, cfg.Treasury = strings.ToLower(cfg.Token), strings.ToLower(cfg.PaymentsContract), strings.ToLower(cfg.Treasury)
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Provider{cfg: cfg, client: &kit.Client{HTTP: client}}, nil
}

func (p *Provider) Capabilities() provapi.Capabilities {
	return provapi.Capabilities{Name: fmt.Sprintf("USDC on chain %d via %s", p.cfg.ChainID, p.cfg.PaymentsContract), IdempotentCharges: true,
		Collections: []provapi.Collection{{Method: provapi.MethodStablecoin, Currencies: []string{Currency}}},
		Details: []provapi.DetailField{
			{Name: "chain_id", Label: "Chain ID"}, {Name: "token", Label: "Token contract"},
			{Name: "payments_contract", Label: "Payments contract"}, {Name: "payment_reference", Label: "On-chain payment reference", Searchable: true},
			{Name: "finalized_block", Label: "Finalized block hash"}, {Name: "payer", Label: "Payer wallet", Searchable: true},
		}}
}

// Reference binds the core's payment UUID and exact atomic amount into one bytes32. No
// browser-supplied transaction hash can be reused to pay a different intent.
func Reference(paymentID string, amount int64) (string, error) {
	id, err := uuid.Parse(paymentID)
	if err != nil || id == uuid.Nil || amount <= 0 {
		return "", fmt.Errorf("a payment UUID and positive amount are required")
	}
	return "0x" + hex.EncodeToString(id[:]) + fmt.Sprintf("%032x", amount), nil
}

func referenceAmount(reference string) (int64, error) {
	if !wordPattern.MatchString(reference) || reference[2:34] == strings.Repeat("0", 32) {
		return 0, fmt.Errorf("invalid payment reference")
	}
	n, ok := new(big.Int).SetString(reference[34:], 16)
	if !ok || !n.IsInt64() || n.Sign() <= 0 {
		return 0, fmt.Errorf("invalid payment amount")
	}
	return n.Int64(), nil
}

func (p *Provider) Charge(ctx context.Context, r provapi.ChargeRequest) (provapi.Result, error) {
	if r.Method != provapi.MethodStablecoin || r.Currency != Currency || r.Source != SourceWallet {
		return provapi.Decline("unsupported_stablecoin_payment"), nil
	}
	ref, err := Reference(r.PaymentID, r.Amount)
	if err != nil {
		return provapi.Decline("invalid_payment"), nil
	}
	// Validate the chain, token, decimals and treasury before issuing wallet instructions.
	if _, err = p.finalized(ctx); err != nil {
		return provapi.Result{}, provapi.Unavailable("stablecoin_configuration_or_rpc")
	}
	return provapi.Result{Outcome: provapi.Pending, Reference: ref, NextAction: &provapi.NextAction{
		Type: "evm_payment", Fields: map[string]string{
			"chainId": fmt.Sprintf("0x%x", p.cfg.ChainID), "token": p.cfg.Token, "contract": p.cfg.PaymentsContract,
			"treasury": p.cfg.Treasury, "reference": ref, "amount": strconv.FormatInt(r.Amount, 10),
			"approveData": Selector("approve(address,uint256)") + strings.Repeat("0", 24) + p.cfg.PaymentsContract[2:] + fmt.Sprintf("%064x", r.Amount),
			"payData":     Selector("pay(bytes32)") + ref[2:],
		},
	}}, nil
}

// Refunds require a separately authorised treasury transfer. Decline explicitly so the core
// compensates its refund reservation; never report a pending or unsupported transfer as paid.
func (p *Provider) Refund(context.Context, provapi.RefundRequest) (provapi.Result, error) {
	return provapi.Decline("stablecoin_refund_requires_treasury"), nil
}

func (p *Provider) CheckCharge(ctx context.Context, reference string) (provapi.Result, error) {
	status, err := p.CheckTransaction(ctx, reference)
	if err != nil {
		return provapi.Result{}, err
	}
	if !status.Definitive {
		return provapi.Waiting(reference), nil
	}
	if !status.Completed {
		return provapi.Decline(status.Code), nil
	}
	return provapi.Result{Outcome: provapi.Succeeded, Reference: reference, Details: status.Details}, nil
}

func (p *Provider) CheckTransaction(ctx context.Context, reference string) (provapi.StatusResult, error) {
	expected, err := referenceAmount(reference)
	if err != nil {
		return provapi.StatusResult{}, err
	}
	block, err := p.finalized(ctx)
	if err != nil {
		return provapi.StatusResult{}, err
	}
	words, err := p.call(ctx, p.cfg.PaymentsContract, Selector("payments(bytes32)")+reference[2:], block)
	if err != nil {
		return provapi.StatusResult{}, err
	}
	if len(words) != 128 {
		return provapi.StatusResult{}, fmt.Errorf("invalid payments response")
	}
	payer, err := decodeAddress(words[:64])
	if err != nil {
		return provapi.StatusResult{}, err
	}
	amount, ok := new(big.Int).SetString(words[64:], 16)
	if !ok {
		return provapi.StatusResult{}, fmt.Errorf("invalid payment amount")
	}
	if amount.Sign() == 0 && payer == "0x"+strings.Repeat("0", 40) {
		return provapi.StatusResult{}, nil
	}
	if !amount.IsInt64() || amount.Int64() != expected || payer == "0x"+strings.Repeat("0", 40) {
		return provapi.StatusResult{}, fmt.Errorf("on-chain payment does not match the invoice")
	}
	return provapi.StatusResult{Definitive: true, Completed: true, Amount: &expected, Receipt: reference, Details: map[string]string{
		"chain_id": strconv.FormatUint(p.cfg.ChainID, 10), "token": p.cfg.Token, "payments_contract": p.cfg.PaymentsContract,
		"payment_reference": reference, "finalized_block": block, "payer": payer,
	}}, nil
}

// Balance reports only finalized USDC controlled by the configured treasury. Gas assets and
// escrow assets are separate and must never be included in the customer's settlement float.
func (p *Provider) Balance(ctx context.Context, currency string) (int64, error) {
	if currency != Currency {
		return 0, fmt.Errorf("unsupported asset")
	}
	block, err := p.finalized(ctx)
	if err != nil {
		return 0, err
	}
	word, err := p.call(ctx, p.cfg.Token, Selector("balanceOf(address)")+strings.Repeat("0", 24)+p.cfg.Treasury[2:], block)
	if err != nil {
		return 0, err
	}
	n, ok := new(big.Int).SetString(word, 16)
	if !ok || len(word) != 64 || !n.IsInt64() {
		return 0, fmt.Errorf("treasury balance exceeds the ledger range or is malformed")
	}
	return n.Int64(), nil
}

// Selector returns an ABI method selector, also used by the contract compatibility tests.
func Selector(signature string) string {
	h := sha3.NewLegacyKeccak256()
	_, _ = h.Write([]byte(signature))
	return "0x" + hex.EncodeToString(h.Sum(nil)[:4])
}

func (p *Provider) rpc(ctx context.Context, method string, params any, out any) error {
	var reply struct {
		Version string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return fmt.Errorf("invalid RPC request")
	}
	// RPC providers often put API keys in URL paths. Keep method/parameter evidence, but
	// redact the entire URL path and provider error body before they leave this module.
	u, _ := url.Parse(p.cfg.RPCURL)
	evidence := provapi.Exchange{Method: "POST", URL: u.Scheme + "://" + u.Host + "/[rpc]", Request: string(body)}
	defer func() { provapi.Record(ctx, evidence) }()
	req, err := http.NewRequestWithContext(ctx, "POST", p.cfg.RPCURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("invalid RPC endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.HTTP.Do(req)
	if err != nil {
		evidence.Error = "EVM RPC request failed"
		return fmt.Errorf("EVM RPC request failed")
	}
	defer resp.Body.Close()
	evidence.Status = resp.StatusCode
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 || resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &reply) != nil {
		evidence.Error = "invalid RPC response"
		return fmt.Errorf("invalid EVM RPC response")
	}
	if reply.Version != "2.0" || reply.ID != 1 || reply.Error != nil || len(reply.Result) == 0 || string(reply.Result) == "null" {
		return fmt.Errorf("EVM RPC %s returned no valid result", method)
	}
	evidence.Response = string(raw)
	if err = json.Unmarshal(reply.Result, out); err != nil {
		return fmt.Errorf("invalid EVM RPC %s result", method)
	}
	return nil
}

func (p *Provider) call(ctx context.Context, to, data, block string) (string, error) {
	var result string
	err := p.rpc(ctx, "eth_call", []any{map[string]string{"to": to, "data": data}, map[string]any{"blockHash": block, "requireCanonical": true}}, &result)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(result, "0x") || len(result)%64 != 2 {
		return "", fmt.Errorf("malformed ABI result")
	}
	if _, err = hex.DecodeString(result[2:]); err != nil {
		return "", fmt.Errorf("malformed ABI result")
	}
	return strings.ToLower(result[2:]), nil
}

func decodeAddress(word string) (string, error) {
	if len(word) != 64 || word[:24] != strings.Repeat("0", 24) {
		return "", fmt.Errorf("malformed ABI address")
	}
	return "0x" + word[24:], nil
}

func (p *Provider) finalized(ctx context.Context) (string, error) {
	var chain string
	if err := p.rpc(ctx, "eth_chainId", []any{}, &chain); err != nil {
		return "", err
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(chain, "0x"), 16, 64)
	if err != nil || !strings.HasPrefix(chain, "0x") || n != p.cfg.ChainID {
		return "", fmt.Errorf("RPC chain ID does not match configured chain")
	}
	var block struct {
		Hash string `json:"hash"`
	}
	if err = p.rpc(ctx, "eth_getBlockByNumber", []any{"finalized", false}, &block); err != nil {
		return "", err
	}
	if !wordPattern.MatchString(block.Hash) || block.Hash == "0x"+strings.Repeat("0", 64) {
		return "", fmt.Errorf("finalized block is unavailable")
	}
	for _, check := range []struct{ to, signature, want string }{
		{p.cfg.PaymentsContract, "token()", strings.Repeat("0", 24) + p.cfg.Token[2:]},
		{p.cfg.PaymentsContract, "treasury()", strings.Repeat("0", 24) + p.cfg.Treasury[2:]},
		{p.cfg.Token, "decimals()", fmt.Sprintf("%064x", 6)},
	} {
		word, err := p.call(ctx, check.to, Selector(check.signature), block.Hash)
		if err != nil {
			return "", err
		}
		if word != check.want {
			return "", fmt.Errorf("configured contract, treasury or token decimals do not match chain state")
		}
	}
	return block.Hash, nil
}
