package crypto

import (
	"fmt"
	"strings"
	"unicode"
)

// MinPassphraseLength is the minimum length accepted for a new master
// passphrase. Because the passphrase is the sole protection for all encrypted
// data and there is no recovery path, the bar is deliberately higher than a
// typical login password.
const MinPassphraseLength = 12

// commonWeakPassphrases is a small denylist of obviously-weak choices that meet
// the length bar but offer little protection. It is not exhaustive — it just
// catches the most common mistakes.
var commonWeakPassphrases = map[string]struct{}{
	"password1234":     {},
	"passwordpassword": {},
	"123456789012":     {},
	"qwertyuiopas":     {},
	"letmeinletmein":   {},
	"gcryptgcrypt":     {},
}

// CheckPassphraseStrength validates a candidate master passphrase and returns a
// user-facing error describing the problem, or nil if it is acceptable. The
// rules are intentionally simple and explain themselves: a minimum length, at
// least two character classes, not a single repeated character, and not on the
// small common-weak denylist.
func CheckPassphraseStrength(passphrase string) error {
	if len(passphrase) < MinPassphraseLength {
		return fmt.Errorf("passphrase must be at least %d characters (there is no recovery if it is lost or guessed)", MinPassphraseLength)
	}

	if _, weak := commonWeakPassphrases[strings.ToLower(passphrase)]; weak {
		return fmt.Errorf("that passphrase is too common — choose something unique")
	}

	var hasLower, hasUpper, hasDigit, hasOther bool
	first := []rune(passphrase)[0]
	allSame := true
	for _, r := range passphrase {
		if r != first {
			allSame = false
		}
		switch {
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsUpper(r):
			hasUpper = true
		case unicode.IsDigit(r):
			hasDigit = true
		default:
			hasOther = true
		}
	}
	if allSame {
		return fmt.Errorf("passphrase cannot be a single repeated character")
	}

	classes := 0
	for _, has := range []bool{hasLower, hasUpper, hasDigit, hasOther} {
		if has {
			classes++
		}
	}
	if classes < 2 {
		return fmt.Errorf("passphrase should mix at least two of: lowercase, uppercase, digits, symbols")
	}

	return nil
}
