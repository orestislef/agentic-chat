package mcp

import (
	"net/http"
	"strings"
)

// IdempotencyHeader carries a key that stays the same across retries of one tool call.
const IdempotencyHeader = "Idempotency-Key"

// BearerToken pulls the token out of an Authorization header.
func BearerToken(r *http.Request) string {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if len(header) > len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return strings.TrimSpace(header[len(prefix):])
	}
	return ""
}
