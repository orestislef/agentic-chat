package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/orestislef/agentic-chat/internal/auth"
	"github.com/orestislef/agentic-chat/internal/mcpserver"
	"github.com/orestislef/agentic-chat/internal/store"
)

// Browser sign in for MCP clients, following the MCP authorization spec: OAuth 2.1 with
// PKCE and dynamic client registration. The result is a connection token, and every chat
// behind it gets its own agent.
//
// Redirects only go to the user's own machine. These clients run locally, and refusing
// everything else means a website cannot trick a logged in person into handing it access.

const oauthCodeLifetime = 5 * time.Minute

func (s *Server) hubURL(r *http.Request) string {
	base, ok := s.baseURL(r)
	if !ok {
		return ""
	}
	return base
}

func (s *Server) handleResourceMetadata(w http.ResponseWriter, r *http.Request) {
	base := s.hubURL(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 base + "/mcp",
		"authorization_servers":    []string{base},
		"bearer_methods_supported": []string{"header"},
	})
}

func (s *Server) handleAuthServerMetadata(w http.ResponseWriter, r *http.Request) {
	base := s.hubURL(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         base,
		"authorization_endpoint":                         base + "/oauth/authorize",
		"token_endpoint":                                 base + "/oauth/token",
		"registration_endpoint":                          base + "/oauth/register",
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"authorization_response_iss_parameter_supported": true,
	})
}

// isLoopback accepts the only redirect targets this hub allows.
func isLoopback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Fragment != "" {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// redirectAllowed compares without the port, because local clients pick a free one each time.
func redirectAllowed(client *store.OAuthClient, raw string) bool {
	if !isLoopback(raw) {
		return false
	}
	want, err := url.Parse(raw)
	if err != nil {
		return false
	}
	for _, registered := range client.RedirectURIs {
		u, err := url.Parse(registered)
		if err == nil && u.Hostname() == want.Hostname() && u.Path == want.Path {
			return true
		}
	}
	return false
}

func oauthError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}

func (s *Server) handleOAuthRegister(w http.ResponseWriter, r *http.Request) {
	if !s.oauthClients.Allow(s.clientIP(r)) {
		oauthError(w, http.StatusTooManyRequests, "invalid_client_metadata", "too many registrations, try again later")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBody))
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "request too large")
		return
	}
	var in struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "not valid JSON")
		return
	}
	if len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > 10 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "send between 1 and 10 redirect uris")
		return
	}
	for _, uri := range in.RedirectURIs {
		if !isLoopback(uri) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "this hub only redirects to http://localhost or http://127.0.0.1")
			return
		}
	}
	name := strings.TrimSpace(in.ClientName)
	if name == "" {
		name = "MCP client"
	}
	if len(name) > 100 {
		name = strings.ToValidUTF8(name[:100], "")
	}
	id, err := auth.NewToken("acl")
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if err := s.store.CreateOAuthClient(r.Context(), store.OAuthClient{ID: id, Name: name, RedirectURIs: in.RedirectURIs}); err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  id,
		"client_name":                name,
		"redirect_uris":              in.RedirectURIs,
		"grant_types":                []string{"authorization_code"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

// handleAuthorize shows the web UI, which logs the person in and asks for approval. A bad
// client or redirect stops here with an error page, it must never be redirected to.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	client, err := s.store.OAuthClientByID(r.Context(), q.Get("client_id"))
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "unknown client, try connecting again from your MCP client", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !redirectAllowed(client, q.Get("redirect_uri")) {
		http.Error(w, "that redirect address is not allowed for this client", http.StatusBadRequest)
		return
	}
	s.static.serveFile(w, r, "index.html")
}

// handleOAuthClientInfo tells the approval screen what it is approving.
func (s *Server) handleOAuthClientInfo(w http.ResponseWriter, r *http.Request, u *store.User) {
	q := r.URL.Query()
	client, err := s.store.OAuthClientByID(r.Context(), q.Get("client_id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "unknown client, try connecting again from your MCP client")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !redirectAllowed(client, q.Get("redirect_uri")) {
		writeError(w, http.StatusBadRequest, "that redirect address is not allowed for this client")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"name": client.Name})
}

func (s *Server) handleOAuthApprove(w http.ResponseWriter, r *http.Request, u *store.User) {
	var in struct {
		ClientID      string `json:"client_id"`
		RedirectURI   string `json:"redirect_uri"`
		State         string `json:"state"`
		Challenge     string `json:"code_challenge"`
		ChallengeType string `json:"code_challenge_method"`
		WorkspaceID   int64  `json:"workspace_id"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	client, err := s.store.OAuthClientByID(r.Context(), in.ClientID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "unknown client, try connecting again from your MCP client")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !redirectAllowed(client, in.RedirectURI) {
		writeError(w, http.StatusBadRequest, "that redirect address is not allowed for this client")
		return
	}
	if in.Challenge == "" || in.ChallengeType != "S256" {
		writeError(w, http.StatusBadRequest, "this client did not send a valid PKCE challenge")
		return
	}
	if _, err := s.store.MemberRole(r.Context(), in.WorkspaceID, u.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "workspace not found")
			return
		}
		s.internalError(w, r, err)
		return
	}
	code, err := auth.NewToken("acd")
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	err = s.store.CreateOAuthCode(r.Context(), auth.HashToken(code), store.OAuthCode{
		ClientID: client.ID, UserID: u.ID, WorkspaceID: in.WorkspaceID, RedirectURI: in.RedirectURI, Challenge: in.Challenge,
	}, time.Now().Add(oauthCodeLifetime).UnixMilli())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	target, err := url.Parse(in.RedirectURI)
	if err != nil {
		writeError(w, http.StatusBadRequest, "that redirect address is not valid")
		return
	}
	query := target.Query()
	query.Set("code", code)
	query.Set("iss", s.hubURL(r))
	if in.State != "" {
		query.Set("state", in.State)
	}
	target.RawQuery = query.Encode()
	writeJSON(w, http.StatusOK, map[string]string{"redirect": target.String()})
}

func (s *Server) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "could not read the form")
		return
	}
	if !s.limits.Allow("token:" + s.clientIP(r)) {
		oauthError(w, http.StatusTooManyRequests, "invalid_request", "too many attempts, try again in a few minutes")
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code is supported")
		return
	}
	code, err := s.store.ConsumeOAuthCode(r.Context(), auth.HashToken(r.PostForm.Get("code")))
	if errors.Is(err, store.ErrNotFound) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code is wrong, expired or already used")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if code.ClientID != r.PostForm.Get("client_id") || code.RedirectURI != r.PostForm.Get("redirect_uri") {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code was issued for something else")
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(computed), []byte(code.Challenge)) != 1 {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code verifier does not match")
		return
	}
	client, err := s.store.OAuthClientByID(r.Context(), code.ClientID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	token, err := auth.NewToken(strings.TrimSuffix(mcpserver.ConnectionTokenPrefix, "_"))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if err := s.store.CreateConnection(r.Context(), auth.HashToken(token), client.Name, code.UserID, code.WorkspaceID); err != nil {
		s.internalError(w, r, err)
		return
	}
	// No expiry and no refresh token: the connection stays valid while it is used, and is
	// signed out in the web UI. Rotating a token that many chats share would log them out.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"access_token": token, "token_type": "Bearer"})
}

func (s *Server) handleListConnections(w http.ResponseWriter, r *http.Request, u *store.User) {
	list, err := s.store.ConnectionsForUser(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleDeleteConnection(w http.ResponseWriter, r *http.Request, u *store.User) {
	id, ok := pathID(r, "connectionID")
	if !ok {
		writeError(w, http.StatusNotFound, "connection not found")
		return
	}
	err := s.store.DeleteConnection(r.Context(), u.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "connection not found")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
