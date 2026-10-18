#!/usr/bin/env bash
# Tambah satu akun GitHub Copilot ke pool copilotd.
#   scripts/tambah-akun.sh [--peran utama|cadangan] [--label "teks"]
#
# Token yang dipakai: PAT berbutir (github_pat_…) milik akun pribadi dengan izin akun "Copilot Requests", token
# OAuth (gho_…) dari `gh auth login`, atau token pengguna GitHub App (ghu_…). PAT klasik (ghp_…) ditolak Copilot CLI.
#
# Token diminta tanpa tampil di layar, tidak masuk riwayat shell maupun argumen proses, lalu diperiksa (login + sisa
# kuota) sebelum disimpan. Bila copilotd jalan, akun langsung ikut pembagian lewat API-nya (tanpa restart); bila
# tidak, akun ditulis ke akun.json (cadangan versi sebelumnya di folder cadangan/). Akun juga bisa ditambah dari
# admin revisi.studio, halaman Akun Copilot.
set -euo pipefail
cd "$(dirname "$0")/.."

read -rsp "Token GitHub akun baru (github_pat_… / gho_…): " AKUN_TOKEN_BARU
echo
if [ -z "${AKUN_TOKEN_BARU}" ]; then
  echo "Token kosong, dibatalkan." >&2
  exit 1
fi
export AKUN_TOKEN_BARU
trap 'unset AKUN_TOKEN_BARU' EXIT
go run ./cmd/akun tambah --env .env "$@"
