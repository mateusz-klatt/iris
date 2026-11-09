package hub

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mateusz-klatt/iris/internal/consoleauth"
	"github.com/mateusz-klatt/iris/internal/store"
)

const (
	consoleCookieName      = "iris_console_session"
	consoleLoginCookieName = "iris_console_login"
	consoleChallengeLimit  = 128
	consoleAttemptLimit    = 256
	dummyConsoleHash       = "$argon2id$v=19$m=65536,t=3,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

type passwordAttempt struct {
	count   int
	expires time.Time
}

// consoleAuth is configured before serving. A missing key closes browser
// authentication while owner recovery and agent coordination remain available.
type consoleAuth struct {
	key        []byte
	mu         sync.Mutex
	challenges map[string]time.Time
	attempts   map[string]passwordAttempt
	aggregate  passwordAttempt
	verify     func(string, string) (bool, error)
	create     func(string) (string, error)
	upgrade    func(string) (string, error)
}

func newConsoleAuth(key []byte) *consoleAuth {
	return &consoleAuth{key: append([]byte(nil), key...), challenges: map[string]time.Time{}, attempts: map[string]passwordAttempt{}, verify: consoleauth.VerifyPassword, create: consoleauth.CreatePassword, upgrade: consoleauth.UpgradePassword}
}

func (c *consoleAuth) mac(domain, value string) []byte {
	mac := hmac.New(sha256.New, c.key)
	_, _ = mac.Write([]byte("iris.console." + domain + "\x00"))
	_, _ = mac.Write([]byte(value))
	return mac.Sum(nil)
}
func (c *consoleAuth) csrf(domain, value string) string {
	return base64.RawURLEncoding.EncodeToString(c.mac(domain, value))
}
func validConsoleToken(value string) bool {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(raw) == 32 && base64.RawURLEncoding.EncodeToString(raw) == value
}
func consoleCookie(r *http.Request, name string) string {
	// Duplicate cookie names are ambiguous and must not select an authority.
	cookies := r.CookiesNamed(name)
	if len(cookies) != 1 || !validConsoleToken(cookies[0].Value) {
		return ""
	}
	return cookies[0].Value
}
func (h *Hub) consoleTransport(r *http.Request) bool {
	if !h.loginAllowed(r) {
		return false
	}
	if r.TLS == nil && h.publicURL == nil {
		u := url.URL{Host: r.Host}
		host := u.Hostname()
		return host == "127.0.0.1" || host == "::1"
	}
	return true
}
func (h *Hub) consoleOrigin(r *http.Request) bool {
	scheme := "http"
	if r.TLS != nil || h.publicURL != nil {
		scheme = "https"
	}
	values := r.Header.Values("Origin")
	return len(values) == 1 && values[0] == scheme+"://"+r.Host
}
func consoleFailure(w http.ResponseWriter) {
	writeError(w, &store.Error{Code: "unauthorized", Message: "Invalid username or password"})
}
func consoleCSRFMatches(r *http.Request, want string) bool {
	headers := r.Header.Values("X-Iris-CSRF")
	return len(headers) == 1 && subtle.ConstantTimeCompare([]byte(headers[0]), []byte(want)) == 1
}
func consoleCSRF(w http.ResponseWriter, r *http.Request, want string) bool {
	if !consoleCSRFMatches(r, want) {
		writeError(w, &store.Error{Code: "forbidden", Message: "valid console CSRF token required"})
		return false
	}
	return true
}
func (h *Hub) consoleSetCookie(w http.ResponseWriter, r *http.Request, name, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil || h.publicURL != nil, MaxAge: age})
}
func (c *consoleAuth) passwordRate(w http.ResponseWriter, now time.Time, username string) bool {
	key, err := consoleauth.LoginKey(username)
	if err != nil {
		key = username
	}
	digest := string(c.mac("attempt", key))
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, bucket := range c.attempts {
		if !bucket.expires.After(now) {
			delete(c.attempts, k)
		}
	}
	if !c.aggregate.expires.After(now) {
		c.aggregate = passwordAttempt{expires: now.Add(time.Minute)}
	}
	bucket := c.attempts[digest]
	if !bucket.expires.After(now) {
		bucket = passwordAttempt{expires: now.Add(15 * time.Minute)}
	}
	retry := time.Duration(0)
	if c.aggregate.count >= 30 {
		retry = c.aggregate.expires.Sub(now)
	}
	if bucket.count >= 5 && bucket.expires.Sub(now) > retry {
		retry = bucket.expires.Sub(now)
	}
	if len(c.attempts) >= consoleAttemptLimit && bucket.count == 0 {
		retry = 15 * time.Minute
	}
	if retry > 0 {
		rateLimited(w, int(retry.Seconds())+1)
		return false
	}
	bucket.count++
	c.attempts[digest] = bucket
	c.aggregate.count++
	return true
}

// serveConsole owns the browser namespace in full. It also rejects mixed credentials
// on machine routes, instead of silently selecting the bearer authority.
func (h *Hub) serveConsole(w http.ResponseWriter, r *http.Request) bool {
	if len(r.Header.Values("Authorization")) != 0 && len(r.CookiesNamed(consoleCookieName)) != 0 {
		writeError(w, &store.Error{Code: "forbidden", Message: "mixed machine and console credentials"})
		return true
	}
	if r.URL.Path == "/ui/login" {
		writeError(w, &store.Error{Code: "not_found", Message: "unknown API route"})
		return true
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/ui/") {
		return false
	}
	if r.URL.Path == "/v1/ui/links" {
		writeError(w, &store.Error{Code: "not_found", Message: "unknown API route"})
		return true
	}
	if r.Method == http.MethodPut && r.URL.Path == "/v1/ui/doctor" {
		if !h.authorized(r) {
			writeError(w, &store.Error{Code: "unauthorized", Message: "machine bearer required for doctor upload"})
			return true
		}
		return false
	}
	if len(h.console.key) != 32 || !h.consoleTransport(r) {
		w.Header().Set("X-Iris-Console-Mode", "closed")
		writeError(w, &store.Error{Code: "unauthorized", Message: "console authentication unavailable"})
		return true
	}
	if len(r.Header.Values("Authorization")) != 0 {
		writeError(w, &store.Error{Code: "forbidden", Message: "a console account session is required"})
		return true
	}
	if (r.Method != http.MethodGet || r.URL.Path == "/v1/ui/watch") && !h.consoleOrigin(r) {
		writeError(w, &store.Error{Code: "forbidden", Message: "exact console Origin required"})
		return true
	}
	if r.URL.Path == "/v1/ui/login" && r.Method == http.MethodGet {
		h.consoleChallenge(w, r)
		return true
	}
	if r.URL.Path == "/v1/ui/login" && r.Method == http.MethodPost {
		h.consoleLogin(w, r)
		return true
	}
	token := consoleCookie(r, consoleCookieName)
	digest := h.console.mac("session", token)
	if token == "" {
		digest = nil
	}
	if r.URL.Path == "/v1/ui/logout" && r.Method == http.MethodPost {
		h.consoleLogout(w, r, token, digest)
		return true
	}
	p, err := consoleRequestPrincipal(r, digest)
	if err != nil {
		writeError(w, err)
		return true
	}
	// A mounted console binds reads to its session even if another tab replaces
	// the shared cookie. Bootstrap reads may omit the synchronizer token.
	if r.Method == http.MethodGet && len(r.Header.Values("X-Iris-CSRF")) != 0 && !consoleCSRFMatches(r, h.console.csrf("session-csrf", token)) {
		writeError(w, &store.Error{Code: "unauthorized", Message: "console session changed"})
		return true
	}
	if r.Method != http.MethodGet && !consoleCSRF(w, r, h.console.csrf("session-csrf", token)) {
		return true
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /v1/ui/session":
		info, err := h.store.ConsoleSessionInfo(r.Context(), digest)
		if err != nil {
			writeError(w, err)
		} else {
			writeJSON(w, map[string]any{"account": info.Account, "created_ms": info.CreatedMS, "expires_ms": info.ExpiresMS, "csrf": h.console.csrf("session-csrf", token)})
		}
	case "POST /v1/ui/password":
		account, err := h.store.ConsoleSessionAccount(r.Context(), digest)
		if err != nil {
			writeError(w, err)
		} else {
			h.consolePassword(w, r, p, account)
		}
	case "PATCH /v1/ui/profile":
		h.consoleProfile(w, r, p)
	case "GET /v1/ui/accounts":
		accounts, err := h.store.ListConsoleAccounts(r.Context(), p)
		if err != nil {
			writeError(w, err)
		} else {
			writeJSON(w, map[string]any{"accounts": accounts})
		}
	case "GET /v1/ui/watch":
		account, err := h.store.ConsoleSessionAccount(r.Context(), digest)
		if err != nil {
			writeError(w, err)
		} else {
			h.consoleWatch(w, r, p, account.ID)
		}
	default:
		h.consoleOperation(w, r, p)
	}
	return true
}
func (h *Hub) consoleChallenge(w http.ResponseWriter, r *http.Request) {
	c := h.console
	now := h.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, expires := range c.challenges {
		if !expires.After(now) {
			delete(c.challenges, key)
		}
	}
	if len(c.challenges) >= consoleChallengeLimit {
		rateLimited(w, 120)
		return
	}
	raw := secret()
	c.challenges[string(c.mac("challenge", raw))] = now.Add(2 * time.Minute)
	h.consoleSetCookie(w, r, consoleLoginCookieName, raw, 120)
	writeJSON(w, map[string]any{"csrf": c.csrf("login-csrf", raw), "expires_ms": now.Add(2 * time.Minute).UnixMilli()})
}
func (h *Hub) consoleLogin(w http.ResponseWriter, r *http.Request) {
	c := h.console
	raw := consoleCookie(r, consoleLoginCookieName)
	if raw == "" || !consoleCSRF(w, r, c.csrf("login-csrf", raw)) {
		if raw == "" {
			writeError(w, &store.Error{Code: "forbidden", Message: "valid console login challenge required"})
		}
		return
	}
	c.mu.Lock()
	expires, ok := c.challenges[string(c.mac("challenge", raw))]
	delete(c.challenges, string(c.mac("challenge", raw)))
	c.mu.Unlock()
	h.consoleSetCookie(w, r, consoleLoginCookieName, "", -1)
	if !ok || !expires.After(h.now()) {
		writeError(w, &store.Error{Code: "forbidden", Message: "console login challenge expired or used"})
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !consoleDecode(w, r, &input) {
		return
	}
	if !c.passwordRate(w, h.now(), input.Username) {
		return
	}
	if len(input.Password) > consoleauth.MaxPasswordBytes || !utf8.ValidString(input.Password) {
		consoleFailure(w)
		return
	}
	account, lookupErr := h.store.LookupConsoleAccount(r.Context(), input.Username)
	record := account.PasswordHash
	rejected := lookupErr != nil || !account.Enabled() || consoleauth.ValidateHash(record) != nil
	if rejected {
		record = dummyConsoleHash
	}
	good, err := c.verify(input.Password, record)
	if errors.Is(err, consoleauth.ErrBusy) {
		rateLimited(w, 1)
		return
	}
	if err != nil || !good || rejected {
		consoleFailure(w)
		return
	}
	upgrade := ""
	if strings.HasPrefix(account.PasswordHash, "scrypt$") {
		upgrade, err = c.upgrade(input.Password)
		if errors.Is(err, consoleauth.ErrBusy) {
			rateLimited(w, 1)
			return
		}
		if err != nil {
			consoleFailure(w)
			return
		}
	}
	token := secret()
	if _, err = h.store.IssueConsoleSession(r.Context(), account, c.mac("session", token), upgrade); err != nil {
		consoleFailure(w)
		return
	}
	h.consoleSetCookie(w, r, consoleCookieName, token, 43200)
	writeJSON(w, map[string]any{"authenticated": true})
}
func (h *Hub) consoleLogout(w http.ResponseWriter, r *http.Request, token string, digest []byte) {
	_, err := h.store.ConsoleSessionAccount(r.Context(), digest)
	if err == nil {
		if !consoleCSRF(w, r, h.console.csrf("session-csrf", token)) {
			return
		}
		if err = h.store.DeleteConsoleSession(r.Context(), digest); err != nil {
			writeError(w, err)
			return
		}
	} else {
		var domain *store.Error
		if !errors.As(err, &domain) || domain.Code != "unauthorized" {
			writeError(w, err)
			return
		}
	}
	h.consoleSetCookie(w, r, consoleCookieName, "", -1)
	writeJSON(w, map[string]any{"logged_out": true})
}
func (h *Hub) consolePassword(w http.ResponseWriter, r *http.Request, p store.Principal, account store.ConsoleAccount) {
	var input struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if !consoleDecode(w, r, &input) {
		return
	}
	if err := consoleauth.ValidateNewPassword(input.New); err != nil {
		writeError(w, &store.Error{Code: "bad_request", Message: "new password must contain at least 15 code points and at most 1024 UTF-8 bytes"})
		return
	}
	if !h.console.passwordRate(w, h.now(), account.Username) {
		return
	}
	good, err := h.console.verify(input.Current, account.PasswordHash)
	if errors.Is(err, consoleauth.ErrBusy) {
		rateLimited(w, 1)
		return
	}
	if err != nil || !good {
		consoleFailure(w)
		return
	}
	hash, err := h.console.create(input.New)
	if errors.Is(err, consoleauth.ErrBusy) {
		rateLimited(w, 1)
		return
	}
	if err != nil {
		writeError(w, &store.Error{Code: "bad_request", Message: "password could not be changed"})
		return
	}
	if err = h.store.ChangeConsolePassword(r.Context(), p, account, hash); err != nil {
		writeError(w, err)
		return
	}
	h.consoleSetCookie(w, r, consoleCookieName, "", -1)
	writeJSON(w, map[string]any{"login_required": true})
}
func (h *Hub) consoleProfile(w http.ResponseWriter, r *http.Request, p store.Principal) {
	var input struct {
		Revision       int64  `json:"edit_revision"`
		Display        string `json:"display_name"`
		UI             string `json:"ui_locale"`
		Correspondence string `json:"correspondence_locale"`
	}
	if !consoleDecode(w, r, &input) {
		return
	}
	account, err := h.store.EditConsoleProfile(r.Context(), p, input.Revision, input.Display, input.UI, input.Correspondence)
	if err != nil {
		writeError(w, err)
	} else {
		writeJSON(w, map[string]any{"account": account})
	}
}
func (h *Hub) consoleOperation(w http.ResponseWriter, r *http.Request, p store.Principal) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/ui/"), "/")
	op := map[string]string{"GET overview": "ui_overview", "GET deliveries": "ui_deliveries", "GET threads": "ui_threads", "POST messages": "ui_messages", "GET doctor": "ui_doctor_get"}[r.Method+" "+strings.Join(parts, "/")]
	key := ""
	fence := int64(0)
	if len(parts) == 2 && parts[0] == "threads" && r.Method == http.MethodGet {
		op, key = "ui_thread", parts[1]
	}
	if len(parts) == 3 {
		switch {
		case parts[0] == "threads" && parts[2] == "read" && r.Method == http.MethodPost:
			op, key = "ui_read", parts[1]
		case parts[0] == "messages" && parts[2] == "status" && r.Method == http.MethodGet:
			op, key = "ui_receipts", parts[1]
		case parts[0] == "claims" && parts[2] == "revoke" && r.Method == http.MethodPost:
			var err error
			fence, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				writeError(w, &store.Error{Code: "bad_request", Message: "invalid fence"})
				return
			}
			op = "ui_revoke"
		case parts[0] == "accounts" && parts[2] == "access" && r.Method == http.MethodPut:
			var input struct {
				Revision int64  `json:"edit_revision"`
				Role     string `json:"role"`
				Disabled bool   `json:"disabled"`
				Grants   []struct {
					Repository string `json:"repository_id"`
					Mode       string `json:"mode"`
				} `json:"grants"`
			}
			if !consoleDecode(w, r, &input) {
				return
			}
			// A missing key is not []: only an explicit empty list removes every grant.
			if input.Grants == nil {
				writeError(w, &store.Error{Code: "bad_request", Message: "grants are required"})
				return
			}
			grants := make([]store.ConsoleGrant, len(input.Grants))
			for i, g := range input.Grants {
				grants[i] = store.ConsoleGrant{RepositoryID: g.Repository, Mode: g.Mode}
			}
			account, err := h.store.ChangeConsoleAccess(r.Context(), p, parts[1], input.Revision, input.Role, input.Disabled, grants)
			if err != nil {
				writeError(w, err)
			} else {
				writeJSON(w, map[string]any{"account": account})
			}
			return
		}
	}
	if op == "" {
		writeError(w, &store.Error{Code: "not_found", Message: "unknown console route"})
		return
	}
	req, err := decode(w, r)
	if err != nil {
		writeError(w, err)
		return
	}
	req.Q, req.Agent, req.Before = r.URL.Query().Get("q"), r.URL.Query().Get("agent"), r.URL.Query().Get("before")
	if key != "" {
		req.Key = key
	}
	req.Fence = fence
	out, err := h.store.CallConsole(r.Context(), p, op, req)
	if err != nil {
		writeError(w, err)
	} else {
		writeJSON(w, out)
	}
}
