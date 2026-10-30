// Package mcp implements the small part of the Model Context Protocol the hub
// needs: initialize, ping, tools/list and tools/call. It is written by hand
// rather than on an SDK for one reason: the bridge has to stay on a protocol
// revision that still allows server initiated notifications, and has to send a
// custom notification method. Both are a few lines here.
package mcp

import (
	"encoding/json"
	"slices"

	"github.com/orestislef/agentic-chat/internal/version"
)

// Revisions this server speaks, newest first. Revision 2026-07-28 is left out on
// purpose: it has no way to deliver the push notifications the bridge relies on,
// and clients fall back to an earlier handshake when a server does not offer it.
var supportedVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// JSON-RPC error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Request is a JSON-RPC request or notification. Notifications have no ID.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification reports whether the sender expects no response.
func (r *Request) IsNotification() bool {
	return len(r.ID) == 0
}

// Response is a JSON-RPC response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is the error member of a response.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Result builds a success response.
func Result(id json.RawMessage, result any) *Response {
	return &Response{JSONRPC: "2.0", ID: id, Result: result}
}

// Error builds an error response.
func Error(id json.RawMessage, code int, message string) *Response {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &Response{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: message}}
}

// NegotiateVersion picks the revision to answer initialize with: the client's
// own if we know it, otherwise our newest.
func NegotiateVersion(params json.RawMessage) string {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	json.Unmarshal(params, &p)
	if slices.Contains(supportedVersions, p.ProtocolVersion) {
		return p.ProtocolVersion
	}
	return supportedVersions[0]
}

// InitializeResult builds the answer to initialize.
func InitializeResult(params json.RawMessage, capabilities map[string]any, instructions string) map[string]any {
	return map[string]any{
		"protocolVersion": NegotiateVersion(params),
		"capabilities":    capabilities,
		"serverInfo":      map[string]any{"name": "agentic-chat", "version": version.Version},
		"instructions":    instructions,
	}
}

// TextResult wraps tool output.
func TextResult(text string, isError bool) map[string]any {
	result := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
	if isError {
		result["isError"] = true
	}
	return result
}
