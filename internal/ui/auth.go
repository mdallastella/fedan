package ui

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie = "fedan_session"
	sessionTTL    = 30 * 24 * time.Hour
)

// auth checks the single UI password and signs stateless session cookies:
// "<expiry>.<hmac>". The MAC covers the configured password, so changing it
// logs every session out.
type auth struct {
	key      []byte
	hash     string
	password string
}

func (a *auth) check(password string) bool {
	if a.hash != "" {
		return bcrypt.CompareHashAndPassword([]byte(a.hash), []byte(password)) == nil
	}
	return subtle.ConstantTimeCompare([]byte(a.password), []byte(password)) == 1
}

func (a *auth) sign(expiry int64) string {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(strconv.FormatInt(expiry, 10) + "|" + a.hash + "|" + a.password))
	return hex.EncodeToString(m.Sum(nil))
}

func (a *auth) valid(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	exp, mac, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	expiry, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() > expiry {
		return false
	}
	return hmac.Equal([]byte(mac), []byte(a.sign(expiry)))
}

func (a *auth) setCookie(w http.ResponseWriter, r *http.Request) {
	expiry := time.Now().Add(sessionTTL).Unix()
	// Secure only over HTTPS, or plain-HTTP LAN access could never log in.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // see above
		Name:     sessionCookie,
		Value:    strconv.FormatInt(expiry, 10) + "." + a.sign(expiry),
		Path:     "/",
		Expires:  time.Unix(expiry, 0),
		HttpOnly: true,
		Secure:   secure(r),
		SameSite: http.SameSiteStrictMode,
	})
}

// secure reports whether the request reached us over HTTPS, directly or via
// a TLS-terminating proxy. Secure cookies over plain HTTP would be dropped.
func secure(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func (a *auth) require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" || strings.HasPrefix(r.URL.Path, "/static/") || a.valid(r) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.valid(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login", "", page{Title: "Log in"})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !s.auth.check(r.FormValue("password")) {
		// Slow down guessing.
		time.Sleep(time.Second)
		s.log.Warn("failed login", "remote", r.RemoteAddr)
		s.render(w, r, "login", "", page{Title: "Log in", Error: "Wrong password."})
		return
	}
	s.auth.setCookie(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, //nolint:gosec // as in setCookie
		HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
