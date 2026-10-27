package app

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/stephenmuema/finnie/internal/domain"
	"github.com/stephenmuema/finnie/internal/store"
)

// Transaction types (FT.TXN.TYPE.CONDITION) and versions (VERSION) are parameter records: input
// INAU, authorised by a second operator, historised.

func (s *Transactions) loadType(q store.Q, code string) (*domain.TransactionType, error) {
	t, err := store.Get[domain.TransactionType](q, "SELECT * FROM transaction_types WHERE code = ?", code)
	if err == nil && t == nil {
		err = domain.NotFound("transaction_type", code)
	}
	return t, err
}

func (s *Transactions) loadVersion(q store.Q, name string) (*domain.Version, error) {
	v, err := store.Get[domain.Version](q, "SELECT * FROM versions WHERE name = ?", name)
	if err == nil && v == nil {
		err = domain.NotFound("version", name)
	}
	return v, err
}

// Types lists the transaction types.
func (s *Transactions) Types(ctx context.Context) ([]domain.TransactionType, error) {
	return store.Select[domain.TransactionType](s.a.read(ctx), "SELECT * FROM transaction_types ORDER BY code")
}

// Type returns one transaction type.
func (s *Transactions) Type(ctx context.Context, code string) (*domain.TransactionType, error) {
	return s.loadType(s.a.read(ctx), strings.ToUpper(code))
}

// VersionView is a version with what a screen needs to render it.
type VersionView struct {
	domain.Version
	Flow domain.TransactionFlow `json:"flow"`
	// Inputs are the core fields the version takes (its list, or every field of an open screen).
	Inputs []string `json:"inputs"`
	// LocalFields are the transaction local fields the version can show.
	LocalFields []domain.LocalColumn `json:"localFields"`
}

// Versions lists versions, optionally those input from a channel.
func (s *Transactions) Versions(ctx context.Context, channel string) ([]VersionView, error) {
	q := s.a.read(ctx)
	versions, err := store.Select[domain.Version](q, "SELECT * FROM versions ORDER BY name")
	if err != nil {
		return nil, err
	}
	out := make([]VersionView, 0, len(versions))
	for _, v := range versions {
		if channel != "" && (!v.AllowsChannel(channel) || v.Disabled) {
			continue
		}
		view, err := s.versionView(q, &v)
		if err != nil {
			return nil, err
		}
		out = append(out, *view)
	}
	return out, nil
}

// VersionByName returns one version with its screen.
func (s *Transactions) VersionByName(ctx context.Context, name string) (*VersionView, error) {
	q := s.a.read(ctx)
	v, err := s.loadVersion(q, strings.ToUpper(name))
	if err != nil {
		return nil, err
	}
	return s.versionView(q, v)
}

func (s *Transactions) versionView(q store.Q, v *domain.Version) (*VersionView, error) {
	t, err := s.loadType(q, v.TransactionType)
	if err != nil {
		return nil, err
	}
	view := &VersionView{Version: *v, Flow: t.Flow, Inputs: []string{}, LocalFields: []domain.LocalColumn{}}
	for _, f := range domain.FlowFields[t.Flow] {
		if f == "linkCode" {
			continue
		}
		if listed, ok := v.Field(f); v.OpenScreen() || (ok && !listed.Hidden) {
			view.Inputs = append(view.Inputs, f)
		}
	}
	local, err := s.a.LocalTables.schema(q, domain.AppTransaction)
	if err != nil || local == nil {
		return view, err
	}
	for _, f := range local.Fields {
		if _, live := local.live[f.Name]; !live {
			continue
		}
		if listed, ok := v.Field("local." + f.Name); v.OpenScreen() || (ok && !listed.Hidden) {
			view.LocalFields = append(view.LocalFields, f)
		}
	}
	return view, nil
}

// ProposeTransactionType inputs a transaction type or an amendment (INAU).
func (p *Parameters) ProposeTransactionType(ctx context.Context, code string, spec domain.TransactionTypeSpec) (*domain.ParameterChangeRequest, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if err := p.a.Transactions.checkType(p.a.read(ctx), code, &spec); err != nil {
		return nil, err
	}
	return p.propose(ctx, domain.ParamTransactionType, code, spec)
}

func (s *Transactions) checkType(q store.Q, code string, spec *domain.TransactionTypeSpec) error {
	if err := domain.ValidateTypeCode(code); err != nil {
		return err
	}
	if err := spec.Normalize(); err != nil {
		return err
	}
	existing, err := store.Get[domain.TransactionType](q, "SELECT * FROM transaction_types WHERE code = ?", code)
	if err != nil || existing == nil {
		return err
	}
	copy := *existing
	return copy.Apply(*spec, copy.UpdatedAt)
}

func (s *Transactions) applyType(tx *store.Tx, request *domain.ParameterChangeRequest) error {
	var spec domain.TransactionTypeSpec
	if err := json.Unmarshal([]byte(request.Payload), &spec); err != nil {
		return err
	}
	return s.defineType(tx, request.Key, spec)
}

func (s *Transactions) defineType(tx *store.Tx, code string, spec domain.TransactionTypeSpec) error {
	if err := s.checkType(tx, code, &spec); err != nil {
		return err
	}
	t, err := store.Get[domain.TransactionType](tx, "SELECT * FROM transaction_types WHERE code = ?", code)
	if err != nil {
		return err
	}
	if t == nil {
		t = &domain.TransactionType{ID: domain.NewID(), Code: code, CreatedAt: tx.Now()}
		if err := t.Apply(spec, tx.Now()); err != nil {
			return err
		}
		return tx.Insert("transaction_types", t)
	}
	if err := t.Apply(spec, tx.Now()); err != nil {
		return err
	}
	return tx.Update("transaction_types", t)
}

// ProposeVersion inputs a version or an amendment (INAU).
func (p *Parameters) ProposeVersion(ctx context.Context, name string, spec domain.VersionSpec) (*domain.ParameterChangeRequest, error) {
	name = strings.ToUpper(strings.TrimSpace(name))
	if err := p.a.Transactions.checkVersion(p.a.read(ctx), name, &spec); err != nil {
		return nil, err
	}
	return p.propose(ctx, domain.ParamVersion, name, spec)
}

func (s *Transactions) checkVersion(q store.Q, name string, spec *domain.VersionSpec) error {
	if err := domain.ValidateVersionName(name); err != nil {
		return err
	}
	spec.TransactionType = strings.ToUpper(strings.TrimSpace(spec.TransactionType))
	t, err := s.loadType(q, spec.TransactionType)
	if err != nil {
		return err
	}
	if err := spec.Normalize(t.Flow); err != nil {
		return err
	}
	for _, routines := range [][]string{spec.InputRoutines, spec.AuthRoutines} {
		if len(routines) > 0 && len(spec.Channels) == 1 && spec.Channels[0] == domain.ChannelSystem {
			return domain.Rule("invalid_version", "System versions run inside Close of Business, where routines are not consulted.")
		}
		for _, name := range routines {
			known, err := s.a.Extensions.Known(q, name)
			if err != nil {
				return err
			}
			if !known {
				return domain.Rule("unknown_extension", "Routine '%s' is neither compiled in nor a registered endpoint.", name)
			}
		}
	}
	local, err := s.a.LocalTables.schema(q, domain.AppTransaction)
	if err != nil {
		return err
	}
	for _, f := range spec.Fields {
		if name, ok := strings.CutPrefix(f.Field, "local."); ok {
			if local == nil {
				return domain.Rule("unknown_local_field", "Transactions have no local field '%s'.", name)
			}
			if declared, known := local.Fields.Field(name); !known || declared.Retired {
				return domain.Rule("unknown_local_field", "Transactions have no local field '%s'.", name)
			}
		}
	}
	existing, err := store.Get[domain.Version](q, "SELECT * FROM versions WHERE name = ?", name)
	if err != nil || existing == nil {
		return err
	}
	copy := *existing
	return copy.Apply(*spec, copy.UpdatedAt)
}

func (s *Transactions) applyVersion(tx *store.Tx, request *domain.ParameterChangeRequest) error {
	var spec domain.VersionSpec
	if err := json.Unmarshal([]byte(request.Payload), &spec); err != nil {
		return err
	}
	return s.defineVersion(tx, request.Key, spec)
}

func (s *Transactions) defineVersion(tx *store.Tx, name string, spec domain.VersionSpec) error {
	if err := s.checkVersion(tx, name, &spec); err != nil {
		return err
	}
	v, err := store.Get[domain.Version](tx, "SELECT * FROM versions WHERE name = ?", name)
	if err != nil {
		return err
	}
	if v == nil {
		v = &domain.Version{ID: domain.NewID(), Name: name, CreatedAt: tx.Now()}
		if err := v.Apply(spec, tx.Now()); err != nil {
			return err
		}
		return tx.Insert("versions", v)
	}
	if err := v.Apply(spec, tx.Now()); err != nil {
		return err
	}
	return tx.Update("versions", v)
}

// ---------------------------------------------------------------------------------------------
// The system set, present from the first start (amendable under maker-checker)

var systemTypes = map[string]domain.TransactionTypeSpec{
	"AC": {Description: "Account transfer", Flow: domain.FlowTransfer, ReportingClass: "IAT01"},
	"CC": {Description: "Collection through a provider", Flow: domain.FlowCollection},
	"PO": {Description: "Payout to a bank account or mobile wallet", Flow: domain.FlowPayout},
	"RF": {Description: "Refund of a collection", Flow: domain.FlowRefund},
	"OT": {Description: "Outward channel transfer", Flow: domain.FlowChannelOut},
	"IT": {Description: "Inward channel transfer", Flow: domain.FlowChannelIn},
	"DP": {Description: "Deposit reported by a provider", Flow: domain.FlowDeposit},
}

var silent = map[string]domain.OverrideClass{"HIGH.VALUE": domain.OverrideIgnore, "NEW.BENEFICIARY": domain.OverrideIgnore, "BENEFICIARY.RESTRICTED": domain.OverrideIgnore}

var systemVersions = map[string]domain.VersionSpec{
	"AC.API":          {Description: "Account transfer by API (overrides accepted by the caller)", TransactionType: "AC", Channels: []string{domain.ChannelAPI}},
	"AC.CONSOLE":      {Description: "Account transfer input by an operator", TransactionType: "AC", Channels: []string{domain.ChannelConsole}, Authorisers: 1, Overrides: map[string]domain.OverrideClass{"HIGH.VALUE": domain.OverrideChecker, "NEW.BENEFICIARY": domain.OverrideChecker, "BENEFICIARY.RESTRICTED": domain.OverrideChecker}},
	"STO.AC":          {Description: "Standing order transfer", TransactionType: "AC", Channels: []string{domain.ChannelSystem}, Overrides: silent},
	"COLLECTION.API":  {Description: "Collection by API", TransactionType: "CC", Channels: []string{domain.ChannelAPI}},
	"COLLECTION.LINK": {Description: "Collection on a payment link", TransactionType: "CC", Channels: []string{domain.ChannelLink}},
	"PAYOUT.API":      {Description: "Payout by API (risk-scored)", TransactionType: "PO", Channels: []string{domain.ChannelAPI}, Overrides: silent},
	"PAYOUT.CONSOLE":  {Description: "Payout input by an operator", TransactionType: "PO", Channels: []string{domain.ChannelConsole}, Authorisers: 1, Overrides: map[string]domain.OverrideClass{"HIGH.VALUE": domain.OverrideChecker, "NEW.BENEFICIARY": domain.OverrideChecker}},
	"STO.PAYOUT":      {Description: "Standing order payout", TransactionType: "PO", Channels: []string{domain.ChannelSystem}, Overrides: silent},
	"REFUND.API":      {Description: "Refund by API", TransactionType: "RF", Channels: []string{domain.ChannelAPI}},
	"OT.API":          {Description: "Outward channel transfer by API", TransactionType: "OT", Channels: []string{domain.ChannelAPI}, Overrides: silent},
	"OT.CONSOLE":      {Description: "Outward channel transfer input by an operator", TransactionType: "OT", Channels: []string{domain.ChannelConsole}, Authorisers: 1, Overrides: map[string]domain.OverrideClass{"HIGH.VALUE": domain.OverrideChecker}},
	"INWARD.SYSTEM":   {Description: "Inward channel transfer reported by a channel module", TransactionType: "IT", Channels: []string{domain.ChannelSystem}},
	"DEPOSIT.SYSTEM":  {Description: "Deposit reported by a provider", TransactionType: "DP", Channels: []string{domain.ChannelSystem}},
}

// ensureTransactionSet creates the system types and versions that are missing.
func (s *Transactions) ensureTransactionSet(tx *store.Tx) error {
	for code, spec := range systemTypes {
		exists, err := store.Exists(tx, "SELECT 1 FROM transaction_types WHERE code = ?", code)
		if err != nil {
			return err
		}
		if !exists {
			if err := s.defineType(tx, code, spec); err != nil {
				return err
			}
		}
	}
	for name, spec := range systemVersions {
		exists, err := store.Exists(tx, "SELECT 1 FROM versions WHERE name = ?", name)
		if err != nil {
			return err
		}
		if !exists {
			if err := s.defineVersion(tx, name, spec); err != nil {
				return err
			}
		}
	}
	return nil
}
