package statements

import (
	"strings"
	"testing"

	"github.com/stephenmuema/finnie/internal/domain"
)

const mt940 = `{1:F01KCBLKENXAXXX0000000000}{2:O9401200261028CITIUS33XXXX00000000002610281200N}{4:
:20:STMT261028
:25:1100223344
:28C:00042/001
:60F:C261027KES1000,00
:61:2610281028C500,00NTRFFT26301K3QZ8//KCB0001
:86:INWARD PESALINK FROM AMANI
 WANJIKU
:61:2610281028D125,50NCHGNONREF//KCB0002
:86:SERVICE CHARGE
:62F:C261028KES1374,50
-}`

func TestMT940(t *testing.T) {
	st, err := Parse(MT940, mt940, "KES")
	if err != nil {
		t.Fatal(err)
	}
	if st.Account != "1100223344" || st.Number != "00042/001" || st.Currency != "KES" {
		t.Fatalf("header: %+v", st)
	}
	if st.Opening.Amount != 100_000 || st.Closing.Amount != 137_450 {
		t.Fatalf("balances: %+v %+v", st.Opening, st.Closing)
	}
	if len(st.Lines) != 2 {
		t.Fatalf("lines: %d", len(st.Lines))
	}
	first := st.Lines[0]
	if first.Direction != "Credit" || first.Amount != 50_000 || first.CustomerReference != "FT26301K3QZ8" || first.BankReference != "KCB0001" {
		t.Fatalf("first line: %+v", first)
	}
	if first.Narrative != "INWARD PESALINK FROM AMANI WANJIKU" {
		t.Fatalf("narrative: %q", first.Narrative)
	}
	if second := st.Lines[1]; second.Direction != "Debit" || second.Amount != 12_550 || second.CustomerReference != "" {
		t.Fatalf("second line: %+v", second)
	}
	if st.FromDate != domain.NewDate(2026, 10, 28) || st.ToDate != domain.NewDate(2026, 10, 28) {
		t.Fatalf("period: %s–%s", st.FromDate, st.ToDate)
	}
}

func TestMT940BalancesMustAgree(t *testing.T) {
	_, err := Parse(MT940, strings.Replace(mt940, "C261028KES1374,50", "C261028KES1374,00", 1), "KES")
	if domain.CodeOf(err) != "invalid_statement" {
		t.Fatalf("expected invalid_statement, got %v", err)
	}
}

func TestMT940CurrencyMustMatchTheNostro(t *testing.T) {
	_, err := Parse(MT940, mt940, "USD")
	if domain.CodeOf(err) != "invalid_statement" {
		t.Fatalf("expected invalid_statement, got %v", err)
	}
}

const camt = `<?xml version="1.0" encoding="UTF-8"?>
<Document xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.02">
 <BkToCstmrStmt>
  <Stmt>
   <Id>STMT-1</Id><ElctrncSeqNb>7</ElctrncSeqNb>
   <Acct><Id><Othr><Id>0170299887766</Id></Othr></Id><Ccy>USD</Ccy></Acct>
   <Bal><Tp><CdOrPrtry><Cd>OPBD</Cd></CdOrPrtry></Tp><Amt Ccy="USD">250.00</Amt><CdtDbtInd>CRDT</CdtDbtInd><Dt><Dt>2026-10-27</Dt></Dt></Bal>
   <Bal><Tp><CdOrPrtry><Cd>CLBD</Cd></CdOrPrtry></Tp><Amt Ccy="USD">300.25</Amt><CdtDbtInd>CRDT</CdtDbtInd><Dt><Dt>2026-10-28</Dt></Dt></Bal>
   <Ntry>
    <Amt Ccy="USD">50.25</Amt><CdtDbtInd>CRDT</CdtDbtInd>
    <BookgDt><Dt>2026-10-28</Dt></BookgDt><ValDt><Dt>2026-10-28</Dt></ValDt>
    <AcctSvcrRef>EQ-991</AcctSvcrRef>
    <NtryDtls><TxDtls><Refs><EndToEndId>FX26301AAAAA</EndToEndId></Refs><RmtInf><Ustrd>Invoice 42</Ustrd></RmtInf></TxDtls></NtryDtls>
   </Ntry>
  </Stmt>
 </BkToCstmrStmt>
</Document>`

func TestCamt053(t *testing.T) {
	st, err := Parse(Camt053, camt, "USD")
	if err != nil {
		t.Fatal(err)
	}
	if st.Number != "7" || st.Account != "0170299887766" || st.Opening.Amount != 25_000 || st.Closing.Amount != 30_025 {
		t.Fatalf("header: %+v", st)
	}
	l := st.Lines[0]
	if l.Amount != 5_025 || l.Direction != "Credit" || l.CustomerReference != "FX26301AAAAA" || l.BankReference != "EQ-991" || l.Narrative != "Invoice 42" {
		t.Fatalf("line: %+v", l)
	}
}

func TestCSV(t *testing.T) {
	st, err := Parse(CSV, "value_date,direction,amount,bank_reference,customer_reference,narrative\n2026-10-28,C,15000,B1,REF1,Hello\n2026-10-28,Debit,500,B2,,Fee\n", "KES")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Lines) != 2 || st.Lines[0].Amount != 15_000 || st.Lines[1].Direction != "Debit" || st.Lines[0].CustomerReference != "REF1" {
		t.Fatalf("lines: %+v", st.Lines)
	}
}

func TestMinorUnitsAreExact(t *testing.T) {
	cases := map[string]int64{"1234,5": 123_450, "0,07": 7, "10": 1_000, "1.10": 110}
	for in, want := range cases {
		got, err := minor(in, domain.MustCurrency("KES"))
		if err != nil || got != want {
			t.Fatalf("%s: got %d %v, want %d", in, got, err, want)
		}
	}
	if _, err := minor("1,234", domain.MustCurrency("KES")); err == nil {
		t.Fatal("three decimals in KES must be rejected")
	}
	if got, _ := minor("1500,", domain.MustCurrency("UGX")); got != 1500 {
		t.Fatalf("UGX: %d", got)
	}
}

func TestWriteMT940RoundTrips(t *testing.T) {
	out, err := WriteMT940("VOSTRO1", "1000000016", "1/1", "KES",
		Balance{domain.NewDate(2026, 10, 27), 100_000}, Balance{domain.NewDate(2026, 10, 28), 137_450},
		[]ExportLine{
			{domain.NewDate(2026, 10, 28), domain.NewDate(2026, 10, 28), "Credit", 50_000, "FT26301K3QZ8", "Inward"},
			{domain.NewDate(2026, 10, 28), domain.NewDate(2026, 10, 28), "Debit", 12_550, "FT26301K3QZ9", "Charge"},
		})
	if err != nil {
		t.Fatal(err)
	}
	st, err := Parse(MT940, out, "KES")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if st.Closing.Amount != 137_450 || len(st.Lines) != 2 || st.Lines[0].CustomerReference != "FT26301K3QZ8" {
		t.Fatalf("round trip: %+v", st)
	}
}
