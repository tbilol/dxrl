package service

import (
	"strings"
	"unicode"

	"github.com/tbilol/dxrl/services/catalog/internal/domain"
)

// maxQueryTerms bounds the words of a search query; extra words are ignored.
const maxQueryTerms = 10

// Terms normalizes free text into search words, the same way for listings
// and queries so both always agree (independently of the database locale):
//
//   - lower case (Unicode), and "ё" as "е";
//   - apostrophes (' ` ´ ʻ ʼ ‘ ’) removed, so "Bug'doy", "Bugʻdoy" and
//     "bugdoy" are the same word, which is also the product key;
//   - every other rune that is not a letter or a digit separates words.
func Terms(s string) []string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		switch {
		case isApostrophe(r):
		case r == 'ё':
			b.WriteRune('е')
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	return strings.Fields(b.String())
}

// isApostrophe reports the apostrophe-like runes Uzbek Latin is written with
// (oʻ, gʻ and the tutuq belgisi), in all the forms keyboards produce. U+02BB
// and U+02BC are letters to Unicode, so they are checked first.
func isApostrophe(r rune) bool {
	switch r {
	case '\'', '`', '´', 'ʻ', 'ʼ', '‘', '’':
		return true
	}
	return false
}

// queryTerms are the distinct words of a search query, at most
// maxQueryTerms of them. A listing matches when its document has a word
// starting with each term.
func queryTerms(query string) []string {
	var out []string
	for _, t := range Terms(query) {
		if len(out) == maxQueryTerms {
			break
		}
		if !contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// searchDocument is the text SearchListings matches a listing against: the
// product's uz and ru names and its key (as written and without hyphens), the
// district, and the notes. catalog.proto also names the owner's name, which
// catalog does not have. TODO(edge): owner-name search needs profile data
// (a projection of profile events or a search service), out of catalog's
// scope by decision.
func searchDocument(pt domain.ProductType, f domain.ListingFields) domain.SearchDocument {
	product := pt.NameUZ + " " + pt.NameRU + " " + pt.Key + " " + strings.ReplaceAll(pt.Key, "-", "")
	return domain.SearchDocument{
		Product: strings.Join(Terms(product), " "),
		Place:   strings.Join(Terms(f.Address.District), " "),
		Notes:   strings.Join(Terms(f.Notes), " "),
	}
}
