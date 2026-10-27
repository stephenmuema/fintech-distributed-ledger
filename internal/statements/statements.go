// Package statements parses the account statements correspondents send for our nostro
// accounts: SWIFT MT940/MT950, ISO 20022 camt.053 and a plain CSV. Amounts become int64 minor
// units in the statement currency; directions are the correspondent's (Credit = money in).
package statements

import (
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/stephenmuema/finnie/internal/domain"
)

type Format string

const (
	CSV     Format = "csv"
	MT940   Format = "mt940"
	Camt053 Format = "camt053"
)

// Balance is a statement balance. Amount is signed from our side: positive is a credit balance
// (the correspondent owes us), negative an overdraft.
type Balance struct {
	Date   domain.Date
	Amount int64
}

type Line struct {
	ValueDate         domain.Date
	EntryDate         *domain.Date
	Direction         string // Credit | Debit (the correspondent's books)
	Amount            int64
	BankReference     string
	CustomerReference string
	Narrative         string
}

type Statement struct {
	Format          Format
	Number          string
	Account         string
	Currency        string
	Opening         *Balance
	Closing         *Balance
	Lines           []Line
	FromDate        domain.Date
	ToDate          domain.Date
	OpeningRequired bool
}

func invalid(format string, args ...any) error {
	return domain.Rule("invalid_statement", format, args...)
}

// Parse reads a statement. currency is the nostro's currency: MT940 and camt.053 must agree
// with it, and a CSV is taken to be in it.
func Parse(format Format, content, currency string) (*Statement, error) {
	ccy, err := domain.CurrencyOf(currency)
	if err != nil {
		return nil, err
	}
	var st *Statement
	switch format {
	case CSV:
		st, err = parseCSV(content, ccy)
	case MT940:
		st, err = parseMT940(content, ccy)
	case Camt053:
		st, err = parseCamt053(content, ccy)
	default:
		return nil, invalid("Unknown statement format '%s' (csv, mt940 or camt053).", format)
	}
	if err != nil {
		return nil, err
	}
	if st.Currency != ccy.Code {
		return nil, invalid("The statement is in %s; this nostro is %s.", st.Currency, ccy.Code)
	}
	if len(st.Lines) == 0 && st.Closing == nil {
		return nil, invalid("The statement has no entries and no closing balance.")
	}
	st.Format = format
	for i, l := range st.Lines {
		if i == 0 || l.ValueDate.Before(st.FromDate) {
			st.FromDate = l.ValueDate
		}
		if i == 0 || l.ValueDate.After(st.ToDate) {
			st.ToDate = l.ValueDate
		}
	}
	if st.Closing != nil && st.Closing.Date.After(st.ToDate) {
		st.ToDate = st.Closing.Date
	}
	if st.FromDate.IsZero() && st.Closing != nil {
		st.FromDate = st.Closing.Date
	}
	if st.Opening != nil && st.Closing != nil {
		net := st.Opening.Amount
		for _, l := range st.Lines {
			if l.Direction == "Credit" {
				net += l.Amount
			} else {
				net -= l.Amount
			}
		}
		if net != st.Closing.Amount {
			return nil, invalid("Opening balance plus entries (%d) does not equal the closing balance (%d).", net, st.Closing.Amount)
		}
	}
	return st, nil
}

// minor converts a decimal string ("1234.5", "1234,50") to minor units, exactly.
func minor(s string, ccy domain.Currency) (int64, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > int(ccy.Exponent) {
		if strings.Trim(frac[ccy.Exponent:], "0") != "" {
			return 0, fmt.Errorf("amount %s has more than %d decimals", s, ccy.Exponent)
		}
		frac = frac[:ccy.Exponent]
	}
	frac += strings.Repeat("0", int(ccy.Exponent)-len(frac))
	n, err := strconv.ParseInt(whole+frac, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	return n, nil
}

// ---------------------------------------------------------------------------------------------
// CSV: value_date,direction,amount,bank_reference,customer_reference,narrative (header optional;
// amount in minor units; direction Credit/Debit or C/D).

func parseCSV(content string, ccy domain.Currency) (*Statement, error) {
	r := csv.NewReader(strings.NewReader(content))
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	records, err := r.ReadAll()
	if err != nil {
		return nil, invalid("Malformed CSV: %s.", err)
	}
	st := &Statement{Currency: ccy.Code}
	for i, rec := range records {
		if len(rec) == 1 && strings.TrimSpace(rec[0]) == "" {
			continue
		}
		if i == 0 && strings.EqualFold(strings.TrimSpace(rec[0]), "value_date") {
			continue
		}
		if len(rec) < 3 {
			return nil, invalid("Line %d needs at least value_date, direction and amount.", i+1)
		}
		date, err := domain.ParseDate(strings.TrimSpace(rec[0]))
		if err != nil {
			return nil, invalid("Line %d: %s.", i+1, err)
		}
		direction, ok := directionOf(rec[1])
		if !ok {
			return nil, invalid("Line %d: direction must be Credit or Debit.", i+1)
		}
		amount, err := strconv.ParseInt(strings.TrimSpace(rec[2]), 10, 64)
		if err != nil || amount <= 0 {
			return nil, invalid("Line %d: amount must be a positive whole number of minor units.", i+1)
		}
		line := Line{ValueDate: date, Direction: direction, Amount: amount}
		fields := []*string{&line.BankReference, &line.CustomerReference, &line.Narrative}
		for j, f := range fields {
			if len(rec) > 3+j {
				*f = strings.TrimSpace(rec[3+j])
			}
		}
		st.Lines = append(st.Lines, line)
	}
	return st, nil
}

func directionOf(s string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "C", "CR", "CREDIT", "CRDT":
		return "Credit", true
	case "D", "DR", "DEBIT", "DBIT":
		return "Debit", true
	}
	return "", false
}

// ---------------------------------------------------------------------------------------------
// MT940 / MT950

var (
	mtTag     = regexp.MustCompile(`(?m)^:(\d{2}[A-Z]?):`)
	mtBalance = regexp.MustCompile(`^([CD])(\d{6})([A-Z]{3})([\d,]+)$`)
	// :61: value date, optional entry date (MMDD), mark (C, D, RC, RD), optional funds code,
	// amount, transaction type (N/F/S + 3), customer reference, optional //bank reference.
	mtLine = regexp.MustCompile(`^(\d{6})(\d{4})?(RC|RD|C|D)([A-Z])?([\d,]+)([NFS][A-Z0-9]{3})([^/\r\n]*)(?://([^\r\n]*))?(?:\r?\n(.*))?$`)
)

func mtDate(yymmdd string) (domain.Date, error) {
	t, err := time.Parse("060102", yymmdd)
	if err != nil {
		return domain.Date{}, fmt.Errorf("invalid date %s", yymmdd)
	}
	return domain.DateOf(t), nil
}

func parseMT940(content string, ccy domain.Currency) (*Statement, error) {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if i := strings.Index(content, "{4:"); i >= 0 { // strip SWIFT envelope blocks
		content = content[i+3:]
		content = strings.TrimSuffix(strings.TrimSpace(strings.SplitN(content, "-}", 2)[0]), "-")
	}
	locs := mtTag.FindAllStringSubmatchIndex(content, -1)
	if len(locs) == 0 {
		return nil, invalid("No MT940 fields found.")
	}
	st := &Statement{}
	var last *Line
	for i, loc := range locs {
		tag := content[loc[2]:loc[3]]
		end := len(content)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		value := strings.TrimRight(content[loc[1]:end], "\n ")
		switch tag {
		case "20":
			if st.Number == "" {
				st.Number = strings.TrimSpace(value)
			}
		case "25":
			st.Account = strings.TrimSpace(value)
		case "28C", "28":
			st.Number = strings.TrimSpace(value)
		case "60F", "60M", "62F", "62M":
			b, currency, err := mtParseBalance(value, ccy)
			if err != nil {
				return nil, invalid(":%s: %s.", tag, err)
			}
			st.Currency = currency
			if tag[:2] == "60" && st.Opening == nil {
				st.Opening = b
			}
			if tag[:2] == "62" {
				st.Closing = b
			}
		case "61":
			m := mtLine.FindStringSubmatch(value)
			if m == nil {
				return nil, invalid(":61: line is malformed: %q.", value)
			}
			valueDate, err := mtDate(m[1])
			if err != nil {
				return nil, invalid(":61: %s.", err)
			}
			amount, err := minor(m[5], ccy)
			if err != nil {
				return nil, invalid(":61: %s.", err)
			}
			direction := "Credit"
			if m[3] == "D" || m[3] == "RC" { // a reversal of a credit takes money out
				direction = "Debit"
			}
			line := Line{ValueDate: valueDate, Direction: direction, Amount: amount,
				CustomerReference: strings.TrimSpace(m[7]), BankReference: strings.TrimSpace(m[8]), Narrative: strings.TrimSpace(m[9])}
			if line.CustomerReference == "NONREF" {
				line.CustomerReference = ""
			}
			if m[2] != "" {
				entry, err := time.Parse("0102", m[2])
				if err == nil {
					d := domain.NewDate(valueDate.Y, entry.Month(), entry.Day())
					if d.Before(valueDate.AddDays(-180)) { // entry date in the next year
						d = domain.NewDate(valueDate.Y+1, entry.Month(), entry.Day())
					}
					line.EntryDate = &d
				}
			}
			st.Lines = append(st.Lines, line)
			last = &st.Lines[len(st.Lines)-1]
		case "86":
			if last != nil {
				narrative := strings.Join(strings.Fields(strings.ReplaceAll(value, "\n", " ")), " ")
				if last.Narrative != "" {
					narrative = last.Narrative + " " + narrative
				}
				last.Narrative = narrative
			}
		}
	}
	if st.Currency == "" {
		st.Currency = ccy.Code
	}
	return st, nil
}

func mtParseBalance(value string, ccy domain.Currency) (*Balance, string, error) {
	m := mtBalance.FindStringSubmatch(strings.TrimSpace(value))
	if m == nil {
		return nil, "", fmt.Errorf("balance %q is malformed", value)
	}
	date, err := mtDate(m[2])
	if err != nil {
		return nil, "", err
	}
	parsed, err := domain.CurrencyOf(m[3])
	if err != nil {
		return nil, "", err
	}
	amount, err := minor(m[4], parsed)
	if err != nil {
		return nil, "", err
	}
	if m[1] == "D" {
		amount = -amount
	}
	return &Balance{Date: date, Amount: amount}, parsed.Code, nil
}

// ---------------------------------------------------------------------------------------------
// camt.053

type camtDoc struct {
	Stmts []camtStmt `xml:"BkToCstmrStmt>Stmt"`
}

type camtAmt struct {
	Value string `xml:",chardata"`
	Ccy   string `xml:"Ccy,attr"`
}

type camtDate struct {
	Dt   string `xml:"Dt"`
	DtTm string `xml:"DtTm"`
}

func (d camtDate) date() (domain.Date, error) {
	if d.Dt != "" {
		return domain.ParseDate(d.Dt)
	}
	if len(d.DtTm) >= 10 {
		return domain.ParseDate(d.DtTm[:10])
	}
	return domain.Date{}, fmt.Errorf("missing date")
}

type camtStmt struct {
	ID       string `xml:"Id"`
	SeqNb    string `xml:"ElctrncSeqNb"`
	IBAN     string `xml:"Acct>Id>IBAN"`
	Other    string `xml:"Acct>Id>Othr>Id"`
	Currency string `xml:"Acct>Ccy"`
	Balances []struct {
		Code string   `xml:"Tp>CdOrPrtry>Cd"`
		Amt  camtAmt  `xml:"Amt"`
		Ind  string   `xml:"CdtDbtInd"`
		Dt   camtDate `xml:"Dt"`
	} `xml:"Bal"`
	Entries []struct {
		Amt        camtAmt  `xml:"Amt"`
		Ind        string   `xml:"CdtDbtInd"`
		Reversal   bool     `xml:"RvslInd"`
		BookingDt  camtDate `xml:"BookgDt"`
		ValueDt    camtDate `xml:"ValDt"`
		ServicerRf string   `xml:"AcctSvcrRef"`
		Info       string   `xml:"AddtlNtryInf"`
		EndToEnd   string   `xml:"NtryDtls>TxDtls>Refs>EndToEndId"`
		Ustrd      []string `xml:"NtryDtls>TxDtls>RmtInf>Ustrd"`
	} `xml:"Ntry"`
}

func parseCamt053(content string, ccy domain.Currency) (*Statement, error) {
	var doc camtDoc
	if err := xml.Unmarshal([]byte(content), &doc); err != nil {
		return nil, invalid("Malformed camt.053: %s.", err)
	}
	if len(doc.Stmts) != 1 {
		return nil, invalid("Expected exactly one statement, found %d.", len(doc.Stmts))
	}
	s := doc.Stmts[0]
	st := &Statement{Number: firstNonEmpty(s.SeqNb, s.ID), Account: firstNonEmpty(s.IBAN, s.Other), Currency: s.Currency}
	for _, b := range s.Balances {
		parsed, err := domain.CurrencyOf(b.Amt.Ccy)
		if err != nil {
			return nil, err
		}
		if st.Currency == "" {
			st.Currency = parsed.Code
		}
		amount, err := minor(b.Amt.Value, parsed)
		if err != nil {
			return nil, invalid("Balance %s: %s.", b.Code, err)
		}
		if b.Ind == "DBIT" {
			amount = -amount
		}
		date, err := b.Dt.date()
		if err != nil {
			return nil, invalid("Balance %s: %s.", b.Code, err)
		}
		switch b.Code {
		case "OPBD", "PRCD":
			if st.Opening == nil {
				st.Opening = &Balance{date, amount}
			}
		case "CLBD":
			st.Closing = &Balance{date, amount}
		}
	}
	for i, e := range s.Entries {
		amount, err := minor(e.Amt.Value, ccy)
		if err != nil || amount <= 0 {
			return nil, invalid("Entry %d: invalid amount.", i+1)
		}
		if e.Amt.Ccy != "" && e.Amt.Ccy != ccy.Code {
			return nil, invalid("Entry %d is in %s; the statement is %s.", i+1, e.Amt.Ccy, ccy.Code)
		}
		direction, ok := directionOf(e.Ind)
		if !ok {
			return nil, invalid("Entry %d: CdtDbtInd must be CRDT or DBIT.", i+1)
		}
		valueDate, err := e.ValueDt.date()
		if err != nil {
			if valueDate, err = e.BookingDt.date(); err != nil {
				return nil, invalid("Entry %d: no value or booking date.", i+1)
			}
		}
		line := Line{ValueDate: valueDate, Direction: direction, Amount: amount, BankReference: e.ServicerRf,
			CustomerReference: e.EndToEnd, Narrative: strings.TrimSpace(strings.Join(append([]string{e.Info}, e.Ustrd...), " "))}
		if line.CustomerReference == "NOTPROVIDED" {
			line.CustomerReference = ""
		}
		if booked, err := e.BookingDt.date(); err == nil {
			line.EntryDate = &booked
		}
		st.Lines = append(st.Lines, line)
	}
	return st, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------------------------
// MT940 export (vostro statements we send to correspondents)

// ExportLine is one posting on a vostro account, from the account holder's point of view.
type ExportLine struct {
	ValueDate   domain.Date
	BookingDate domain.Date
	Direction   string // Credit | Debit
	Amount      int64
	Reference   string
	Narrative   string
}

func mtAmount(amount int64, ccy domain.Currency) string {
	if amount < 0 {
		amount = -amount
	}
	s := strconv.FormatInt(amount, 10)
	if ccy.Exponent == 0 {
		return s + ","
	}
	for len(s) <= int(ccy.Exponent) {
		s = "0" + s
	}
	return s[:len(s)-int(ccy.Exponent)] + "," + s[len(s)-int(ccy.Exponent):]
}

func mtMark(amount int64) string {
	if amount < 0 {
		return "D"
	}
	return "C"
}

func mtSafe(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 32 || r > 126 || r == ':' {
			return ' '
		}
		return r
	}, s)
	if len(s) > n {
		s = s[:n]
	}
	return s
}

// WriteMT940 renders an MT940 statement. Balances are signed: positive is a credit balance.
func WriteMT940(reference, account, number string, currency string, opening, closing Balance, lines []ExportLine) (string, error) {
	ccy, err := domain.CurrencyOf(currency)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, ":20:%s\n:25:%s\n:28C:%s\n", mtSafe(reference, 16), mtSafe(account, 35), mtSafe(number, 11))
	fmt.Fprintf(&b, ":60F:%s%s%s%s\n", mtMark(opening.Amount), opening.Date.Format("060102"), ccy.Code, mtAmount(opening.Amount, ccy))
	for _, l := range lines {
		mark := "C"
		if l.Direction == "Debit" {
			mark = "D"
		}
		fmt.Fprintf(&b, ":61:%s%s%s%sNTRF%s//%s\n", l.ValueDate.Format("060102"), l.BookingDate.Format("0102"), mark, mtAmount(l.Amount, ccy),
			mtSafe(l.Reference, 16), mtSafe(l.Reference, 16))
		if l.Narrative != "" {
			fmt.Fprintf(&b, ":86:%s\n", mtSafe(l.Narrative, 390))
		}
	}
	fmt.Fprintf(&b, ":62F:%s%s%s%s\n", mtMark(closing.Amount), closing.Date.Format("060102"), ccy.Code, mtAmount(closing.Amount, ccy))
	return b.String(), nil
}
