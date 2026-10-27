package domain

import (
	"crypto/rand"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
)

// NewID returns a time-ordered UUIDv7, which keeps B-tree inserts append-only.
func NewID() uuid.UUID { return uuid.Must(uuid.NewV7()) }

// N formats an id as 32 hex digits without hyphens (used inside references, e.g. payment:{N}).
func N(id uuid.UUID) string { return hex.EncodeToString(id[:]) }

// RandomString returns n characters drawn uniformly from alphabet using crypto/rand.
func RandomString(alphabet string, n int) string {
	var b strings.Builder
	max := big.NewInt(int64(len(alphabet)))
	for range n {
		i, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		b.WriteByte(alphabet[i.Int64()])
	}
	return b.String()
}

// RandomInt returns a uniform random integer in [0, n) from crypto/rand.
func RandomInt(n int64) int64 {
	i, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		panic(err)
	}
	return i.Int64()
}

// RandomHex returns 2n lowercase hex characters from n random bytes.
func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// ParseEnum matches s case-insensitively against the allowed values.
func ParseEnum[T ~string](s string, values ...T) (T, bool) {
	for _, v := range values {
		if strings.EqualFold(string(v), s) {
			return v, true
		}
	}
	var zero T
	return zero, false
}

// Ptr returns a pointer to v.
func Ptr[T any](v T) *T { return &v }

// Date is a calendar date without time or zone (a business date, T24 style).
// It is stored and serialised as YYYY-MM-DD, which also sorts correctly as text.
type Date struct {
	Y int
	M time.Month
	D int
}

func NewDate(y int, m time.Month, d int) Date {
	return DateOf(time.Date(y, m, d, 0, 0, 0, 0, time.UTC))
}

// DateOf returns the calendar date of t in t's location.
func DateOf(t time.Time) Date {
	y, m, d := t.Date()
	return Date{y, m, d}
}

func ParseDate(s string) (Date, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return Date{}, fmt.Errorf("invalid date %q: expected YYYY-MM-DD", s)
	}
	return DateOf(t), nil
}

func (d Date) Time() time.Time             { return time.Date(d.Y, d.M, d.D, 0, 0, 0, 0, time.UTC) }
func (d Date) AddDays(n int) Date          { return DateOf(d.Time().AddDate(0, 0, n)) }
func (d Date) Weekday() time.Weekday       { return d.Time().Weekday() }
func (d Date) YearDay() int                { return d.Time().YearDay() }
func (d Date) Before(o Date) bool          { return d.Time().Before(o.Time()) }
func (d Date) After(o Date) bool           { return d.Time().After(o.Time()) }
func (d Date) IsZero() bool                { return d == Date{} }
func (d Date) String() string              { return d.Time().Format(time.DateOnly) }
func (d Date) DaysUntil(o Date) int        { return int(o.Time().Sub(d.Time()).Hours() / 24) }
func (d Date) Format(layout string) string { return d.Time().Format(layout) }

func (d Date) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *Date) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := ParseDate(s)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

func (d Date) Value() (driver.Value, error) { return d.String(), nil }

func (d *Date) Scan(src any) error {
	switch v := src.(type) {
	case string:
		parsed, err := ParseDate(v)
		*d = parsed
		return err
	case []byte:
		parsed, err := ParseDate(string(v))
		*d = parsed
		return err
	case time.Time:
		*d = DateOf(v.UTC())
		return nil
	}
	return fmt.Errorf("cannot scan %T into Date", src)
}
