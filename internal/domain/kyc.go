package domain

import (
	"time"

	"github.com/google/uuid"
)

// KycSubmissionRecord is what a customer submitted for verification, kept while it waits for an
// operator's review (outside the sandbox, until an identity vendor is integrated) and after.
type KycSubmissionRecord struct {
	ID                 uuid.UUID  `db:"id" json:"id"`
	Version            int64      `db:"version" json:"version"`
	CustomerID         uuid.UUID  `db:"customer_id" json:"customerId"`
	DocumentType       string     `db:"document_type" json:"documentType"`
	DocumentNumber     string     `db:"document_number" json:"documentNumber"`
	DateOfBirth        *Date      `db:"date_of_birth" json:"dateOfBirth"`
	RegistrationNumber *string    `db:"registration_number" json:"registrationNumber"`
	Directors          StringList `db:"directors" json:"directors"`
	SubmittedAt        time.Time  `db:"submitted_at" json:"submittedAt"`
	DecidedAt          *time.Time `db:"decided_at" json:"decidedAt"`
	Decision           *string    `db:"decision" json:"decision"`
}
