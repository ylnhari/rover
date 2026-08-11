// Package childenv builds the environment inherited by user-controlled child
// processes without exposing rover or other parent-process credentials.
package childenv

import "strings"

var sensitiveSuffixes = [...]string{"_SECRET", "_TOKEN", "_KEY"}

// Filter returns a copy of env with credential-shaped variables removed.
// Variable-name matching is case-insensitive so the boundary is identical on
// Windows and Unix. Values are never inspected or logged.
func Filter(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if ok && sensitiveName(name) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func sensitiveName(name string) bool {
	upper := strings.ToUpper(name)
	if upper == "ROVER_SECRET" || upper == "ROVER_PROXY_VERIFY" {
		return true
	}
	for _, suffix := range sensitiveSuffixes {
		if strings.HasSuffix(upper, suffix) {
			return true
		}
	}
	return false
}
