// Package search turns what someone types into a safe FTS5 query.
package search

import (
	"regexp"
	"strings"
)

var word = regexp.MustCompile(`[\p{L}\p{N}]+`)

// Query quotes every word (so FTS5 operators in user input are inert) and
// makes the last one a prefix match, for search-as-you-type. Returns "" when
// there is nothing searchable.
func Query(input string) string {
	words := word.FindAllString(input, 12)
	if len(words) == 0 {
		return ""
	}
	parts := make([]string, len(words))
	for i, w := range words {
		parts[i] = `"` + strings.ToLower(w) + `"`
	}
	parts[len(parts)-1] += "*"
	return strings.Join(parts, " ")
}

// Snippet highlight markers: control characters that never occur in notes,
// so the web layer can HTML-escape the snippet first and then turn these
// into <mark> tags without ever trusting note text as HTML.
const (
	MarkStart = "\x02"
	MarkEnd   = "\x03"
)
