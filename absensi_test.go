package main

import (
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestVerifyLegacyPassword(t *testing.T) {
	sum := md5.Sum([]byte("rahasia123"))
	md5Hash := hex.EncodeToString(sum[:])

	if !verifyLegacyPassword("rahasia123", md5Hash) {
		t.Fatal("MD5 valid password should pass")
	}
	if verifyLegacyPassword("salah", md5Hash) {
		t.Fatal("MD5 invalid password should fail")
	}

	bcryptBytes, err := bcrypt.GenerateFromPassword([]byte("admin55"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt generate failed: %v", err)
	}
	if !verifyLegacyPassword("admin55", string(bcryptBytes)) {
		t.Fatal("bcrypt valid password should pass")
	}
	// PHP $2y$ prefix compatibility
	phpHash := "$2y$" + string(bcryptBytes)[4:]
	if !verifyLegacyPassword("admin55", phpHash) {
		t.Fatal("PHP $2y$ bcrypt valid password should pass")
	}
	if verifyLegacyPassword("wrong", phpHash) {
		t.Fatal("PHP $2y$ bcrypt invalid password should fail")
	}
}

func TestSessionSignAndParse(t *testing.T) {
	app := &App{cfg: Config{SessionSecret: "test-secret-key-32-chars-minimum"}}
	user := &SessionUser{
		ID:       7,
		Role:     "admin",
		Username: "admin55",
		Nama:     "Administrator",
		Exp:      time.Now().Add(time.Hour).Unix(),
	}

	token, err := app.signSession(user)
	if err != nil {
		t.Fatalf("signSession error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	parsed := app.parseSession(req)
	if parsed == nil || parsed.Username != "admin55" || parsed.Role != "admin" {
		t.Fatalf("expected valid parsed session, got %+v", parsed)
	}

	// Tampered token must be rejected
	reqBad := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	reqBad.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token + "x"})
	if app.parseSession(reqBad) != nil {
		t.Fatal("tampered session token should be rejected")
	}

	// Expired token must be rejected
	expiredUser := &SessionUser{ID: 1, Role: "admin", Username: "old", Exp: time.Now().Add(-time.Minute).Unix()}
	expTok, _ := app.signSession(expiredUser)
	reqExp := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	reqExp.AddCookie(&http.Cookie{Name: sessionCookieName, Value: expTok})
	if app.parseSession(reqExp) != nil {
		t.Fatal("expired session token should be rejected")
	}
}

func TestGenerateQRTokenAndTimeValidation(t *testing.T) {
	tok := generateQRToken("2025 001/A")
	if !strings.HasPrefix(tok, "ABS55-2025001A-") {
		t.Fatalf("unexpected QR token format: %s", tok)
	}
	if !isValidHHMM("07:00") || !isValidHHMM("23:59") {
		t.Fatal("valid HH:MM rejected")
	}
	if isValidHHMM("24:00") || isValidHHMM("7:00") || isValidHHMM("ab:cd") {
		t.Fatal("invalid HH:MM accepted")
	}
}

func TestEmbeddedStaticPages(t *testing.T) {
	app := &App{cfg: Config{SessionSecret: "test"}, schema: revampSchema(true, true), limiter: newLoginLimiter()}
	mux := app.routes()

	for _, path := range []string{"/", "/admin", "/print-qr", "/assets/styles.css"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s returned %d, want 200", path, rec.Code)
		}
	}
}

func TestAdminOnlySettingsAndExportValidation(t *testing.T) {
	app := &App{cfg: Config{SessionSecret: "test-secret-32-chars-minimum", Location: time.FixedZone("WIB", 7*3600)}, schema: revampSchema(true, true), limiter: newLoginLimiter()}
	mux := app.routes()

	teacherTok, err := app.signSession(&SessionUser{
		ID:       2,
		Role:     "pengajar",
		Username: "guru1",
		Nama:     "Guru Satu",
		Exp:      time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("signSession failed: %v", err)
	}

	// Pengajar must be forbidden from saving settings
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/settings", strings.NewReader(`{"nama_sekolah":"X"}`))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: teacherTok})
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for pengajar saving settings, got %d", rec.Code)
	}

	// Export CSV with invalid date must return 400
	recExp := httptest.NewRecorder()
	reqExp := httptest.NewRequest(http.MethodGet, "/api/admin/export?dari=2026-99-99&sampai=2026-01-01", nil)
	reqExp.AddCookie(&http.Cookie{Name: sessionCookieName, Value: teacherTok})
	mux.ServeHTTP(recExp, reqExp)
	if recExp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid export date, got %d", recExp.Code)
	}
}

func TestLoginRateLimiterAndSchemas(t *testing.T) {
	lim := newLoginLimiter()
	for i := 0; i < 10; i++ {
		if !lim.allow("127.0.0.1|admin") {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
	}
	if lim.allow("127.0.0.1|admin") {
		t.Fatal("11th attempt should be rate-limited")
	}
	lim.reset("127.0.0.1|admin")
	if !lim.allow("127.0.0.1|admin") {
		t.Fatal("attempt after reset should be allowed")
	}

	r := revampSchema(true, true)
	if r.StudentsTable != "unbk_students" || r.ClassesTable != "unbk_classes" || !r.HasCredentials {
		t.Fatalf("unexpected revamp schema: %+v", r)
	}
	l := legacySchema(false, false)
	if l.StudentsTable != "tb_siswa" || l.ClassesTable != "tb_kelas" || l.HasCredentials {
		t.Fatalf("unexpected legacy schema: %+v", l)
	}
}
