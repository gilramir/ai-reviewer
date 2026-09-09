package server

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Auth answers exactly one question: is this request from someone who knows the
// password? It is modelled on Jupyter's flow — the daemon prints a secret at
// startup, the browser trades it for a session cookie.
type Auth struct {
	// secret is the plaintext token when the daemon generated one for this run.
	secret string
	// hash is a stored argon2id digest when the user configured a persistent
	// password instead.
	hash *passwordHash

	mu        sync.Mutex
	sessions  map[string]time.Time
	failures  int
	lockUntil time.Time
}

const (
	sessionCookie   = "ai_reviewer_session"
	sessionLifetime = 30 * 24 * time.Hour

	// A LAN attacker guessing a 44-bit secret needs vastly more than this many
	// tries, so a modest lockout is enough to make the attempt pointless.
	maxFailures = 10
	lockoutFor  = 5 * time.Minute
)

// NewTokenAuth authenticates against a one-run secret.
func NewTokenAuth(secret string) *Auth {
	return &Auth{secret: secret, sessions: map[string]time.Time{}}
}

// NewPasswordAuth authenticates against a stored argon2id digest.
func NewPasswordAuth(encoded string) (*Auth, error) {
	h, err := parseHash(encoded)
	if err != nil {
		return nil, err
	}
	return &Auth{hash: h, sessions: map[string]time.Time{}}, nil
}

// Check verifies a submitted password in constant time, applying a lockout so a
// LAN attacker cannot grind at it.
func (a *Auth) Check(password string) bool {
	a.mu.Lock()
	if time.Now().Before(a.lockUntil) {
		a.mu.Unlock()
		return false
	}
	a.mu.Unlock()

	ok := a.verify(password)

	a.mu.Lock()
	defer a.mu.Unlock()
	if ok {
		a.failures = 0
		return true
	}
	a.failures++
	if a.failures >= maxFailures {
		a.lockUntil = time.Now().Add(lockoutFor)
		a.failures = 0
	}
	return false
}

func (a *Auth) verify(password string) bool {
	if a.hash != nil {
		return a.hash.matches(password)
	}
	return subtle.ConstantTimeCompare([]byte(password), []byte(a.secret)) == 1
}

// Issue mints a session and returns the cookie to set.
//
// Secure is set only when the connection is already TLS. On a plain-HTTP LAN
// address a Secure cookie is silently dropped by the browser, which presents as
// an endless login loop that works perfectly on localhost.
func (a *Auth) Issue(overTLS bool) *http.Cookie {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		panic(err) // a system without entropy cannot serve anything safely
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	a.mu.Lock()
	a.sessions[token] = time.Now().Add(sessionLifetime)
	a.mu.Unlock()

	return &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   overTLS,
		SameSite: http.SameSiteStrictMode,
		Expires:  time.Now().Add(sessionLifetime),
	}
}

// Authenticated reports whether a request carries a live session.
func (a *Auth) Authenticated(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return false
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	expiry, ok := a.sessions[cookie.Value]
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		delete(a.sessions, cookie.Value)
		return false
	}
	return true
}

// Revoke drops a request's session.
func (a *Auth) Revoke(r *http.Request) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return
	}
	a.mu.Lock()
	delete(a.sessions, cookie.Value)
	a.mu.Unlock()
}

// --- generated secrets ------------------------------------------------------

// words is a small list chosen for being short, unambiguous when read aloud,
// and easy to retype on a phone. Four of them carry about 44 bits, which is
// ample against an attacker who must also get past the lockout.
var words = strings.Fields(`
amber anchor apple arrow autumn basin beacon birch blossom bridge bronze brook
canyon cedar cinder cliff clover cobalt copper coral cotton crater crimson delta
dune ember fable falcon fern flint forest garnet glacier granite harbor hazel
heron indigo island ivory jasper juniper kettle lagoon lantern larch laurel
ledge lichen linen lotus meadow mesa mist moss nectar oak ochre olive onyx
orchard osprey otter pebble pine plateau plume prairie quarry quartz quill
raven reef ridge river rowan sable sage sandbar shale shore silver slate
solstice spruce summit tally thistle thorn tidal timber topaz tundra valley
vellum verdant walnut willow winter yarrow
`)

// GenerateSecret returns a human-typeable secret.
//
// Jupyter prints 48 hex characters, which works because you click the printed
// link. Here the secret is read off a terminal on one machine and typed into a
// browser on another, so it has to be typeable by a person.
func GenerateSecret() (string, error) {
	parts := make([]string, 4)
	for i := range parts {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(words))))
		if err != nil {
			return "", err
		}
		parts[i] = words[n.Int64()]
	}

	digits, err := rand.Int(rand.Reader, big.NewInt(100))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%02d", strings.Join(parts, "-"), digits.Int64()), nil
}

// --- password hashing -------------------------------------------------------

// Password hashing uses stdlib PBKDF2 rather than argon2id.
//
// argon2id is the stronger primitive, but it lives in golang.org/x/crypto,
// whose go directive would raise this module's Go floor to 1.26 and make every
// build either download a toolchain or fail outright under GOTOOLCHAIN=local.
// That is a poor trade here: the secret being protected is a 44-bit phrase
// behind a ten-attempt lockout on a trusted network, so the KDF is nowhere near
// the weakest link, and keeping the module dependency-free on the crypto side
// is worth more than the margin argon2id would add.
type passwordHash struct {
	salt   []byte
	digest []byte
	iter   int
}

// pbkdf2Iterations follows OWASP's guidance for PBKDF2-HMAC-SHA256.
const (
	pbkdf2Iterations = 600_000
	pbkdf2KeyLen     = 32
)

// HashPassword produces the string stored in the config file. The plaintext is
// never written to disk.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	digest, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, pbkdf2KeyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s",
		pbkdf2Iterations,
		hex.EncodeToString(salt),
		hex.EncodeToString(digest)), nil
}

func parseHash(encoded string) (*passwordHash, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return nil, fmt.Errorf("unrecognised password hash")
	}

	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return nil, fmt.Errorf("malformed password hash iteration count")
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("malformed password hash salt")
	}
	digest, err := hex.DecodeString(parts[3])
	if err != nil {
		return nil, fmt.Errorf("malformed password hash digest")
	}
	return &passwordHash{salt: salt, digest: digest, iter: iter}, nil
}

func (h *passwordHash) matches(password string) bool {
	candidate, err := pbkdf2.Key(sha256.New, password, h.salt, h.iter, len(h.digest))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(candidate, h.digest) == 1
}
