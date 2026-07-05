package auth

import (
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// TestDummyHashCostMatchesRealHashes locks the timing-defense invariant: the
// dummy hash the "unknown user" branch compares against must be at the same
// bcrypt cost as hashes produced by HashPassword. A cost mismatch makes a
// missing-user login measurably slower than a wrong-password login and leaks
// username existence by timing.
func TestDummyHashCostMatchesRealHashes(t *testing.T) {
	realHash, err := HashPassword("some-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	realCost, err := bcrypt.Cost([]byte(realHash))
	if err != nil {
		t.Fatalf("cost of real hash: %v", err)
	}
	dummyCost, err := bcrypt.Cost(dummyHash)
	if err != nil {
		t.Fatalf("cost of dummy hash: %v", err)
	}
	if dummyCost != realCost {
		t.Errorf("dummy hash cost = %d, real hash cost = %d; must match to preserve the timing defense", dummyCost, realCost)
	}
}
