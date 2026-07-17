package pairing

import (
	"strings"
	"testing"
	"time"
)

func TestIssueAndClaim(t *testing.T) {
	c := New(time.Minute)
	code, exp, err := c.Issue("alice")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if len(code) != codeLen {
		t.Fatalf("code %q: want %d chars, got %d", code, codeLen, len(code))
	}
	if !exp.After(time.Now()) {
		t.Fatalf("expiry %v is not in the future", exp)
	}
	subject, ok := c.Claim(code)
	if !ok || subject != "alice" {
		t.Fatalf("claim: got (%q, %v), want (alice, true)", subject, ok)
	}
}

// A code is a credential, so spending it must be final: a replayed code is how a shoulder-surfed or
// logged pairing string would become a second, unnoticed token.
func TestClaimIsSingleUse(t *testing.T) {
	c := New(time.Minute)
	code, _, _ := c.Issue("alice")
	if _, ok := c.Claim(code); !ok {
		t.Fatal("first claim failed")
	}
	if subject, ok := c.Claim(code); ok {
		t.Fatalf("second claim succeeded for %q", subject)
	}
}

func TestClaimRejectsUnknown(t *testing.T) {
	c := New(time.Minute)
	if _, ok := c.Claim("ZZZZZZZZ"); ok {
		t.Fatal("claimed a code that was never issued")
	}
}

func TestExpiredCodeIsRejected(t *testing.T) {
	c := New(-time.Second) // already expired the moment it is minted
	code, _, _ := c.Issue("alice")
	if _, ok := c.Claim(code); ok {
		t.Fatal("claimed an expired code")
	}
}

// The user retypes what they read, so case and the runs they break it into must not matter.
func TestClaimNormalizesWhatAPersonTypes(t *testing.T) {
	c := New(time.Minute)
	code, _, _ := c.Issue("alice")
	typed := "  " + strings.ToLower(code[:4]) + "-" + strings.ToLower(code[4:]) + " "
	subject, ok := c.Claim(typed)
	if !ok || subject != "alice" {
		t.Fatalf("claim(%q): got (%q, %v), want (alice, true)", typed, subject, ok)
	}
}

// Every symbol must come from the readable alphabet — a code with an I or O in it is one the user
// cannot reliably retype, which defeats the point of the format.
func TestCodesUseOnlyTheReadableAlphabet(t *testing.T) {
	c := New(time.Minute)
	for i := 0; i < 200; i++ {
		code, _, err := c.Issue("alice")
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		for _, r := range code {
			if !strings.ContainsRune(alphabet, r) {
				t.Fatalf("code %q contains %q, which is not in the alphabet", code, r)
			}
		}
	}
}

// Two codes minted back to back must differ, or "single-use" would be a property of the wrong thing.
func TestCodesAreDistinct(t *testing.T) {
	c := New(time.Minute)
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		code, _, _ := c.Issue("alice")
		if seen[code] {
			t.Fatalf("code %q issued twice", code)
		}
		seen[code] = true
	}
}

// Expired codes must not accumulate: the pool is swept on use, and nothing else owns that job.
func TestExpiredCodesArePruned(t *testing.T) {
	c := New(-time.Second)
	for i := 0; i < 10; i++ {
		c.Issue("alice")
	}
	c.Issue("bob") // the sweep runs on Issue, so this one clears the ten dead ones before landing
	if n := len(c.m); n != 1 {
		t.Fatalf("pool holds %d codes, want 1 (the live one)", n)
	}
}
