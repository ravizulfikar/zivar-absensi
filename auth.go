package main

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const sessionCookieName = "absensi55_session"

type SessionUser struct {
	ID       int    `json:"id"`
	Role     string `json:"role"` // "admin" atau "pengajar"
	Username string `json:"username"`
	Nama     string `json:"nama"`
	Exp      int64  `json:"exp"`
}

type loginAttemptState struct {
	count   int
	resetAt time.Time
}

type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]loginAttemptState
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{entries: make(map[string]loginAttemptState)}
}

func (l *loginLimiter) allow(key string) bool {
	if l == nil {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) > 2000 {
		for k, v := range l.entries {
			if now.After(v.resetAt) {
				delete(l.entries, k)
			}
		}
	}
	st, ok := l.entries[key]
	if !ok || now.After(st.resetAt) {
		l.entries[key] = loginAttemptState{count: 1, resetAt: now.Add(10 * time.Minute)}
		return true
	}
	if st.count >= 10 {
		return false
	}
	st.count++
	l.entries[key] = st
	return true
}

func (l *loginLimiter) reset(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	delete(l.entries, key)
	l.mu.Unlock()
}

func clientIP(r *http.Request) string {
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		if first, _, ok := strings.Cut(xff, ","); ok {
			return strings.TrimSpace(first)
		}
		return xff
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}

func verifyLegacyPassword(inputPass, storedHash string) bool {
	stored := strings.TrimSpace(storedHash)
	if stored == "" || inputPass == "" {
		return false
	}
	if strings.HasPrefix(stored, "$2y$") || strings.HasPrefix(stored, "$2a$") || strings.HasPrefix(stored, "$2b$") {
		normalized := stored
		if strings.HasPrefix(normalized, "$2y$") {
			normalized = "$2a$" + normalized[4:]
		}
		return bcrypt.CompareHashAndPassword([]byte(normalized), []byte(inputPass)) == nil
	}
	sum := md5.Sum([]byte(inputPass))
	md5Hex := hex.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(strings.ToLower(stored)), []byte(md5Hex)) == 1
}

// authenticateStaff mendukung autentikasi pada:
// 1. Database Revamp Go (unbk_credentials + unbk_admins / unbk_teachers)
// 2. Database Legacy PHP (tb_admin / tb_pengajar dengan MD5 atau bcrypt)
func (a *App) authenticateStaff(ctx context.Context, username, password string) (*SessionUser, error) {
	u := strings.TrimSpace(username)
	if u == "" || password == "" {
		return nil, nil
	}

	// 1. Cek skema Revamp (unbk_credentials) jika tersedia
	if a.schema.HasCredentials {
		if user := a.authViaRevampCredentials(ctx, u, password); user != nil {
			return user, nil
		}
	}

	// 2. Cek tabel legacy tb_admin & tb_pengajar jika mode legacy (atau fallback jika tabel legacy ada)
	if a.schema.Mode == "legacy" {
		if user := a.authViaLegacyTables(ctx, u, password); user != nil {
			return user, nil
		}
	}

	return nil, nil
}

func (a *App) authViaRevampCredentials(ctx context.Context, username, password string) *SessionUser {
	// Admin di revamp: role='admin' -> unbk_admins
	var id int
	var nama, dbUser, hash string
	err := a.db.QueryRowContext(ctx,
		`SELECT a.id_admin, COALESCE(NULLIF(a.nama_lengkap,''), c.username), c.username, c.password_hash
		 FROM unbk_credentials c
		 INNER JOIN `+a.schema.AdminsTable+` a ON a.id_admin = c.legacy_id
		 WHERE c.role = 'admin' AND c.username = ? LIMIT 1`, username).
		Scan(&id, &nama, &dbUser, &hash)
	if err == nil && verifyLegacyPassword(password, hash) {
		return &SessionUser{
			ID:       id,
			Role:     "admin",
			Username: dbUser,
			Nama:     nama,
			Exp:      time.Now().Add(12 * time.Hour).Unix(),
		}
	}

	// Pengajar di revamp: role='teacher' -> unbk_teachers
	var status sql.NullString
	err = a.db.QueryRowContext(ctx,
		`SELECT t.id_pengajar, COALESCE(NULLIF(t.nama_lengkap,''), c.username), c.username, c.password_hash, t.status
		 FROM unbk_credentials c
		 INNER JOIN `+a.schema.TeachersTable+` t ON t.id_pengajar = c.legacy_id
		 WHERE c.role = 'teacher' AND c.username = ? LIMIT 1`, username).
		Scan(&id, &nama, &dbUser, &hash, &status)
	if err == nil && verifyLegacyPassword(password, hash) {
		if status.Valid && strings.EqualFold(strings.TrimSpace(status.String), "tidak aktif") {
			return nil
		}
		return &SessionUser{
			ID:       id,
			Role:     "pengajar",
			Username: dbUser,
			Nama:     nama,
			Exp:      time.Now().Add(12 * time.Hour).Unix(),
		}
	}
	return nil
}

func (a *App) authViaLegacyTables(ctx context.Context, username, password string) *SessionUser {
	var id int
	var nama, dbUser, dbPass string
	err := a.db.QueryRowContext(ctx,
		`SELECT id_admin, COALESCE(nama_lengkap, username), username, password FROM tb_admin WHERE username = ? LIMIT 1`, username).
		Scan(&id, &nama, &dbUser, &dbPass)
	if err == nil && verifyLegacyPassword(password, dbPass) {
		return &SessionUser{
			ID:       id,
			Role:     "admin",
			Username: dbUser,
			Nama:     nama,
			Exp:      time.Now().Add(12 * time.Hour).Unix(),
		}
	}

	var status sql.NullString
	err = a.db.QueryRowContext(ctx,
		`SELECT id_pengajar, COALESCE(nama_lengkap, username), username, password, status FROM tb_pengajar WHERE username = ? LIMIT 1`, username).
		Scan(&id, &nama, &dbUser, &dbPass, &status)
	if err == nil && verifyLegacyPassword(password, dbPass) {
		if status.Valid && strings.EqualFold(strings.TrimSpace(status.String), "tidak aktif") {
			return nil
		}
		return &SessionUser{
			ID:       id,
			Role:     "pengajar",
			Username: dbUser,
			Nama:     nama,
			Exp:      time.Now().Add(12 * time.Hour).Unix(),
		}
	}
	return nil
}

func (a *App) signSession(u *SessionUser) (string, error) {
	raw, err := json.Marshal(u)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, []byte(a.cfg.SessionSecret))
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payload + "." + sig, nil
}

func (a *App) parseSession(r *http.Request) *SessionUser {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	payload, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return nil
	}
	mac := hmac.New(sha256.New, []byte(a.cfg.SessionSecret))
	mac.Write([]byte(payload))
	expected := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(sig), []byte(expected)) != 1 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil
	}
	var u SessionUser
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil
	}
	if time.Now().Unix() > u.Exp {
		return nil
	}
	return &u
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Payload login tidak valid"})
		return
	}
	limitKey := clientIP(r) + "|" + strings.ToLower(strings.TrimSpace(req.Username))
	if !a.limiter.allow(limitKey) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "error": "Terlalu banyak percobaan login. Tunggu beberapa menit lalu coba lagi."})
		return
	}
	user, err := a.authenticateStaff(r.Context(), req.Username, req.Password)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Gagal memeriksa akun"})
		return
	}
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Username atau password tidak cocok dengan akun admin/pengajar"})
		return
	}
	a.limiter.reset(limitKey)

	token, err := a.signSession(user)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Gagal membuat sesi"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cfg.SecureCookie,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   12 * 3600,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": user, "schema": a.schema.Mode})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cfg.SecureCookie,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleMe(w http.ResponseWriter, r *http.Request) {
	u := a.parseSession(r)
	if u == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Belum login"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": u, "schema": a.schema.Mode})
}

func (a *App) requireAuth(next func(http.ResponseWriter, *http.Request, *SessionUser)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := a.parseSession(r)
		if u == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Sesi berakhir, silakan login ulang"})
			return
		}
		next(w, r, u)
	}
}

func (a *App) requireAdmin(next func(http.ResponseWriter, *http.Request, *SessionUser)) http.HandlerFunc {
	return a.requireAuth(func(w http.ResponseWriter, r *http.Request, u *SessionUser) {
		if u.Role != "admin" {
			writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "Hanya administrator yang dapat mengubah pengaturan ini"})
			return
		}
		next(w, r, u)
	})
}
