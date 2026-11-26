package hub

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
	"uuid"

	"argus"
	"argus/internal/hub/config"
	"argus/internal/hub/utils"
	"ganigeorgiev.com/pocketbase/dbx"
	"ganigeorgiev.com/pocketbase/pocketbase/apis"
	"ganigeorgiev.com/pocketbase/pocketbase/core"
)

// UpdateInfo holds information about the latest update check

// Middleware to allow only admin role users
var requireAdminRole = customAuthMiddleware(func(e *core.RequestEvent) bool {
	return e.Auth.GetString("role") == "admin"
})

// Middleware to exclude readonly users
var excludeReadOnlyRole = customAuthMiddleware(func(e *core.RequestEvent) bool {
	return e.Auth.GetString("role") != "readonly"
})

// customAuthMiddleware handles boilerplate for custom authentication middlewares. fn should
// return true if the request is allowed, false otherwise. e.Auth is guaranteed to be non-nil.
func customAuthMiddleware(fn func(*core.RequestEvent) bool) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		if e.Auth == nil {
			return e.UnauthorizedError("The request requires valid record authorization token.", nil)
		}
		if !fn(e) {
			return e.ForbiddenError("The authorized record is not allowed to perform this action.", nil)
		}
		return e.Next()
	}
}

// registerMiddlewares registers custom middlewares
func (h *Hub) registerMiddlewares(se *core.ServeEvent) {
	// authorizes request with user matching the provided email
	authorizeRequestWithEmail := func(e *core.RequestEvent, email string) (err error) {
		if e.Auth != nil || email == "" {
			return e.Next()
		}
		isAuthRefresh := e.Request.URL.Path == "/api/collections/users/auth-refresh" && e.Request.Method == http.MethodPost
		e.Auth, err = e.App.FindAuthRecordByEmail("users", email)
		// log on the auth refresh only: the login page sends one per attempt,
		// while this middleware runs on every request
		if isAuthRefresh && errors.Is(err, sql.ErrNoRows) {
			slog.Warn("No user matches the automatic login email, showing the login form", "email", email)
		}
		if err != nil || !isAuthRefresh {
			return e.Next()
		}
		// auth refresh endpoint, make sure token is set in header
		token, _ := e.Auth.NewAuthToken()
		e.Request.Header.Set("Authorization", token)
		return e.Next()
	}
	// authenticate with trusted header
	if autoLogin, _ := utils.GetEnv("AUTO_LOGIN"); autoLogin != "" {
		se.Router.BindFunc(func(e *core.RequestEvent) error {
			return authorizeRequestWithEmail(e, autoLogin)
		})
	}
	// authenticate with trusted header
	if trustedHeader, _ := utils.GetEnv("TRUSTED_AUTH_HEADER"); trustedHeader != "" {
		// only honor the header from these peers, if set
		trustedProxies, restricted := parseTrustedProxies()
		if !restricted {
			slog.Warn("TRUSTED_AUTH_HEADER is set without TRUSTED_PROXY_IPS: the header is trusted from any client that can reach the hub", "header", trustedHeader)
		}
		se.Router.BindFunc(func(e *core.RequestEvent) error {
			if restricted && !isTrustedProxy(trustedProxies, e.Request.RemoteAddr) {
				return e.Next()
			}
			return authorizeRequestWithEmail(e, e.Request.Header.Get(trustedHeader))
		})
	}
}

// parseTrustedProxies reads TRUSTED_PROXY_IPS (comma-separated IPs or CIDRs).
// restricted is false when the variable is unset or empty, meaning the trusted
// header is accepted from any peer. Invalid entries are skipped with a warning,
// so a typo narrows the allowlist rather than widening it.
func parseTrustedProxies() (prefixes []netip.Prefix, restricted bool) {
	value, _ := utils.GetEnv("TRUSTED_PROXY_IPS")
	if value == "" {
		return nil, false
	}
	for entry := range strings.SplitSeq(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if prefix, err := parseProxyPrefix(entry); err == nil {
			prefixes = append(prefixes, prefix)
		} else {
			slog.Warn("Ignoring invalid TRUSTED_PROXY_IPS entry", "entry", entry)
		}
	}
	return prefixes, true
}

// parseProxyPrefix parses an IP or CIDR into a masked prefix. IPv4-mapped IPv6
// entries are converted to IPv4 so they match IPv4 peers.
func parseProxyPrefix(entry string) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(entry)
	if err != nil {
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			return netip.Prefix{}, err
		}
		addr = addr.Unmap()
		return netip.PrefixFrom(addr, addr.BitLen()), nil
	}
	if prefix.Addr().Is4In6() {
		if prefix.Bits() < 96 {
			return netip.Prefix{}, fmt.Errorf("%s covers more than the IPv4-mapped range", entry)
		}
		prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
	}
	return prefix.Masked(), nil
}

// isTrustedProxy reports whether the peer address of a request (host:port) is
// within one of the prefixes.
func isTrustedProxy(prefixes []netip.Prefix, remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.Unmap().WithZone("")
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// registerApiRoutes registers custom API routes
func (h *Hub) registerApiRoutes(se *core.ServeEvent) error {
	// auth protected routes
	apiAuth := se.Router.Group("/api/argus")
	apiAuth.Bind(apis.RequireAuth())
	// auth optional routes
	apiNoAuth := se.Router.Group("/api/argus")

	// create first user endpoint only needed if no users exist
	if totalUsers, _ := se.App.CountRecords("users"); totalUsers == 0 {
		apiNoAuth.POST("/create-user", h.um.CreateFirstUser)
	}
	// check if first time setup on login page
	apiNoAuth.GET("/first-run", func(e *core.RequestEvent) error {
		total, err := e.App.CountRecords("users")
		return e.JSON(http.StatusOK, map[string]bool{"firstRun": err == nil && total == 0})
	})
	// public key only (not secret): lets agents fetch the key to verify the hub
	apiNoAuth.GET("/public-key", h.getPublicKey)
	// cluster view (pods, services, nodes) and how fresh it is
	apiAuth.GET("/k8s", h.getKubernetes)
	// service and node states, open and recent incidents
	apiAuth.GET("/status", h.getStatus)
	apiAuth.GET("/history", h.getHistory)
	// get public key and version
	apiAuth.GET("/info", h.getInfo)
	apiAuth.GET("/getkey", h.getInfo) // deprecated - keep for compatibility w/ integrations
	// get config.yml content
	apiAuth.GET("/config-yaml", config.GetYamlConfig).BindFunc(requireAdminRole)
	// handle agent websocket connection
	apiNoAuth.GET("/agent-connect", h.handleAgentConnect)
	// get, create, or delete universal tokens (POST keeps tokens out of URLs; GET is kept for legacy clients)
	apiAuth.GET("/universal-token", h.handleUniversalToken).BindFunc(excludeReadOnlyRole)
	apiAuth.POST("/universal-token", h.handleUniversalToken).BindFunc(excludeReadOnlyRole)
	return nil
}

// getInfo returns data needed by authenticated users, such as the public key and current version
func (h *Hub) getInfo(e *core.RequestEvent) error {
	type infoResponse struct {
		Key     string `json:"key"`
		Version string `json:"v"`
	}
	h.sshKeyMu.Lock()
	pubKey := h.pubKey
	h.sshKeyMu.Unlock()
	info := infoResponse{
		Key:     pubKey,
		Version: argus.Version,
	}
	return e.JSON(http.StatusOK, info)
}

// universalTokenRequest holds the options for the universal token endpoint.
// Enable and Permanent use 0 for false, 1 for true, and -1 for unset.
type universalTokenRequest struct {
	Token     string `json:"token"`
	Enable    int    `json:"enable"`
	Permanent int    `json:"permanent"`
}

// handleUniversalToken handles the universal token API endpoint (create, read, delete)
func (h *Hub) handleUniversalToken(e *core.RequestEvent) error {
	e.Response.Header().Set("Cache-Control", "no-store")
	if e.Auth.IsSuperuser() {
		return e.ForbiddenError("Superusers cannot use universal tokens", nil)
	}

	req := universalTokenRequest{Enable: -1, Permanent: -1}
	if e.Request.Method == http.MethodPost {
		if err := e.BindBody(&req); err != nil {
			return e.BadRequestError("Invalid universal token request", err)
		}
	} else {
		// legacy GET requests pass options as query params
		query := e.Request.URL.Query()
		req.Token = query.Get("token")
		if v, err := strconv.Atoi(query.Get("enable")); err == nil {
			req.Enable = v
		}
		if v, err := strconv.Atoi(query.Get("permanent")); err == nil {
			req.Permanent = v
		}
	}

	tokenMap := universalTokenMap.GetMap()
	userID := e.Auth.Id
	token := req.Token

	// helper for deleting any existing permanent token record for this user
	deletePermanent := func() error {
		rec, err := h.FindFirstRecordByFilter("universal_tokens", "user = {:user}", dbx.Params{"user": userID})
		if err != nil {
			return nil // no record
		}
		return h.Delete(rec)
	}

	// helper for upserting a permanent token record for this user
	upsertPermanent := func(token string) error {
		rec, err := h.FindFirstRecordByFilter("universal_tokens", "user = {:user}", dbx.Params{"user": userID})
		if err == nil {
			rec.Set("token", token)
			return h.Save(rec)
		}

		col, err := h.FindCachedCollectionByNameOrId("universal_tokens")
		if err != nil {
			return err
		}
		newRec := core.NewRecord(col)
		newRec.Set("user", userID)
		newRec.Set("token", token)
		return h.Save(newRec)
	}

	// Disable universal tokens (both ephemeral and permanent)
	if req.Enable == 0 {
		tokenMap.RemovebyValue(userID)
		_ = deletePermanent()
		return e.JSON(http.StatusOK, map[string]any{"token": token, "active": false, "permanent": false})
	}

	// Enable universal token (ephemeral or permanent)
	if req.Enable == 1 {
		if token == "" {
			token = uuid.New().String()
		}

		if req.Permanent == 1 {
			// make token permanent (persist across restarts)
			tokenMap.RemovebyValue(userID)
			if err := upsertPermanent(token); err != nil {
				return err
			}
			return e.JSON(http.StatusOK, map[string]any{"token": token, "active": true, "permanent": true})
		}

		// default: ephemeral mode (1 hour)
		_ = deletePermanent()
		tokenMap.Set(token, userID, time.Hour)
		return e.JSON(http.StatusOK, map[string]any{"token": token, "active": true, "permanent": false})
	}

	// Read current state
	// Prefer permanent token if it exists.
	if rec, err := h.FindFirstRecordByFilter("universal_tokens", "user = {:user}", dbx.Params{"user": userID}); err == nil {
		dbToken := rec.GetString("token")
		// If no token was provided, or the caller is asking about their permanent token, return it.
		if token == "" || token == dbToken {
			return e.JSON(http.StatusOK, map[string]any{"token": dbToken, "active": true, "permanent": true})
		}
		// Token doesn't match their permanent token (avoid leaking other info)
		return e.JSON(http.StatusOK, map[string]any{"token": token, "active": false, "permanent": false})
	}

	// No permanent token; fall back to ephemeral token map.
	if token == "" {
		// return existing token if it exists
		if token, _, ok := tokenMap.GetByValue(userID); ok {
			return e.JSON(http.StatusOK, map[string]any{"token": token, "active": true, "permanent": false})
		}
		// if no token is provided, generate a new one
		token = uuid.New().String()
	}

	// Token is considered active only if it belongs to the current user.
	activeUser, ok := tokenMap.GetOk(token)
	active := ok && activeUser == userID
	response := map[string]any{"token": token, "active": active, "permanent": false}
	return e.JSON(http.StatusOK, response)
}
