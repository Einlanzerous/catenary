package media

// CANT-311 — the stable signed URL CANT-47 picked: /media/{key}?kid=<n>&sig=<b64url>,
// sig = HMAC-SHA256(secret[kid], "catenary-media/v1\n" + key).
//
// NO EXPIRY AND NO VIEWER in the signature, so a URL is a pure function of the
// key: /sync, the hub and REST derive byte-identical URLs, and a journaled
// record never goes stale. Revocation is deleting the blob or retiring a kid.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// MinKeyBytes is the shortest secret accepted: 32 bytes, 64 hex characters.
const MinKeyBytes = 32

const signContext = "catenary-media/v1\n"

// Signer signs with its newest key and verifies against all of them.
type Signer struct {
	keys    map[int][]byte
	signing int
}

// ParseKeys reads CATENARY_MEDIA_URL_KEYS: "1=<hex>,2=<hex>". The LAST entry
// signs; every entry verifies, which is what lets a key be rotated in while
// URLs signed by the older kid keep working. Errors name the variable.
func ParseKeys(raw string) (*Signer, error) {
	const name = "CATENARY_MEDIA_URL_KEYS"
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("%s is required when CATENARY_MEDIA_DIR is set (e.g. 1=<64 hex characters>)", name)
	}
	s := &Signer{keys: map[int][]byte{}}
	for _, part := range strings.Split(raw, ",") {
		kidStr, hexKey, ok := strings.Cut(strings.TrimSpace(part), "=")
		kid, err := strconv.Atoi(kidStr)
		if !ok || err != nil || kid < 1 {
			return nil, fmt.Errorf("%s: entry must be <kid>=<hex> with a positive integer kid", name)
		}
		if _, dup := s.keys[kid]; dup {
			return nil, fmt.Errorf("%s: kid %d appears twice", name, kid)
		}
		key, err := hex.DecodeString(hexKey)
		if err != nil {
			return nil, fmt.Errorf("%s: the key for kid %d is not hex", name, kid)
		}
		if len(key) < MinKeyBytes {
			return nil, fmt.Errorf("%s: the key for kid %d is %d bytes; at least %d are required", name, kid, len(key), MinKeyBytes)
		}
		s.keys[kid] = key
		s.signing = kid
	}
	return s, nil
}

// KIDs lists the known key ids, ascending.
func (s *Signer) KIDs() []int {
	out := make([]int, 0, len(s.keys))
	for k := range s.keys {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

func (s *Signer) mac(kid int, key string) ([]byte, bool) {
	secret, ok := s.keys[kid]
	if !ok {
		return nil, false
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(signContext + key))
	return m.Sum(nil), true
}

// URL is the served URL for a storage key, signed with the newest key.
func (s *Signer) URL(key string) string {
	sum, _ := s.mac(s.signing, key)
	return "/media/" + url.PathEscape(key) + "?kid=" + strconv.Itoa(s.signing) +
		"&sig=" + base64.RawURLEncoding.EncodeToString(sum)
}

// Verify reports whether sig is a valid signature of key under kid. An unknown
// kid, a malformed sig and a wrong sig are all just false, and the comparison
// is constant-time.
func (s *Signer) Verify(key string, kid int, sig string) bool {
	want, ok := s.mac(kid, key)
	if !ok {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return false
	}
	return hmac.Equal(got, want)
}

// ErrNoKeys is for callers that need to say a Signer is missing.
var ErrNoKeys = errors.New("media: no URL signing keys")
