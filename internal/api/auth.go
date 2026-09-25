package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	cookieName = "noema_session"
	// csrfHeader must accompany every state-changing request authenticated by
	// cookie. Browsers cannot attach a custom header cross-origin without a CORS
	// preflight, which this server never approves. Native clients use Bearer
	// tokens and are unaffected.
	csrfHeader = "X-Noema-Client"
)

// handleLogin verifies the password and issues a JWT. Body:
// {"password": "...", "cookie": true} — cookie=true (the web client) also sets
// an HttpOnly session cookie so the token never touches JavaScript.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if wait := s.logins.blocked(ip); wait > 0 {
		w.Header().Set("Retry-After", itoa(int64(wait/time.Second)+1))
		writeError(w, http.StatusTooManyRequests, "too many failed attempts; try again later")
		return
	}
	var req struct {
		Password string `json:"password"`
		Cookie   bool   `json:"cookie"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if !decode(w, r, &req) {
		return
	}
	if !passwordMatches(req.Password, s.cfg.Password) {
		s.logins.fail(ip)
		time.Sleep(s.failDelay) // blunt online guessing further
		writeError(w, http.StatusUnauthorized, "invalid password")
		return
	}
	s.logins.succeed(ip)
	token, exp, err := s.issueToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "token error")
		return
	}
	if req.Cookie {
		http.SetCookie(w, &http.Cookie{
			Name: cookieName, Value: token, Path: "/", Expires: exp,
			HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteStrictMode,
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "expires_at": exp.Unix()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "expires_at": exp.Unix()})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

// passwordMatches compares in constant time (hashing first equalises lengths).
func passwordMatches(got, want string) bool {
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1 && want != ""
}

func (s *Server) issueToken() (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(s.cfg.TokenTTL)
	c := jwt.RegisteredClaims{
		ExpiresAt: jwt.NewNumericDate(exp),
		IssuedAt:  jwt.NewNumericDate(now),
		Subject:   "user",
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(s.cfg.JWTSecret))
	return tok, exp, err
}

// authMiddleware accepts a Bearer token (native clients) or the session
// cookie (web client, which must also send the CSRF header on writes).
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenStr, viaCookie := "", false
		if hdr := r.Header.Get("Authorization"); strings.HasPrefix(hdr, "Bearer ") {
			tokenStr = strings.TrimPrefix(hdr, "Bearer ")
		} else if c, err := r.Cookie(cookieName); err == nil {
			tokenStr, viaCookie = c.Value, true
		}
		if tokenStr == "" {
			writeError(w, http.StatusUnauthorized, "missing token")
			return
		}
		token, err := jwt.ParseWithClaims(tokenStr, &jwt.RegisteredClaims{}, func(t *jwt.Token) (any, error) {
			return []byte(s.cfg.JWTSecret), nil
		}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
		if err != nil || !token.Valid {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		if viaCookie && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(csrfHeader) == "" {
			writeError(w, http.StatusForbidden, "missing "+csrfHeader+" header")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// loginLimiter throttles password guessing. Per-IP limits stop a single
// client; the global limit bounds an attacker rotating addresses (or forging
// X-Forwarded-For). Tripping it only blocks new logins: devices that already
// hold tokens keep working.
type loginLimiter struct {
	mu     sync.Mutex
	perIP  map[string][]time.Time
	global []time.Time
	now    func() time.Time
}

const (
	loginWindow    = 15 * time.Minute
	loginPerIP     = 8
	loginGlobalMax = 40
)

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{perIP: map[string][]time.Time{}, now: time.Now}
}

func prune(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && ts[i].Before(cutoff) {
		i++
	}
	return ts[i:]
}

func (l *loginLimiter) blocked(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cutoff := now.Add(-loginWindow)
	l.global = prune(l.global, cutoff)
	l.perIP[ip] = prune(l.perIP[ip], cutoff)
	if len(l.perIP[ip]) >= loginPerIP {
		return l.perIP[ip][0].Add(loginWindow).Sub(now)
	}
	if len(l.global) >= loginGlobalMax {
		return l.global[0].Add(loginWindow).Sub(now)
	}
	return 0
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.perIP[ip] = append(l.perIP[ip], now)
	l.global = append(l.global, now)
	if len(l.perIP) > 10000 { // bound memory under address rotation
		l.perIP = map[string][]time.Time{}
	}
}

func (l *loginLimiter) succeed(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.perIP, ip)
}
