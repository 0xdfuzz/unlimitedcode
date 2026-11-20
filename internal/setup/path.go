package setup

import (
	"os"
	"path/filepath"
	"strings"
)

// StripOpenhop removes PATH entries that point inside an openhop tree and
// returns the removed entries. Everything else keeps its order.
func StripOpenhop(value string) (string, []string) {
	entries := filepath.SplitList(value)
	kept := make([]string, 0, len(entries))
	var removed []string
	for _, entry := range entries {
		if IsOpenhopEntry(entry) {
			removed = append(removed, entry)
			continue
		}
		kept = append(kept, entry)
	}
	return strings.Join(kept, string(os.PathListSeparator)), removed
}

// IsOpenhopEntry reports whether any path segment is exactly "openhop".
// A sibling such as "openhopkit" is left alone.
func IsOpenhopEntry(entry string) bool {
	for _, segment := range strings.FieldsFunc(entry, func(r rune) bool {
		return r == '\\' || r == '/'
	}) {
		if strings.EqualFold(segment, "openhop") {
			return true
		}
	}
	return false
}
