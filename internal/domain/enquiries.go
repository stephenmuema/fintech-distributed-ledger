package domain

import (
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------------------------
// Enquiries (T24: ENQUIRY)
//
// An enquiry is a saved query a bank defines in data: an application (transactions, customers,
// accounts or one of its local applications), fixed selection criteria, the criteria the user
// fills in, the columns and the sort. The core compiles it to SQL itself, from a whitelist of
// fields per application — an enquiry never carries SQL.

// Operand is a selection operator (T24 ENQUIRY operands).
type Operand string

const (
	OpEQ Operand = "EQ" // equal
	OpNE Operand = "NE" // not equal
	OpGT Operand = "GT"
	OpGE Operand = "GE"
	OpLT Operand = "LT"
	OpLE Operand = "LE"
	OpLK Operand = "LK" // contains
	OpRG Operand = "RG" // range: "from,to"
	OpIN Operand = "IN" // one of: "a,b,c"
)

var Operands = []Operand{OpEQ, OpNE, OpGT, OpGE, OpLT, OpLE, OpLK, OpRG, OpIN}

// Criterion is a fixed selection.
type Criterion struct {
	Field string  `json:"field"`
	Op    Operand `json:"op"`
	Value string  `json:"value"`
}

// Prompt is a selection the user fills in when running the enquiry.
type Prompt struct {
	Field     string  `json:"field"`
	Op        Operand `json:"op"`
	Label     string  `json:"label,omitempty"`
	Mandatory bool    `json:"mandatory,omitempty"`
}

// OutputColumn is one column of the result.
type OutputColumn struct {
	Field string `json:"field"`
	Label string `json:"label,omitempty"`
}

// SortKey orders the result.
type SortKey struct {
	Field string `json:"field"`
	Desc  bool   `json:"desc,omitempty"`
}

// EnquiryCriteria is the selection (JSON column).
type EnquiryCriteria struct {
	Fixed   []Criterion `json:"fixed,omitempty"`
	Prompts []Prompt    `json:"prompts,omitempty"`
}

// EnquiryOutput is the columns and sort (JSON column).
type EnquiryOutput struct {
	Columns []OutputColumn `json:"columns"`
	Sort    []SortKey      `json:"sort,omitempty"`
}

// EnquirySpec is what a maker inputs.
type EnquirySpec struct {
	Description string         `json:"description"`
	Application string         `json:"application"`
	Fixed       []Criterion    `json:"fixed,omitempty"`
	Prompts     []Prompt       `json:"prompts,omitempty"`
	Columns     []OutputColumn `json:"columns"`
	Sort        []SortKey      `json:"sort,omitempty"`
	MaxRows     int            `json:"maxRows,omitempty"`
}

var enquiryName = regexp.MustCompile(`^[A-Z][A-Z0-9]*(\.[A-Z0-9]+)*$`)

func ValidateEnquiryName(name string) error {
	if len(name) > 40 || !enquiryName.MatchString(name) {
		return Rule("invalid_enquiry", "An enquiry name is upper case with dots, at most 40 characters (e.g. TXN.BY.MPESA.RECEIPT).")
	}
	return nil
}

func validOperand(op Operand) bool {
	for _, o := range Operands {
		if o == op {
			return true
		}
	}
	return false
}

// Normalize checks the shape of a spec; the fields are checked against the application's
// catalogue by the core.
func (s *EnquirySpec) Normalize() error {
	s.Description = strings.TrimSpace(s.Description)
	s.Application = strings.ToUpper(strings.TrimSpace(s.Application))
	if s.Description == "" || len(s.Description) > 120 {
		return Rule("invalid_enquiry", "A description of at most 120 characters is required.")
	}
	if s.Application == "" {
		return Rule("invalid_enquiry", "Name the application the enquiry reads (TRANSACTION, CUSTOMER, ACCOUNT or a local application).")
	}
	if len(s.Columns) == 0 || len(s.Columns) > 40 {
		return Rule("invalid_enquiry", "An enquiry shows 1 to 40 columns.")
	}
	if len(s.Fixed) > 20 || len(s.Prompts) > 20 || len(s.Sort) > 5 {
		return Rule("invalid_enquiry", "At most 20 fixed criteria, 20 prompts and 5 sort keys.")
	}
	for i := range s.Fixed {
		s.Fixed[i].Op = Operand(strings.ToUpper(string(s.Fixed[i].Op)))
		if !validOperand(s.Fixed[i].Op) {
			return Rule("invalid_enquiry", "Unknown operand '%s' (EQ, NE, GT, GE, LT, LE, LK, RG, IN).", s.Fixed[i].Op)
		}
	}
	for i := range s.Prompts {
		s.Prompts[i].Op = Operand(strings.ToUpper(string(s.Prompts[i].Op)))
		if s.Prompts[i].Op == "" {
			s.Prompts[i].Op = OpEQ
		}
		if !validOperand(s.Prompts[i].Op) {
			return Rule("invalid_enquiry", "Unknown operand '%s' (EQ, NE, GT, GE, LT, LE, LK, RG, IN).", s.Prompts[i].Op)
		}
	}
	if s.MaxRows == 0 {
		s.MaxRows = 200
	}
	if s.MaxRows < 1 || s.MaxRows > 1000 {
		return Rule("invalid_enquiry", "An enquiry returns at most 1000 rows.")
	}
	return nil
}

// Enquiry is an authorised enquiry (historised).
type Enquiry struct {
	ID          uuid.UUID               `db:"id" json:"id"`
	Version     int64                   `db:"version" json:"version"`
	Name        string                  `db:"name" json:"name"`
	Description string                  `db:"description" json:"description"`
	Application string                  `db:"application" json:"application"`
	Criteria    JSONOf[EnquiryCriteria] `db:"criteria" json:"criteria"`
	Output      JSONOf[EnquiryOutput]   `db:"output" json:"output"`
	MaxRows     int                     `db:"max_rows" json:"maxRows"`
	CreatedAt   time.Time               `db:"created_at" json:"createdAt"`
	UpdatedAt   time.Time               `db:"updated_at" json:"updatedAt"`
}

func (e *Enquiry) HistoryName() string { return "Enquiry" }

func (e *Enquiry) Apply(s EnquirySpec, now time.Time) {
	e.Description, e.Application, e.MaxRows, e.UpdatedAt = s.Description, s.Application, s.MaxRows, now
	e.Criteria = JSONOf[EnquiryCriteria]{EnquiryCriteria{Fixed: s.Fixed, Prompts: s.Prompts}}
	e.Output = JSONOf[EnquiryOutput]{EnquiryOutput{Columns: s.Columns, Sort: s.Sort}}
}

func (e *Enquiry) Spec() EnquirySpec {
	return EnquirySpec{Description: e.Description, Application: e.Application, Fixed: e.Criteria.V.Fixed, Prompts: e.Criteria.V.Prompts,
		Columns: e.Output.V.Columns, Sort: e.Output.V.Sort, MaxRows: e.MaxRows}
}
