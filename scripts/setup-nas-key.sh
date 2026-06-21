#!/usr/bin/env bash
#
# setup-nas-key.sh — generate a dedicated SSH key and install it on the backup NAS
# so unattended (systemd-timer) restic backups can authenticate without a password.
#
# Safe to re-run: reuses an existing key and won't duplicate the authorized_keys
# entry or the ~/.ssh/config block.
#
set -euo pipefail

NAS_USER="${NAS_USER:-btb}"
NAS_HOST="${NAS_HOST:-10.0.0.132}"
SSH_ALIAS="${SSH_ALIAS:-nas-backup}"
KEY_PATH="${KEY_PATH:-$HOME/.ssh/nas_backup}"
KEY_COMMENT="backd-nas-backup@$(hostname)"

umask 077
mkdir -p "$(dirname "$KEY_PATH")"

# 1. Generate the key (ed25519, no passphrase so a timer can use it unattended).
if [[ -f "$KEY_PATH" ]]; then
  echo "==> Reusing existing key: $KEY_PATH"
else
  echo "==> Generating new ed25519 key: $KEY_PATH"
  ssh-keygen -t ed25519 -a 100 -N "" -C "$KEY_COMMENT" -f "$KEY_PATH"
fi

PUB_B64="$(base64 -w0 < "${KEY_PATH}.pub")"

# 2. Install the public key on the NAS.
#    The 'btb' user has no home directory by default, so we locate (and, if needed,
#    create) a usable home and write authorized_keys there. base64 sidesteps all the
#    quoting pain of shipping a key over the ssh command line.
echo "==> Installing public key on ${NAS_USER}@${NAS_HOST} (enter the NAS password when prompted)"
install_out="$(ssh "${NAS_USER}@${NAS_HOST}" "sh -s ${PUB_B64}" <<'EOSSH'
set -eu
PUB="$(printf '%s' "$1" | base64 -d)"

# Find the account's home from passwd; fall back to $HOME.
H="$(getent passwd "$(id -un)" 2>/dev/null | cut -d: -f6 || true)"
[ -n "${H:-}" ] || H="${HOME:-}"

if [ -z "${H:-}" ]; then
  echo "RESULT=no-home"; exit 0
fi
if ! mkdir -p "$H/.ssh" 2>/dev/null; then
  echo "RESULT=home-unwritable home=$H"; exit 0
fi
chmod 700 "$H/.ssh" 2>/dev/null || true
touch "$H/.ssh/authorized_keys"
chmod 600 "$H/.ssh/authorized_keys" 2>/dev/null || true

if grep -qF "$PUB" "$H/.ssh/authorized_keys" 2>/dev/null; then
  echo "RESULT=already-present home=$H"
else
  printf '%s\n' "$PUB" >> "$H/.ssh/authorized_keys"
  echo "RESULT=installed home=$H"
fi
EOSSH
)"
echo "    NAS says: ${install_out}"

# 3. Add a ~/.ssh/config alias so plain ssh + restic auto-select this key for the NAS.
CONFIG="$HOME/.ssh/config"
if grep -qE "^Host[[:space:]]+${SSH_ALIAS}([[:space:]]|$)" "$CONFIG" 2>/dev/null; then
  echo "==> ~/.ssh/config already has a '${SSH_ALIAS}' host block — leaving it alone"
else
  echo "==> Adding '${SSH_ALIAS}' host block to ~/.ssh/config"
  {
    printf '\nHost %s\n' "$SSH_ALIAS"
    printf '    HostName %s\n' "$NAS_HOST"
    printf '    User %s\n' "$NAS_USER"
    printf '    IdentityFile %s\n' "$KEY_PATH"
    printf '    IdentitiesOnly yes\n'
  } >> "$CONFIG"
  chmod 600 "$CONFIG"
fi

# 4. Verify key-only auth (BatchMode => never falls back to a password prompt).
echo "==> Testing key-based login (no password should be requested)"
if ssh -o BatchMode=yes -o ConnectTimeout=10 "$SSH_ALIAS" true 2>/dev/null; then
  echo "✅ SUCCESS: 'ssh ${SSH_ALIAS}' works with the key alone."
  echo "   Use this as the restic repo target: sftp:${SSH_ALIAS}:<path-on-nas>"
else
  cat <<EOF
❌ Key auth did NOT work yet.

If the NAS reported RESULT=no-home or RESULT=home-unwritable above, the '${NAS_USER}'
account has no writable home, so sshd can't find authorized_keys. Fix it on the NAS
(e.g. OpenMediaVault) by either:
  - giving '${NAS_USER}' a home directory, or
  - setting 'AuthorizedKeysFile' in sshd_config to a path you can write, then placing
    the contents of ${KEY_PATH}.pub there.
Then re-run this script.
EOF
  exit 1
fi
