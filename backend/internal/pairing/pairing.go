// Package pairing hands a device its first credential.
//
// A freshly installed desktop client has nothing: no session cookie, no token. It must not ask for a
// password either — a native app collecting the account password is the one habit that trains people
// to type it into anything that asks. So the exchange is inverted: the BROWSER, where the user is
// already authenticated, mints a short code, and the app trades that code for a bearer token. The code
// is a credential for exactly one exchange, and the user carries it the few metres between two windows.
//
// Codes live in memory only. They are valid for minutes and single-use, so persisting them would buy
// nothing but a file full of live credentials; a daemon restart just means asking for a fresh code.
package pairing

import (
	"crypto/rand"
	"strings"
	"sync"
	"time"
)

// alphabet is Crockford base32: the digits and letters that survive being read off one screen and
// typed into another. I, L, O and U are absent — the first three because they are 1/1/0 to a reader,
// U because dropping it keeps the accidental-word rate down.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// codeLen of 8 over a 32-symbol alphabet is 40 bits. Against a single-use code that expires in minutes
// that is not a close call: an attacker guessing at a thousand tries a second for the whole window is
// still nine orders of magnitude short of the handful of codes ever live at once.
const codeLen = 8

// Codes issues and redeems pairing codes.
type Codes struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]entry
}

type entry struct {
	subject string
	expires time.Time
}

// New returns a code pool whose codes stay claimable for ttl.
func New(ttl time.Duration) *Codes {
	return &Codes{ttl: ttl, m: map[string]entry{}}
}

// Issue mints a code for subject and returns it with its absolute expiry.
func (c *Codes) Issue(subject string) (string, time.Time, error) {
	raw := make([]byte, codeLen)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	b := make([]byte, codeLen)
	for i, v := range raw {
		// len(alphabet) is 32 and divides 256, so the modulo is uniform — no bias to reject-sample away.
		b[i] = alphabet[int(v)%len(alphabet)]
	}
	code := string(b)
	exp := time.Now().Add(c.ttl)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune()
	c.m[code] = entry{subject: subject, expires: exp}
	return code, exp, nil
}

// Claim redeems a code and reports the subject it was minted for. A code works exactly once: it is
// spent on being looked at, whether or not the caller goes on to do anything with what it bought.
func (c *Codes) Claim(code string) (string, bool) {
	code = normalize(code)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune()
	e, ok := c.m[code]
	if !ok {
		return "", false
	}
	delete(c.m, code)
	return e.subject, true
}

// normalize accepts a code the way a person hands one over: in whatever case they typed, and with the
// dashes or spaces they used to break it into readable runs.
func normalize(code string) string {
	return strings.NewReplacer("-", "", " ", "").Replace(strings.ToUpper(strings.TrimSpace(code)))
}

// prune drops expired codes. The caller holds the mutex. Sweeping on every Issue and Claim keeps the
// map the size of what is actually in flight, which is a handful — no reaper goroutine to own.
func (c *Codes) prune() {
	now := time.Now()
	for k, e := range c.m {
		if !e.expires.After(now) {
			delete(c.m, k)
		}
	}
}
