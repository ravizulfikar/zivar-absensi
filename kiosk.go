package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type ScanRequest struct {
	Kode   string `json:"kode"`
	Metode string `json:"metode"` // "rfid", "qr", atau "auto"
	Mode   string `json:"mode"`   // "auto", "masuk", "pulang"
	Kiosk  string `json:"kiosk"`
}

type StudentBrief struct {
	IDSiswa   int    `json:"id_siswa"`
	NIS       string `json:"nis"`
	Nama      string `json:"nama"`
	NamaKelas string `json:"nama_kelas"`
	Status    string `json:"status"`
}

type ScanResponse struct {
	OK          bool          `json:"ok"`
	Hasil       string        `json:"hasil"` // masuk, pulang, duplikat, tidak_dikenal, nonaktif
	Pesan       string        `json:"pesan"`
	Metode      string        `json:"metode"`
	StatusAbsen string        `json:"status_absen,omitempty"`
	Waktu       string        `json:"waktu"`
	Student     *StudentBrief `json:"student,omitempty"`
}

func (a *App) handleIoTScan(w http.ResponseWriter, r *http.Request) {
	settings := loadSettings(r.Context(), a.db)
	if settings.KioskAPIKey != "" {
		provided := strings.TrimSpace(r.Header.Get("X-API-Key"))
		if subtle.ConstantTimeCompare([]byte(provided), []byte(settings.KioskAPIKey)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"ok":    false,
				"error": "API key perangkat tidak valid",
			})
			return
		}
	}
	a.handleKioskScan(w, r)
}

func (a *App) handleKioskScan(w http.ResponseWriter, r *http.Request) {
	var req ScanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Format permintaan tidak valid"})
		return
	}
	kode := strings.TrimSpace(req.Kode)
	if kode == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Kode RFID atau QR kosong"})
		return
	}
	if len(kode) > 100 {
		kode = kode[:100]
	}

	kiosk := strings.TrimSpace(req.Kiosk)
	if kiosk == "" {
		kiosk = "kiosk-utama"
	}
	if len(kiosk) > 64 {
		kiosk = kiosk[:64]
	}

	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode != "masuk" && mode != "pulang" {
		mode = "auto"
	}

	reqMetode := strings.ToLower(strings.TrimSpace(req.Metode))
	if reqMetode != "rfid" && reqMetode != "qr" {
		reqMetode = "auto"
	}

	ctx := r.Context()
	settings := loadSettings(ctx, a.db)
	now := time.Now().In(a.cfg.Location)
	today := now.Format("2006-01-02")
	waktuStr := now.Format("15:04:05")

	student, cardStatus, resolvedMetode, err := a.lookupStudentByCode(ctx, kode, reqMetode, settings.IzinkanQRNIS)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Gagal memeriksa database siswa"})
		return
	}

	if student == nil {
		logMetode := reqMetode
		if logMetode == "auto" {
			if strings.HasPrefix(strings.ToUpper(kode), "ABS55-") {
				logMetode = "qr"
			} else {
				logMetode = "rfid"
			}
		}
		pesan := fmt.Sprintf("Kartu atau QR '%s' belum terdaftar pada data siswa", kode)
		a.insertScanLog(ctx, kode, logMetode, kiosk, nil, "tidak_dikenal", pesan)
		writeJSON(w, http.StatusNotFound, ScanResponse{
			OK:     false,
			Hasil:  "tidak_dikenal",
			Pesan:  pesan,
			Metode: logMetode,
			Waktu:  waktuStr,
		})
		return
	}

	if !strings.EqualFold(student.Status, "aktif") {
		pesan := fmt.Sprintf("Akun siswa %s berstatus tidak aktif", student.Nama)
		a.insertScanLog(ctx, kode, resolvedMetode, kiosk, &student.IDSiswa, "nonaktif", pesan)
		writeJSON(w, http.StatusForbidden, ScanResponse{
			OK:      false,
			Hasil:   "nonaktif",
			Pesan:   pesan,
			Metode:  resolvedMetode,
			Waktu:   waktuStr,
			Student: student,
		})
		return
	}

	if cardStatus == "nonaktif" {
		pesan := fmt.Sprintf("Kartu milik %s sedang dinonaktifkan oleh admin", student.Nama)
		a.insertScanLog(ctx, kode, resolvedMetode, kiosk, &student.IDSiswa, "nonaktif", pesan)
		writeJSON(w, http.StatusForbidden, ScanResponse{
			OK:      false,
			Hasil:   "nonaktif",
			Pesan:   pesan,
			Metode:  resolvedMetode,
			Waktu:   waktuStr,
			Student: student,
		})
		return
	}

	// Cek data absensi hari ini
	var recID int64
	var jamMasuk, jamPulang sql.NullTime
	var statusAbsen string
	err = a.db.QueryRowContext(ctx,
		`SELECT id, jam_masuk, jam_pulang, status FROM tb_absensi_harian WHERE id_siswa = ? AND tanggal = ? LIMIT 1`,
		student.IDSiswa, today).Scan(&recID, &jamMasuk, &jamPulang, &statusAbsen)

	if err != nil && err != sql.ErrNoRows {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Gagal membaca catatan absensi hari ini"})
		return
	}

	hasRecord := err == nil

	// Cooldown untuk mencegah double-tap tidak sengaja
	if hasRecord && settings.CooldownDetik > 0 {
		var lastTap time.Time
		if jamPulang.Valid {
			lastTap = jamPulang.Time
		} else if jamMasuk.Valid {
			lastTap = jamMasuk.Time
		}
		if !lastTap.IsZero() && now.Sub(lastTap) < time.Duration(settings.CooldownDetik)*time.Second {
			pesan := fmt.Sprintf("%s baru saja tercatat pukul %s", student.Nama, lastTap.Format("15:04:05"))
			a.insertScanLog(ctx, kode, resolvedMetode, kiosk, &student.IDSiswa, "duplikat", pesan)
			writeJSON(w, http.StatusOK, ScanResponse{
				OK:          true,
				Hasil:       "duplikat",
				Pesan:       pesan,
				Metode:      resolvedMetode,
				StatusAbsen: statusAbsen,
				Waktu:       waktuStr,
				Student:     student,
			})
			return
		}
	}

	nowHHMM := now.Format("15:04")

	// Kasus 1: Belum ada record hari ini atau jam_masuk belum terisi dan mode bukan pulang
	if (!hasRecord || !jamMasuk.Valid) && mode != "pulang" {
		st := "hadir"
		if nowHHMM > settings.JamMasukBatas {
			st = "terlambat"
		}
		if !hasRecord {
			_, err = a.db.ExecContext(ctx,
				`INSERT INTO tb_absensi_harian (id_siswa, tanggal, jam_masuk, status, metode_masuk, kiosk_masuk, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				student.IDSiswa, today, now, st, resolvedMetode, kiosk, now, now)
		} else {
			_, err = a.db.ExecContext(ctx,
				`UPDATE tb_absensi_harian SET jam_masuk = ?, status = ?, metode_masuk = ?, kiosk_masuk = ?, updated_at = ? WHERE id = ?`,
				now, st, resolvedMetode, kiosk, now, recID)
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Gagal menyimpan absensi masuk"})
			return
		}
		label := "Tepat Waktu"
		if st == "terlambat" {
			label = "Terlambat"
		}
		pesan := fmt.Sprintf("Absen masuk berhasil (%s) pukul %s", label, waktuStr)
		a.insertScanLog(ctx, kode, resolvedMetode, kiosk, &student.IDSiswa, "masuk", pesan)
		writeJSON(w, http.StatusOK, ScanResponse{
			OK:          true,
			Hasil:       "masuk",
			Pesan:       pesan,
			Metode:      resolvedMetode,
			StatusAbsen: st,
			Waktu:       waktuStr,
			Student:     student,
		})
		return
	}

	// Kasus 2: Mode paksa masuk padahal sudah absen masuk
	if mode == "masuk" && hasRecord && jamMasuk.Valid {
		pesan := fmt.Sprintf("%s sudah absen masuk pukul %s", student.Nama, jamMasuk.Time.Format("15:04:05"))
		a.insertScanLog(ctx, kode, resolvedMetode, kiosk, &student.IDSiswa, "duplikat", pesan)
		writeJSON(w, http.StatusOK, ScanResponse{
			OK:          true,
			Hasil:       "duplikat",
			Pesan:       pesan,
			Metode:      resolvedMetode,
			StatusAbsen: statusAbsen,
			Waktu:       waktuStr,
			Student:     student,
		})
		return
	}

	// Kasus 3: Mode auto, sudah masuk, tapi belum masuk jam pulang
	if mode == "auto" && hasRecord && jamMasuk.Valid && !jamPulang.Valid && nowHHMM < settings.JamPulangMulai {
		pesan := fmt.Sprintf("%s sudah absen masuk pukul %s. Absen pulang dibuka mulai %s",
			student.Nama, jamMasuk.Time.Format("15:04"), settings.JamPulangMulai)
		a.insertScanLog(ctx, kode, resolvedMetode, kiosk, &student.IDSiswa, "duplikat", pesan)
		writeJSON(w, http.StatusOK, ScanResponse{
			OK:          true,
			Hasil:       "duplikat",
			Pesan:       pesan,
			Metode:      resolvedMetode,
			StatusAbsen: statusAbsen,
			Waktu:       waktuStr,
			Student:     student,
		})
		return
	}

	// Kasus 4: Sudah absen pulang sebelumnya
	if hasRecord && jamPulang.Valid {
		pesan := fmt.Sprintf("%s sudah tercatat absen pulang pukul %s", student.Nama, jamPulang.Time.Format("15:04:05"))
		a.insertScanLog(ctx, kode, resolvedMetode, kiosk, &student.IDSiswa, "duplikat", pesan)
		writeJSON(w, http.StatusOK, ScanResponse{
			OK:          true,
			Hasil:       "duplikat",
			Pesan:       pesan,
			Metode:      resolvedMetode,
			StatusAbsen: statusAbsen,
			Waktu:       waktuStr,
			Student:     student,
		})
		return
	}

	// Kasus 5: Catat absen pulang
	if !hasRecord {
		_, err = a.db.ExecContext(ctx,
			`INSERT INTO tb_absensi_harian (id_siswa, tanggal, jam_pulang, status, metode_pulang, kiosk_pulang, created_at, updated_at)
			 VALUES (?, ?, ?, 'hadir', ?, ?, ?, ?)`,
			student.IDSiswa, today, now, resolvedMetode, kiosk, now, now)
		statusAbsen = "hadir"
	} else {
		_, err = a.db.ExecContext(ctx,
			`UPDATE tb_absensi_harian SET jam_pulang = ?, metode_pulang = ?, kiosk_pulang = ?, updated_at = ? WHERE id = ?`,
			now, resolvedMetode, kiosk, now, recID)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Gagal menyimpan absensi pulang"})
		return
	}

	pesan := fmt.Sprintf("Absen pulang berhasil pukul %s", waktuStr)
	a.insertScanLog(ctx, kode, resolvedMetode, kiosk, &student.IDSiswa, "pulang", pesan)
	writeJSON(w, http.StatusOK, ScanResponse{
		OK:          true,
		Hasil:       "pulang",
		Pesan:       pesan,
		Metode:      resolvedMetode,
		StatusAbsen: statusAbsen,
		Waktu:       waktuStr,
		Student:     student,
	})
}

func (a *App) lookupStudentByCode(ctx context.Context, kode, reqMetode string, allowNIS bool) (*StudentBrief, string, string, error) {
	stuTbl := a.schema.StudentsTable
	clsTbl := a.schema.ClassesTable

	queryCard := func(where string, arg any) (*StudentBrief, string, error) {
		var s StudentBrief
		var cardStatus string
		err := a.db.QueryRowContext(ctx,
			`SELECT s.id_siswa, COALESCE(s.nis, ''), COALESCE(s.nama_lengkap, ''),
			        COALESCE(k.nama_kelas, '-'), COALESCE(s.status, 'aktif'),
			        COALESCE(c.status_kartu, 'aktif')
			 FROM tb_absensi_kartu c
			 INNER JOIN `+stuTbl+` s ON s.id_siswa = c.id_siswa
			 LEFT JOIN `+clsTbl+` k ON CAST(k.id_kelas AS CHAR) = TRIM(s.id_kelas)
			 WHERE `+where+` LIMIT 1`, arg).
			Scan(&s.IDSiswa, &s.NIS, &s.Nama, &s.NamaKelas, &s.Status, &cardStatus)
		if err == sql.ErrNoRows {
			return nil, "", nil
		}
		if err != nil {
			return nil, "", err
		}
		return &s, cardStatus, nil
	}

	if reqMetode == "rfid" || reqMetode == "auto" {
		s, cs, err := queryCard("UPPER(c.rfid_uid) = UPPER(?)", kode)
		if err != nil || s != nil {
			return s, cs, "rfid", err
		}
	}

	if reqMetode == "qr" || reqMetode == "auto" {
		s, cs, err := queryCard("c.qr_token = ?", kode)
		if err != nil || s != nil {
			return s, cs, "qr", err
		}
		if allowNIS {
			var s StudentBrief
			var cardStatus sql.NullString
			err := a.db.QueryRowContext(ctx,
				`SELECT s.id_siswa, COALESCE(s.nis, ''), COALESCE(s.nama_lengkap, ''),
				        COALESCE(k.nama_kelas, '-'), COALESCE(s.status, 'aktif'),
				        c.status_kartu
				 FROM `+stuTbl+` s
				 LEFT JOIN `+clsTbl+` k ON CAST(k.id_kelas AS CHAR) = TRIM(s.id_kelas)
				 LEFT JOIN tb_absensi_kartu c ON c.id_siswa = s.id_siswa
				 WHERE TRIM(s.nis) = ? LIMIT 1`, kode).
				Scan(&s.IDSiswa, &s.NIS, &s.Nama, &s.NamaKelas, &s.Status, &cardStatus)
			if err == nil {
				cs := "aktif"
				if cardStatus.Valid && cardStatus.String != "" {
					cs = cardStatus.String
				}
				return &s, cs, "qr", nil
			}
			if err != sql.ErrNoRows {
				return nil, "", "qr", err
			}
		}
	}
	return nil, "", reqMetode, nil
}

func (a *App) insertScanLog(ctx context.Context, kode, metode, kiosk string, idSiswa *int, hasil, pesan string) {
	if metode != "rfid" && metode != "qr" {
		metode = "rfid"
	}
	now := time.Now().In(a.cfg.Location)
	_, _ = a.db.ExecContext(ctx,
		`INSERT INTO tb_absensi_log (waktu, kode, metode, kiosk, id_siswa, hasil, pesan) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		now, kode, metode, kiosk, idSiswa, hasil, pesan)
}

func (a *App) handleKioskRecent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now().In(a.cfg.Location)
	today := now.Format("2006-01-02")
	settings := loadSettings(ctx, a.db)

	type summary struct {
		TotalSiswaAktif int `json:"total_siswa_aktif"`
		Hadir           int `json:"hadir"`
		Terlambat       int `json:"terlambat"`
		Pulang          int `json:"pulang"`
		IzinSakitAlpha  int `json:"izin_sakit_alpha"`
	}
	var sum summary
	_ = a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+a.schema.StudentsTable+` WHERE status = 'aktif'`).Scan(&sum.TotalSiswaAktif)
	_ = a.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN status = 'hadir' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'terlambat' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN jam_pulang IS NOT NULL THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status IN ('izin','sakit','alpha') THEN 1 ELSE 0 END), 0)
		FROM tb_absensi_harian WHERE tanggal = ?`, today).
		Scan(&sum.Hadir, &sum.Terlambat, &sum.Pulang, &sum.IzinSakitAlpha)

	type recentRow struct {
		IDSiswa      int    `json:"id_siswa"`
		NIS          string `json:"nis"`
		Nama         string `json:"nama"`
		NamaKelas    string `json:"nama_kelas"`
		Status       string `json:"status"`
		JamMasuk     string `json:"jam_masuk"`
		JamPulang    string `json:"jam_pulang"`
		MetodeMasuk  string `json:"metode_masuk"`
		MetodePulang string `json:"metode_pulang"`
	}

	rows, err := a.db.QueryContext(ctx, `
		SELECT a.id_siswa, COALESCE(s.nis, ''), COALESCE(s.nama_lengkap, ''),
		       COALESCE(k.nama_kelas, '-'), a.status,
		       a.jam_masuk, a.jam_pulang,
		       COALESCE(a.metode_masuk, ''), COALESCE(a.metode_pulang, '')
		FROM tb_absensi_harian a
		INNER JOIN `+a.schema.StudentsTable+` s ON s.id_siswa = a.id_siswa
		LEFT JOIN `+a.schema.ClassesTable+` k ON CAST(k.id_kelas AS CHAR) = TRIM(s.id_kelas)
		WHERE a.tanggal = ?
		ORDER BY COALESCE(a.jam_pulang, a.jam_masuk, a.updated_at) DESC
		LIMIT 12`, today)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Gagal memuat daftar terbaru"})
		return
	}
	defer rows.Close()

	recent := make([]recentRow, 0, 12)
	for rows.Next() {
		var r recentRow
		var jm, jp sql.NullTime
		if err := rows.Scan(&r.IDSiswa, &r.NIS, &r.Nama, &r.NamaKelas, &r.Status, &jm, &jp, &r.MetodeMasuk, &r.MetodePulang); err != nil {
			continue
		}
		if jm.Valid {
			r.JamMasuk = jm.Time.In(a.cfg.Location).Format("15:04:05")
		}
		if jp.Valid {
			r.JamPulang = jp.Time.In(a.cfg.Location).Format("15:04:05")
		}
		recent = append(recent, r)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"server_time": now.Format(time.RFC3339),
		"tanggal":     today,
		"settings": map[string]any{
			"nama_sekolah":     settings.NamaSekolah,
			"jam_masuk_batas":  settings.JamMasukBatas,
			"jam_pulang_mulai": settings.JamPulangMulai,
		},
		"summary": sum,
		"recent":  recent,
	})
}
