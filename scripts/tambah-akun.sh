#!/usr/bin/env bash
# Tambah satu akun GitHub Copilot ke pool copilotd (TURNITIN_COPILOT_GITHUB_TOKENS di .env).
#
# Token yang dipakai: PAT berbutir (github_pat_…) milik akun pribadi dengan izin akun "Copilot Requests", atau token
# OAuth (gho_…) dari `gh auth login`. PAT klasik (ghp_…) ditolak Copilot CLI.
#
# Token diminta tanpa tampil di layar, tidak masuk riwayat shell maupun argumen proses, lalu diperiksa (login + sisa
# kuota) sebelum ditulis. Cadangan .env lama disimpan di ~/backups/copilotd-env/.
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
