package apiclient

import (
	"regexp"
	"strings"
	"unicode"
)

var apiTokenPattern = regexp.MustCompile(`rush_[0-9a-f]{8}\.[A-Za-z0-9_-]{43}`)

func scrubSecrets(value, token string) string {
	if token != "" {
		value = strings.ReplaceAll(value, token, "[redacted]")
	}
	return apiTokenPattern.ReplaceAllString(value, "[redacted]")
}

func cleanText(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
}
