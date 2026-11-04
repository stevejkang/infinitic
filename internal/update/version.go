package update

import (
	"cmp"
	"strconv"
	"strings"
)

func Trim(v string) string { return strings.TrimPrefix(v, "v") }

// Valid reports whether v is MAJOR.MINOR.PATCH with an optional -prerelease, "v" allowed.
func Valid(v string) bool {
	_, _, ok := parse(v)
	return ok
}

// Newer reports whether a is a later release than b. A prerelease comes before its release, and
// rc2 before rc10. Invalid versions are never newer.
func Newer(a, b string) bool {
	ca, pa, okA := parse(a)
	cb, pb, okB := parse(b)
	if !okA || !okB {
		return false
	}
	for i := range ca {
		if ca[i] != cb[i] {
			return ca[i] > cb[i]
		}
	}
	switch {
	case pa == pb:
		return false
	case pa == "":
		return true
	case pb == "":
		return false
	}
	return comparePre(pa, pb) > 0
}

func parse(v string) (core [3]int, pre string, ok bool) {
	main, pre, _ := strings.Cut(Trim(v), "-")
	parts := strings.Split(main, ".")
	if len(parts) != 3 {
		return core, "", false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" {
			return core, "", false
		}
		core[i] = n
	}
	return core, pre, true
}

func comparePre(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < min(len(as), len(bs)); i++ {
		if c := compareIdent(as[i], bs[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(as), len(bs))
}

// compareIdent compares "rc2" and "rc10" by their number when the text before it is the same.
func compareIdent(a, b string) int {
	ta, na := splitNumber(a)
	tb, nb := splitNumber(b)
	if ta == tb && na >= 0 && nb >= 0 {
		return cmp.Compare(na, nb)
	}
	return cmp.Compare(a, b)
}

func splitNumber(s string) (text string, n int) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	n, err := strconv.Atoi(s[i:])
	if err != nil {
		return s, -1
	}
	return s[:i], n
}
