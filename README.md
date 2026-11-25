# Absensi QR & RFID (Go — Kompatibel Database Revamp & Legacy)

Aplikasi absensi harian siswa berbasis Go yang **otomatis mendeteksi skema database** — dapat berjalan baik pada **database Revamp Go (`unbk_students`, `unbk_classes`, `unbk_admins`, `unbk_teachers`, `unbk_credentials`)** maupun **database Legacy PHP (`tb_siswa`, `tb_kelas`, `tb_admin`, `tb_pengajar`)**, serta 100% kompatibel dengan **MariaDB 5.5.68 (CentOS 7)** hingga MariaDB 10+ / MySQL 8+.

## Tabel Database
Aplikasi ini **tidak mengubah tabel legacy maupun tabel revamp**. Saat pertama kali dijalankan, aplikasi otomatis membuat 4 tabel tambahan:
- `tb_absensi_kartu`: pemetaan `id_siswa` dengan `rfid_uid` dan `qr_token` statis.
- `tb_absensi_harian`: catatan absen masuk, pulang, status (`hadir`, `terlambat`, `izin`, `sakit`, `alpha`), metode, dan keterangan.
- `tb_absensi_pengaturan`: nama sekolah, batas jam masuk, jam mulai pulang, cooldown double-tap, API key IoT.
- `tb_absensi_log`: log setiap tap RFID / scan QR (termasuk kartu yang belum terdaftar agar mudah dipasangkan dari panel admin).

## Cara Menjalankan
1. Pastikan MySQL Laragon berjalan dan database `db_ujian55` tersedia.
2. (Opsional) Salin `.env.example` menjadi `.env` jika port atau kredensial MySQL berbeda dari `+koneksi.php`.
3. Jalankan binary atau build dari source:
   ```powershell
   cd c:\laragon\www\unbk55\absensi
   go build -o absensi55.exe .
   .\absensi55.exe
   ```
4. Akses melalui browser:
   - **Kiosk Absensi (RFID & QR)**: `http://localhost:8090/`
   - **Panel Admin / Guru**: `http://localhost:8090/admin` (login menggunakan akun `tb_admin` atau `tb_pengajar` legacy)
   - **Cetak Kartu QR per Kelas**: `http://localhost:8090/print-qr`

## Integrasi Perangkat RFID / QR
- **RFID Reader USB (HID Keyboard)** & **USB QR/Barcode Scanner**: langsung arahkan browser ke halaman Kiosk (`/`). Input selalu siap menerima tap + Enter.
- **Kamera Webcam / HP**: klik tombol **Aktifkan Kamera QR** di halaman Kiosk (mendukung Chrome/Edge via `BarcodeDetector` offline).
- **Perangkat IoT / ESP32**: kirim HTTP `POST /api/iot/scan` (sertakan header `X-API-Key` jika diisi di Pengaturan):
  ```json
  {
    "kode": "04A2B1C3",
    "metode": "rfid",
    "mode": "auto",
    "kiosk": "gerbang-utama"
  }
  ```
