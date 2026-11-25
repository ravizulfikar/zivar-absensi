package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

type Settings struct {
	NamaSekolah    string `json:"nama_sekolah"`
	JamMasukBatas  string `json:"jam_masuk_batas"`
	JamPulangMulai string `json:"jam_pulang_mulai"`
	CooldownDetik  int    `json:"cooldown_detik"`
	IzinkanQRNIS   bool   `json:"izinkan_qr_nis"`
	KioskAPIKey    string `json:"kiosk_api_key"`
}

// DBSchema memetakan tabel sumber agar aplikasi berjalan lancar baik pada
// database legacy (tb_siswa, tb_kelas, tb_admin, tb_pengajar) maupun
// database revamp Go (unbk_students, unbk_classes, unbk_admins, unbk_teachers, unbk_credentials).
type DBSchema struct {
	Mode             string // "revamp" atau "legacy"
	StudentsTable    string
	ClassesTable     string
	AdminsTable      string
	TeachersTable    string
	HasCredentials   bool
	HasSchoolProfile bool
}

func revampSchema(hasCreds, hasSchool bool) DBSchema {
	return DBSchema{
		Mode:             "revamp",
		StudentsTable:    "unbk_students",
		ClassesTable:     "unbk_classes",
		AdminsTable:      "unbk_admins",
		TeachersTable:    "unbk_teachers",
		HasCredentials:   hasCreds,
		HasSchoolProfile: hasSchool,
	}
}

func legacySchema(hasCreds, hasSchool bool) DBSchema {
	return DBSchema{
		Mode:             "legacy",
		StudentsTable:    "tb_siswa",
		ClassesTable:     "tb_kelas",
		AdminsTable:      "tb_admin",
		TeachersTable:    "tb_pengajar",
		HasCredentials:   hasCreds,
		HasSchoolProfile: hasSchool,
	}
}

func tableExists(ctx context.Context, db *sql.DB, name string) bool {
	var count int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`,
		name).Scan(&count)
	return err == nil && count > 0
}

func detectSchema(ctx context.Context, db *sql.DB, forcedMode string) (DBSchema, error) {
	hasRevamp := tableExists(ctx, db, "unbk_students") && tableExists(ctx, db, "unbk_classes")
	hasLegacy := tableExists(ctx, db, "tb_siswa") && tableExists(ctx, db, "tb_kelas")
	hasCreds := tableExists(ctx, db, "unbk_credentials")
	hasSchool := tableExists(ctx, db, "unbk_school_profile")

	switch forcedMode {
	case "revamp":
		if !hasRevamp {
			return DBSchema{}, fmt.Errorf("DB_SCHEMA=revamp tetapi tabel unbk_students / unbk_classes tidak ditemukan")
		}
		return revampSchema(hasCreds, hasSchool), nil
	case "legacy":
		if !hasLegacy {
			return DBSchema{}, fmt.Errorf("DB_SCHEMA=legacy tetapi tabel tb_siswa / tb_kelas tidak ditemukan")
		}
		return legacySchema(hasCreds, hasSchool), nil
	default:
		if hasRevamp {
			return revampSchema(hasCreds, hasSchool), nil
		}
		if hasLegacy {
			return legacySchema(hasCreds, hasSchool), nil
		}
		return DBSchema{}, fmt.Errorf("tabel siswa tidak ditemukan (tidak ada unbk_students maupun tb_siswa di database)")
	}
}

func openDB(cfg Config) (*sql.DB, error) {
	locParam := url.QueryEscape(cfg.Location.String())
	if cfg.Location.String() == "WIB" {
		locParam = "Local"
	}
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?parseTime=true&loc=%s&charset=utf8mb4,utf8&timeout=5s",
		cfg.DBUser, cfg.DBPass, cfg.DBHost, cfg.DBPort, cfg.DBName, locParam)

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// migrateAttendanceTables membuat tabel khusus absensi dengan DDL yang 100% kompatibel
// dengan MariaDB 5.5.68 (tanpa DATETIME DEFAULT CURRENT_TIMESTAMP / ON UPDATE CURRENT_TIMESTAMP)
// maupun MariaDB 10+ / MySQL 8+, tanpa mengubah tabel legacy atau revamp.
func migrateAttendanceTables(ctx context.Context, db *sql.DB, schema DBSchema) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS tb_absensi_kartu (
			id_siswa INT(11) NOT NULL,
			rfid_uid VARCHAR(64) DEFAULT NULL,
			qr_token VARCHAR(64) NOT NULL,
			status_kartu ENUM('aktif','nonaktif') NOT NULL DEFAULT 'aktif',
			updated_at DATETIME DEFAULT NULL,
			PRIMARY KEY (id_siswa),
			UNIQUE KEY uq_absensi_rfid (rfid_uid),
			UNIQUE KEY uq_absensi_qr (qr_token)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

		`CREATE TABLE IF NOT EXISTS tb_absensi_harian (
			id BIGINT(20) NOT NULL AUTO_INCREMENT,
			id_siswa INT(11) NOT NULL,
			tanggal DATE NOT NULL,
			jam_masuk DATETIME DEFAULT NULL,
			jam_pulang DATETIME DEFAULT NULL,
			status ENUM('hadir','terlambat','izin','sakit','alpha') NOT NULL DEFAULT 'hadir',
			metode_masuk ENUM('rfid','qr','manual') DEFAULT NULL,
			metode_pulang ENUM('rfid','qr','manual') DEFAULT NULL,
			kiosk_masuk VARCHAR(64) DEFAULT NULL,
			kiosk_pulang VARCHAR(64) DEFAULT NULL,
			keterangan VARCHAR(255) NOT NULL DEFAULT '',
			updated_by VARCHAR(64) NOT NULL DEFAULT '',
			created_at DATETIME DEFAULT NULL,
			updated_at DATETIME DEFAULT NULL,
			PRIMARY KEY (id),
			UNIQUE KEY uq_absensi_siswa_tanggal (id_siswa, tanggal),
			KEY idx_absensi_tanggal_status (tanggal, status)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

		`CREATE TABLE IF NOT EXISTS tb_absensi_pengaturan (
			kunci VARCHAR(64) NOT NULL,
			nilai VARCHAR(255) NOT NULL,
			PRIMARY KEY (kunci)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,

		`CREATE TABLE IF NOT EXISTS tb_absensi_log (
			id BIGINT(20) NOT NULL AUTO_INCREMENT,
			waktu DATETIME NOT NULL,
			kode VARCHAR(100) NOT NULL,
			metode ENUM('rfid','qr') NOT NULL DEFAULT 'rfid',
			kiosk VARCHAR(64) NOT NULL DEFAULT 'kiosk-utama',
			id_siswa INT(11) DEFAULT NULL,
			hasil VARCHAR(32) NOT NULL,
			pesan VARCHAR(255) NOT NULL,
			PRIMARY KEY (id),
			KEY idx_absensi_log_waktu (waktu),
			KEY idx_absensi_log_hasil (hasil)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	}

	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("migrasi tabel absensi gagal: %w", err)
		}
	}

	defaultSchool := "SMK NEGERI 55 JAKARTA"
	if schema.HasSchoolProfile {
		var name string
		if err := db.QueryRowContext(ctx, `SELECT name FROM unbk_school_profile WHERE id = 1 LIMIT 1`).Scan(&name); err == nil && strings.TrimSpace(name) != "" {
			defaultSchool = strings.TrimSpace(name)
		}
	}

	defaults := map[string]string{
		"nama_sekolah":     defaultSchool,
		"jam_masuk_batas":  "07:00",
		"jam_pulang_mulai": "14:00",
		"cooldown_detik":   "30",
		"izinkan_qr_nis":   "1",
		"kiosk_api_key":    "",
	}
	for k, v := range defaults {
		if _, err := db.ExecContext(ctx,
			`INSERT IGNORE INTO tb_absensi_pengaturan (kunci, nilai) VALUES (?, ?)`, k, v); err != nil {
			return fmt.Errorf("seed pengaturan %s gagal: %w", k, err)
		}
	}
	return nil
}

func loadSettings(ctx context.Context, db *sql.DB) Settings {
	s := Settings{
		NamaSekolah:    "SMK NEGERI 55 JAKARTA",
		JamMasukBatas:  "07:00",
		JamPulangMulai: "14:00",
		CooldownDetik:  30,
		IzinkanQRNIS:   true,
		KioskAPIKey:    "",
	}
	rows, err := db.QueryContext(ctx, `SELECT kunci, nilai FROM tb_absensi_pengaturan`)
	if err != nil {
		return s
	}
	defer rows.Close()

	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			continue
		}
		switch k {
		case "nama_sekolah":
			if strings.TrimSpace(v) != "" {
				s.NamaSekolah = strings.TrimSpace(v)
			}
		case "jam_masuk_batas":
			if isValidHHMM(v) {
				s.JamMasukBatas = v
			}
		case "jam_pulang_mulai":
			if isValidHHMM(v) {
				s.JamPulangMulai = v
			}
		case "cooldown_detik":
			if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 3600 {
				s.CooldownDetik = n
			}
		case "izinkan_qr_nis":
			s.IzinkanQRNIS = v == "1" || strings.EqualFold(v, "true")
		case "kiosk_api_key":
			s.KioskAPIKey = strings.TrimSpace(v)
		}
	}
	return s
}

func saveSettings(ctx context.Context, db *sql.DB, s Settings) error {
	if strings.TrimSpace(s.NamaSekolah) == "" {
		s.NamaSekolah = "SMK NEGERI 55 JAKARTA"
	}
	if !isValidHHMM(s.JamMasukBatas) {
		return fmt.Errorf("format jam_masuk_batas harus HH:MM")
	}
	if !isValidHHMM(s.JamPulangMulai) {
		return fmt.Errorf("format jam_pulang_mulai harus HH:MM")
	}
	if s.CooldownDetik < 0 || s.CooldownDetik > 3600 {
		return fmt.Errorf("cooldown_detik harus antara 0 sampai 3600")
	}
	qrNIS := "0"
	if s.IzinkanQRNIS {
		qrNIS = "1"
	}
	items := map[string]string{
		"nama_sekolah":     strings.TrimSpace(s.NamaSekolah),
		"jam_masuk_batas":  s.JamMasukBatas,
		"jam_pulang_mulai": s.JamPulangMulai,
		"cooldown_detik":   strconv.Itoa(s.CooldownDetik),
		"izinkan_qr_nis":   qrNIS,
		"kiosk_api_key":    strings.TrimSpace(s.KioskAPIKey),
	}
	for k, v := range items {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO tb_absensi_pengaturan (kunci, nilai) VALUES (?, ?)
			 ON DUPLICATE KEY UPDATE nilai = VALUES(nilai)`, k, v); err != nil {
			return err
		}
	}
	return nil
}

func isValidHHMM(s string) bool {
	if len(s) != 5 || s[2] != ':' {
		return false
	}
	h, err1 := strconv.Atoi(s[:2])
	m, err2 := strconv.Atoi(s[3:])
	return err1 == nil && err2 == nil && h >= 0 && h <= 23 && m >= 0 && m <= 59
}

func generateQRToken(nis string) string {
	clean := strings.Map(func(r rune) rune {
		if (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			return r
		}
		return -1
	}, strings.TrimSpace(nis))
	if clean == "" {
		clean = "SISWA"
	}
	clean = strings.ToUpper(clean)
	if len(clean) > 16 {
		clean = clean[:16]
	}
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("ABS55-%s-%s", clean, strings.ToUpper(hex.EncodeToString(buf)))
}

func ensureStudentQRToken(ctx context.Context, db *sql.DB, idSiswa int, nis string) (string, error) {
	var existing string
	err := db.QueryRowContext(ctx, `SELECT qr_token FROM tb_absensi_kartu WHERE id_siswa = ?`, idSiswa).Scan(&existing)
	if err == nil && strings.TrimSpace(existing) != "" {
		return existing, nil
	}
	token := generateQRToken(nis)
	_, err = db.ExecContext(ctx,
		`INSERT INTO tb_absensi_kartu (id_siswa, qr_token, status_kartu, updated_at) VALUES (?, ?, 'aktif', NOW())
		 ON DUPLICATE KEY UPDATE qr_token = IF(qr_token = '', VALUES(qr_token), qr_token), updated_at = NOW()`,
		idSiswa, token)
	if err != nil {
		return "", err
	}
	var finalToken string
	if err := db.QueryRowContext(ctx, `SELECT qr_token FROM tb_absensi_kartu WHERE id_siswa = ?`, idSiswa).Scan(&finalToken); err != nil {
		return token, nil
	}
	return finalToken, nil
}
