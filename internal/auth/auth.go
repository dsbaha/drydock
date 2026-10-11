// Package auth handles user registration, login, session cookies, and the
// authentication middleware. It also implements the trusted-header mode:
// when DRYDOCK_TRUSTED_USER_HEADER is set (e.g. "X-Remote-User"), nginx in
// front does OAuth and passes the authenticated username in that header;
// Drydock auto-provisions the account on first sight. Cookie auth is used
// otherwise. Auth stays pluggable middleware — handlers only see the user.
package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/dsbaha/drydock/internal/config"
	"github.com/dsbaha/drydock/internal/db"
	"github.com/dsbaha/drydock/internal/model"
)

const cookieName = "drydock_session"

const sessionTTL = 30 * 24 * time.Hour // 30 days

var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]{3,32}$`)

// Handler serves the /api/auth/* endpoints.
type Handler struct {
	DB     *db.DB
	Config config.Config
}

// UserFromContext returns the authenticated user stored by RequireAuth.
func UserFromContext(ctx context.Context) *model.User {
	u, _ := ctx.Value(ctxKey{}).(*model.User)
	return u
}

type ctxKey struct{}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func validUsername(u string) bool { return usernameRe.MatchString(u) }

func publicUser(u *model.User) map[string]any {
	return map[string]any{
		"id":         u.ID,
		"username":   u.Username,
		"is_admin":   u.IsAdmin,
		"created_at": u.CreatedAt,
	}
}

// trustedUser resolves the user from the trusted proxy header, auto-
// provisioning on first sight. Returns nil when the mode is disabled or the
// header is absent.
func (h *Handler) trustedUser(r *http.Request) *model.User {
	if h.Config.TrustedHeader == "" {
		return nil
	}
	username := strings.TrimSpace(r.Header.Get(h.Config.TrustedHeader))
	if username == "" || !validUsername(username) {
		return nil
	}
	u, err := h.DB.GetUserByUsername(username)
	if err != nil || u != nil {
		return u
	}
	count, err := h.DB.CountUsers()
	if err != nil {
		return nil
	}
	u = &model.User{
		ID:           db.NewID(),
		Username:     username,
		PasswordHash: "!", // unusable: auth comes from the proxy
		IsAdmin:      count == 0,
		CreatedAt:    db.Now(),
	}
	if err := h.DB.CreateUser(u); err != nil {
		return nil
	}
	return u
}

// Register creates a new user. The first-ever user becomes an admin.
func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	if !h.Config.RegOpen {
		writeErr(w, http.StatusForbidden, "registration is disabled")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if !validUsername(req.Username) {
		writeErr(w, http.StatusBadRequest, "username must be 3-32 chars: letters, digits, _ . -")
		return
	}
	if len(req.Password) < 8 {
		writeErr(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	existing, err := h.DB.GetUserByUsername(req.Username)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if existing != nil {
		writeErr(w, http.StatusConflict, "username already taken")
		return
	}
	count, err := h.DB.CountUsers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not hash password")
		return
	}
	u := &model.User{
		ID:           db.NewID(),
		Username:     req.Username,
		PasswordHash: string(hash),
		IsAdmin:      count == 0, // first user becomes admin
		CreatedAt:    db.Now(),
	}
	if err := h.DB.CreateUser(u); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	h.setSessionCookie(w, u.ID)
	writeJSON(w, http.StatusCreated, publicUser(u))
}

// Login verifies credentials and issues a session cookie.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	u, err := h.DB.GetUserByUsername(strings.TrimSpace(req.Username))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if u == nil || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Password)) != nil {
		writeErr(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	h.setSessionCookie(w, u.ID)
	writeJSON(w, http.StatusOK, publicUser(u))
}

// Logout deletes the session token and clears the cookie.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		_ = h.DB.DeleteAuthSession(c.Value)
	}
	h.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

// Me returns the current authenticated user.
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, publicUser(UserFromContext(r.Context())))
}

func (h *Handler) setSessionCookie(w http.ResponseWriter, userID string) {
	token := db.NewToken()
	expires := time.Now().Add(sessionTTL)
	if err := h.DB.CreateAuthSession(token, userID, expires.Unix()); err != nil {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   h.Config.SecureCookie,
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
	})
}

func (h *Handler) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
}

// Authenticate resolves r to its user via the trusted header or the session
// cookie, or nil when there is no valid identity. Unlike RequireAuth it
// writes no response.
func (h *Handler) Authenticate(r *http.Request) *model.User {
	if u := h.trustedUser(r); u != nil {
		return u
	}
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil
	}
	sess, err := h.DB.GetAuthSession(c.Value)
	if err != nil || sess == nil || sess.ExpiresAt < db.Now() {
		return nil
	}
	u, err := h.DB.GetUserByID(sess.UserID)
	if err != nil {
		return nil
	}
	return u
}

// RequireAuth is middleware that establishes the request user (trusted header
// first, then session cookie) and puts it in the request context.
func (h *Handler) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := h.trustedUser(r); u != nil {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
			return
		}
		c, err := r.Cookie(cookieName)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		sess, err := h.DB.GetAuthSession(c.Value)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "database error")
			return
		}
		if sess == nil || sess.ExpiresAt < db.Now() {
			if sess != nil {
				_ = h.DB.DeleteAuthSession(sess.Token)
			}
			h.clearSessionCookie(w)
			writeErr(w, http.StatusUnauthorized, "session expired")
			return
		}
		u, err := h.DB.GetUserByID(sess.UserID)
		if err != nil || u == nil {
			writeErr(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u)))
	})
}
