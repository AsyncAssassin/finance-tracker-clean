package domain

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// normalizationAtoms are short texts with every kind of change NFKC makes:
// composition, decomposition, compatibility mappings, the widest expansion,
// runs of combining marks, a joiner and invalid bytes.
var normalizationAtoms = []string{
	"a", "Z", "7", " ", "-",
	"\u0439", "\u0438\u0306", "\u0451", "\u0435\u0308", // composed and decomposed Cyrillic letters
	"\u00e9", "e\u0301", "\u1ead", "a\u0323\u0302", // Latin letters with one and two marks
	"\u1f82", "\u03b1\u0313\u0300\u0345", // Greek: four code points compose into one
	"\ud55c", "\u1112\u1161\u11ab", // a Hangul syllable and its jamo
	"\ufb01", "\ufb03", "\u2460", "\uff21", "\uff11", "\u338f", "\u2122", "\u00bd", // compatibility characters
	"\ufdfa",                     // the widest NFKC expansion: 18 characters
	"\u0301", "\u0323", "\u034f", // lone combining marks and the combining grapheme joiner
	"\U0001f469\u200d\U0001f4bb",                                     // an emoji sequence with a zero-width joiner
	"\xff", "\xc3\x28", "\xed\xa0\x80", "\xf0\x9f\x98", "\xf2\u02e1", // invalid UTF-8, alone and before a mapped character
}

// normalizationInputs returns the atoms, a few edge cases and random
// combinations of the atoms, short and long.
func normalizationInputs() []string {
	inputs := append([]string(nil), normalizationAtoms...)
	inputs = append(inputs,
		"",
		strings.Repeat("\u0301", 100), // more combining marks than a stream-safe segment holds
		"a"+strings.Repeat("\u0301", 100),
		strings.Repeat("\ufdfa", 7)+"ab", // exactly MaxPasswordLen characters after normalization
		strings.Repeat("\ufdfa", 8),      // 144 characters from 8 code points
		strings.Repeat("\u0438\u0306\u0435\u0308", 64), // 128 characters from 256 code points
		strings.Repeat("x", MaxPasswordLen),
		strings.Repeat("x", MaxPasswordLen+1),
		strings.Repeat("0", MaxPasswordLen+1)+"\x9c", // cut before the invalid byte
	)
	rng := rand.New(rand.NewPCG(2026, 10))
	for range 3000 {
		var b strings.Builder
		for range 1 + rng.IntN(40) {
			b.WriteString(normalizationAtoms[rng.IntN(len(normalizationAtoms))])
		}
		inputs = append(inputs, b.String())
	}
	return inputs
}

// normalizationMismatch checks NormalizePassword(in) against the library.
// For valid UTF-8 the result must equal the full NFKC form when that form
// fits in MaxPasswordLen characters, and its first MaxPasswordLen+1
// characters otherwise. Invalid bytes pass through normalization, and the
// library's String and Iter differ in what they still normalize after
// one; such a result must only be rejected like the input: it keeps an
// invalid byte, which fails the UTF-8 rule and matches no stored
// password, or it was cut before one and is over the length limit. It
// returns "" when the result is right.
func normalizationMismatch(in string) string {
	got := NormalizePassword(in)
	if !utf8.ValidString(in) {
		if n := passwordLen(got); n > MaxPasswordLen+1 || n <= MaxPasswordLen && utf8.ValidString(got) {
			return fmt.Sprintf("NormalizePassword(%q) = %q, want an invalid result or one of exactly %d characters", in, got, MaxPasswordLen+1)
		}
		return ""
	}
	want := norm.NFKC.String(in)
	if utf8.RuneCountInString(want) <= MaxPasswordLen {
		if got != want {
			return fmt.Sprintf("NormalizePassword(%q) = %q, want the NFKC form %q", in, got, want)
		}
		return ""
	}
	if passwordLen(got) != MaxPasswordLen+1 || !strings.HasPrefix(want, got) {
		return fmt.Sprintf("NormalizePassword(%q) has %d characters, want the first %d characters of the NFKC form",
			in, passwordLen(got), MaxPasswordLen+1)
	}
	return ""
}

func TestNormalizePasswordMatchesNFKC(t *testing.T) {
	t.Parallel()
	reported := 0
	for _, in := range normalizationInputs() {
		if msg := normalizationMismatch(in); msg != "" {
			if reported++; reported > 5 {
				t.Fatal("more mismatches follow")
			}
			t.Error(msg)
		}
	}
}

func FuzzNormalizePassword(f *testing.F) {
	for _, in := range normalizationInputs()[:60] {
		f.Add(in)
	}
	f.Fuzz(func(t *testing.T, in string) {
		if msg := normalizationMismatch(in); msg != "" {
			t.Error(msg)
		}
	})
}

// allocatedPerCall returns the bytes one call of f allocates, averaged over
// a few sequential calls on one processor, so other goroutines add little.
func allocatedPerCall(f func()) uint64 {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	f() // a first call may allocate what later calls reuse
	const runs = 5
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range runs {
		f()
	}
	runtime.ReadMemStats(&after)
	return (after.TotalAlloc - before.TotalAlloc) / runs
}

// Normalizing an oversized password must cost about the same whatever its
// size: the result is cut at MaxPasswordLen+1 characters, so the full
// NFKC form, up to 18 times the input, is never built.
func TestNormalizePasswordCostIsBounded(t *testing.T) {
	// Not parallel: it measures the allocations of the whole process.
	const limit = 16 << 10 // bytes per call; the full form of 1 MiB costs megabytes
	for _, size := range []int{64 << 10, 1 << 20} {
		inputs := map[string]string{
			"widest expansion":    strings.Repeat("\ufdfa", size/3),
			"decomposed Cyrillic": strings.Repeat("\u0438\u0306", size/4),
			"ASCII":               strings.Repeat("a", size),
		}
		for name, in := range inputs {
			var out string
			got := allocatedPerCall(func() { out = NormalizePassword(in) })
			t.Logf("%s, %d bytes: %d bytes allocated per call", name, len(in), got)
			if passwordLen(out) != MaxPasswordLen+1 {
				t.Errorf("%s, %d bytes: the result has %d characters, want %d", name, len(in), passwordLen(out), MaxPasswordLen+1)
			}
			if got > limit {
				t.Errorf("%s, %d bytes: %d bytes allocated per call, want at most %d", name, len(in), got, limit)
			}
		}
	}
}

// The length rules count characters after normalization: a password may be
// typed as more code points than it has after NFKC, or far fewer.
func TestPasswordLengthCountsNormalizedCharacters(t *testing.T) {
	t.Parallel()
	const email = "anna@example.com"
	tests := []struct {
		name     string
		password string
		ok       bool
	}{
		{"128 characters typed as 256 decomposed code points", strings.Repeat("\u0438\u0306\u0435\u0308", 64), true},
		{"129 characters typed as 257 code points", strings.Repeat("\u0438\u0306\u0435\u0308", 64) + "a", false},
		{"128 characters from 44 code points with ligatures", strings.Repeat("\ufb03", 42) + "ab", true},
		{"129 characters from 43 ligatures", strings.Repeat("\ufb03", 43), false},
		{"128 characters from 9 code points with the widest expansion", strings.Repeat("\ufdfa", 7) + "ab", true},
		{"144 characters from 8 code points", strings.Repeat("\ufdfa", 8), false},
		{"128 ASCII characters", strings.Repeat("xy", 64), true},
		{"129 ASCII characters", strings.Repeat("xy", 64) + "z", false},
	}
	for _, tc := range tests {
		normalized := NormalizePassword(tc.password)
		problem := fieldProblem(t, ValidateNewAccount(email, normalized), "password")
		if tc.ok && problem != "" || !tc.ok && !strings.Contains(problem, "at most 128") {
			t.Errorf("%s: registration problem %q, want ok=%v", tc.name, problem, tc.ok)
		}
		// A login counts the same way, so every accepted password can log in.
		problem = fieldProblem(t, ValidateLogin(email, normalized), "password")
		if (problem == "") != tc.ok {
			t.Errorf("%s: login problem %q, want ok=%v", tc.name, problem, tc.ok)
		}
	}
}
