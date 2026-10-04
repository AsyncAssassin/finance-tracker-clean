package domain

import (
	"bufio"
	_ "embed"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/pkg/validation"
)

// Password length in characters. The policy follows NIST SP 800-63B-4 for
// passwords that are the only authentication factor: at least 15
// characters, long passphrases allowed, no rules about character classes,
// and a check against common and context-specific passwords.
const (
	MinPasswordLen = 15
	MaxPasswordLen = 128
)

// NormalizePassword applies Unicode NFKC normalization, so the same
// password typed on different keyboards or systems, with letters such as
// "й" composed or decomposed, gives the same bytes to hash.
//
// The result has at most MaxPasswordLen+1 characters. Normalization runs
// segment by segment and stops as soon as the result is longer than any
// password the rules accept. NFKC can make a text up to 18 times longer
// (U+FDFA alone becomes 18 characters), so normalizing an oversized input
// to the end would take memory in proportion to its size before the
// length check could reject it; this way the cost does not depend on the
// input. The cut result fails the length check of ValidateNewAccount and
// ValidateLogin, and it can never match the hash of an accepted password,
// which has at most MaxPasswordLen characters.
func NormalizePassword(s string) string {
	const limit = MaxPasswordLen + 1
	var it norm.Iter
	it.InitString(norm.NFKC, s)
	// Every segment is at most norm.MaxSegmentSize bytes, so the buffer
	// never needs more than this.
	out := make([]byte, 0, min(len(s), limit*utf8.UTFMax+norm.MaxSegmentSize))
	n := 0
	for !it.Done() && n <= MaxPasswordLen {
		seg := it.Next()
		out = append(out, seg...)
		n += utf8.RuneCount(seg)
	}
	if n > limit {
		// Keep exactly limit characters: enough to fail the length check.
		cut := 0
		for range limit {
			_, size := utf8.DecodeRune(out[cut:])
			cut += size
		}
		out = out[:cut]
	}
	return string(out)
}

func passwordLen(s string) int { return utf8.RuneCountInString(s) }

//go:embed blocklist.txt
var blocklistText string

// blocklist holds well-known passwords of at least MinPasswordLen
// characters, lowercased; shorter ones fail the length check anyway.
var blocklist = parseBlocklist(blocklistText)

func parseBlocklist(text string) map[string]bool {
	set := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			set[strings.ToLower(line)] = true
		}
	}
	return set
}

// validatePassword checks a normalized password of a new account with the
// normalized email of that account.
func validatePassword(v *validation.Error, password, email string) {
	n := passwordLen(password)
	switch {
	case password == "":
		v.Add("password", "is required")
	case !utf8.ValidString(password):
		v.Add("password", "must be valid UTF-8 text")
	case n < MinPasswordLen:
		v.Add("password", "must be at least 15 characters; a phrase of a few words works well")
	case n > MaxPasswordLen:
		v.Add("password", "must be at most 128 characters")
	case guessable(password, email):
		v.Add("password", "is too common or easy to guess, choose another one")
	}
}

// guessable reports passwords an attacker tries first: well-known ones,
// the email or its name part, and one character repeated.
func guessable(password, email string) bool {
	p := strings.ToLower(password)
	if blocklist[p] || p == email {
		return true
	}
	if at := strings.LastIndexByte(email, '@'); at > 0 && p == email[:at] {
		return true
	}
	first, _ := utf8.DecodeRuneInString(p)
	return strings.Trim(p, string(first)) == ""
}
