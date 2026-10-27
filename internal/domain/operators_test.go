package domain

import (
	"strings"
	"testing"
	"time"
)

func TestPasswordsAreHashedWithArgon2idAndVerifiedExactly(t *testing.T) {
	hash := HashPassword("correct horse battery")
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("unexpected hash format %q", hash)
	}
	if hash == HashPassword("correct horse battery") {
		t.Fatal("hashes must be salted")
	}
	if !VerifyPassword("correct horse battery", hash) || VerifyPassword("correct horse batterY", hash) || VerifyPassword("x", "garbage") {
		t.Fatal("verification is wrong")
	}
}

func TestOperatorsAreLockedOutAfterRepeatedFailures(t *testing.T) {
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	o := NewOperator("amina", OperatorSpec{Name: "Amina", Email: "a@example.com", Permissions: []string{PermMaker}}, now)
	if err := o.CanLogIn(now); CodeOf(err) != "password_not_issued" {
		t.Fatalf("a new operator has no password yet: %v", err)
	}
	if err := o.SetPassword("Amina-1234567", true, now); CodeOf(err) != "weak_password" {
		t.Fatalf("passwords must not contain the username: %v", err)
	}
	if err := o.SetPassword("long-enough-secret", true, now); err != nil {
		t.Fatal(err)
	}
	for range MaxFailedLogins {
		o.RecordFailedLogin(now)
	}
	if err := o.CanLogIn(now.Add(LockoutDuration - time.Second)); CodeOf(err) != "operator_locked" {
		t.Fatalf("expected a lock-out, got %v", err)
	}
	if err := o.CanLogIn(now.Add(LockoutDuration)); err != nil {
		t.Fatalf("the lock-out ends: %v", err)
	}
}

func TestSessionsExpireWhenIdleAndAfterTheirMaximumLife(t *testing.T) {
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	token, s := NewSessionToken(NewID(), "127.0.0.1", "test", now)
	if s.TokenHash != TokenHash(token) || strings.Contains(s.TokenHash, token) {
		t.Fatal("only the token's hash is stored")
	}
	if !s.Live(now.Add(SessionIdle-time.Second)) || s.Live(now.Add(SessionIdle)) {
		t.Fatal("idle timeout")
	}
	s.LastSeenAt = now.Add(SessionMax - time.Minute)
	if s.Live(now.Add(SessionMax)) {
		t.Fatal("absolute timeout")
	}
}
