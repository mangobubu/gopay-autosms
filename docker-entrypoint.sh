#!/usr/bin/env bash
set -Eeuo pipefail

data_dir="${AUTOSMS_DATA_DIR:-/data}"
runtime_dir="${data_dir}/runtime"
db_env_file="${runtime_dir}/db.env"
postgres_data_dir="${PGDATA:-${data_dir}/postgres}"
db_host="127.0.0.1"
db_port="${AUTOSMS_DB_PORT:-5432}"
public_url="${AUTOSMS_PUBLIC_URL:-http://localhost:8080}"

if [[ "$(id -u)" -ne 0 ]]; then
  echo "AutoSMS container entrypoint must start as root so PostgreSQL can drop privileges." >&2
  exit 1
fi

if [[ ! "${db_port}" =~ ^[0-9]+$ ]] || (( ${#db_port} > 5 )); then
  echo "AUTOSMS_DB_PORT must be an integer between 1 and 65535." >&2
  exit 1
fi
db_port="$((10#${db_port}))"
if (( db_port < 1 || db_port > 65535 )); then
  echo "AUTOSMS_DB_PORT must be an integer between 1 and 65535." >&2
  exit 1
fi

for value_name in AUTOSMS_DB_USER AUTOSMS_DB_NAME AUTOSMS_DB_PASSWORD AUTOSMS_SECRET_KEY; do
  value="${!value_name:-}"
  if [[ "${value}" == *$'\n'* || "${value}" == *$'\r'* ]]; then
    echo "${value_name} must not contain a newline." >&2
    exit 1
  fi
done

# Capture explicit overrides before loading the values persisted in /data.
requested_db_user="${AUTOSMS_DB_USER:-${POSTGRES_USER:-}}"
requested_db_name="${AUTOSMS_DB_NAME:-${POSTGRES_DB:-}}"
requested_db_password="${AUTOSMS_DB_PASSWORD:-${POSTGRES_PASSWORD:-}}"
requested_secret_key="${AUTOSMS_SECRET_KEY:-}"
requested_database_url="${DATABASE_URL:-}"

install -d -m 0700 "${runtime_dir}" "${postgres_data_dir}"

persisted_db_user=""
persisted_db_name=""
persisted_db_password=""
persisted_secret_key=""
persisted_db_admin_user=""
if [[ -s "${db_env_file}" ]]; then
  while IFS='=' read -r credential_name encoded_value; do
    [[ -z "${credential_name}" ]] && continue
    if ! decoded_value="$(printf '%s' "${encoded_value}" | base64 --decode)"; then
      echo "Invalid persisted credential data in ${db_env_file}." >&2
      exit 1
    fi
    case "${credential_name}" in
      AUTOSMS_DB_USER_B64) persisted_db_user="${decoded_value}" ;;
      AUTOSMS_DB_NAME_B64) persisted_db_name="${decoded_value}" ;;
      AUTOSMS_DB_PASSWORD_B64) persisted_db_password="${decoded_value}" ;;
      AUTOSMS_SECRET_KEY_B64) persisted_secret_key="${decoded_value}" ;;
      AUTOSMS_DB_ADMIN_USER_B64) persisted_db_admin_user="${decoded_value}" ;;
      *)
        echo "Unknown persisted credential field ${credential_name}." >&2
        exit 1
        ;;
    esac
  done < "${db_env_file}"
  persisted_db_admin_user="${persisted_db_admin_user:-${persisted_db_user}}"
fi

if [[ -n "${persisted_secret_key}" &&
      -n "${requested_secret_key}" &&
      "${requested_secret_key}" != "${persisted_secret_key}" ]]; then
  echo "Startup aborted: AUTOSMS_SECRET_KEY does not match the key persisted in ${db_env_file}; the persisted encryption key was left unchanged." >&2
  exit 1
fi

AUTOSMS_DB_USER="${requested_db_user:-${persisted_db_user:-autosms}}"
AUTOSMS_DB_NAME="${requested_db_name:-${persisted_db_name:-autosms}}"
AUTOSMS_DB_PASSWORD="${requested_db_password:-${persisted_db_password:-}}"
AUTOSMS_SECRET_KEY="${requested_secret_key:-${persisted_secret_key:-}}"
AUTOSMS_DB_ADMIN_USER="${persisted_db_admin_user:-${AUTOSMS_DB_USER}}"

if [[ -z "${AUTOSMS_DB_USER}" || -z "${AUTOSMS_DB_NAME}" ]]; then
  echo "Database user and database name must not be empty." >&2
  exit 1
fi
for value_name in AUTOSMS_DB_USER AUTOSMS_DB_NAME AUTOSMS_DB_PASSWORD AUTOSMS_SECRET_KEY AUTOSMS_DB_ADMIN_USER; do
  value="${!value_name:-}"
  if [[ "${value}" == *$'\n'* || "${value}" == *$'\r'* ]]; then
    echo "${value_name} must not contain a newline." >&2
    exit 1
  fi
done
if [[ -z "${AUTOSMS_DB_PASSWORD}" ]]; then
  AUTOSMS_DB_PASSWORD="$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')"
fi
if [[ -z "${AUTOSMS_SECRET_KEY}" ]]; then
  AUTOSMS_SECRET_KEY="$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')"
fi

# Persist generated or explicitly overridden credentials atomically. Reusing
# the /data volume therefore reuses the exact same database and encryption key.
credentials_tmp="${db_env_file}.tmp.$$"
umask 077
{
  printf 'AUTOSMS_DB_USER_B64=%s\n' "$(printf '%s' "${AUTOSMS_DB_USER}" | base64 | tr -d '\n')"
  printf 'AUTOSMS_DB_NAME_B64=%s\n' "$(printf '%s' "${AUTOSMS_DB_NAME}" | base64 | tr -d '\n')"
  printf 'AUTOSMS_DB_PASSWORD_B64=%s\n' "$(printf '%s' "${AUTOSMS_DB_PASSWORD}" | base64 | tr -d '\n')"
  printf 'AUTOSMS_SECRET_KEY_B64=%s\n' "$(printf '%s' "${AUTOSMS_SECRET_KEY}" | base64 | tr -d '\n')"
  printf 'AUTOSMS_DB_ADMIN_USER_B64=%s\n' "$(printf '%s' "${AUTOSMS_DB_ADMIN_USER}" | base64 | tr -d '\n')"
} > "${credentials_tmp}"
chmod 0600 "${credentials_tmp}"
mv -f "${credentials_tmp}" "${db_env_file}"

dsn_quote() {
  local escaped="$1"
  escaped="${escaped//\\/\\\\}"
  escaped="${escaped//\'/\\\'}"
  printf "'%s'" "${escaped}"
}

generated_database_url="host=$(dsn_quote "${db_host}") port=${db_port} dbname=$(dsn_quote "${AUTOSMS_DB_NAME}") user=$(dsn_quote "${AUTOSMS_DB_USER}") password=$(dsn_quote "${AUTOSMS_DB_PASSWORD}") sslmode=disable"

export AUTOSMS_DB_USER AUTOSMS_DB_NAME AUTOSMS_DB_PASSWORD AUTOSMS_SECRET_KEY
export POSTGRES_USER="${AUTOSMS_DB_USER}"
export POSTGRES_DB="${AUTOSMS_DB_NAME}"
export POSTGRES_PASSWORD="${AUTOSMS_DB_PASSWORD}"
export PGDATA="${postgres_data_dir}"
export DATABASE_URL="${requested_database_url:-${generated_database_url}}"

shutdown() {
  local exit_code="${1:-0}"
  trap - TERM INT EXIT
  if [[ -n "${app_pid:-}" ]] && kill -0 "${app_pid}" 2>/dev/null; then
    kill -TERM "${app_pid}" 2>/dev/null || true
  fi
  if [[ -n "${pg_pid:-}" ]] && kill -0 "${pg_pid}" 2>/dev/null; then
    kill -TERM "${pg_pid}" 2>/dev/null || true
  fi
  if [[ -n "${app_pid:-}" ]]; then
    wait "${app_pid}" 2>/dev/null || true
  fi
  if [[ -n "${pg_pid:-}" ]]; then
    wait "${pg_pid}" 2>/dev/null || true
  fi
  exit "${exit_code}"
}
trap 'shutdown 143' TERM
trap 'shutdown 130' INT
trap 'shutdown $?' EXIT

/usr/local/bin/docker-entrypoint.sh postgres \
  -c "listen_addresses=${db_host}" \
  -p "${db_port}" &
pg_pid=$!

for ((attempt = 1; attempt <= 60; attempt++)); do
  if pg_isready -h "${db_host}" -p "${db_port}" -U "${AUTOSMS_DB_ADMIN_USER}" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "${pg_pid}" 2>/dev/null; then
    echo "PostgreSQL exited before becoming ready" >&2
    wait "${pg_pid}" || exit $?
  fi
  sleep 1
done

if ! pg_isready -h "${db_host}" -p "${db_port}" -U "${AUTOSMS_DB_ADMIN_USER}" >/dev/null 2>&1; then
  echo "PostgreSQL did not become ready within 60 seconds" >&2
  exit 1
fi

# Environment overrides also work with an existing volume. The local socket is
# trust-authenticated by the official image, so the requested role/database can
# be created or updated before the Go service connects.
psql_base=(gosu postgres psql --host=/var/run/postgresql --port="${db_port}" --username="${AUTOSMS_DB_ADMIN_USER}" --dbname=postgres --no-psqlrc --set=ON_ERROR_STOP=1)
role_exists="$(printf '%s\n' "SELECT 1 FROM pg_roles WHERE rolname = :'db_user';" | "${psql_base[@]}" --tuples-only --no-align --set=db_user="${AUTOSMS_DB_USER}")"
if [[ "${role_exists}" != "1" ]]; then
  printf '%s\n' "SELECT format('CREATE ROLE %I LOGIN', :'db_user') \\gexec" | "${psql_base[@]}" --set=db_user="${AUTOSMS_DB_USER}"
fi
printf '%s\n' "SELECT format('ALTER ROLE %I WITH LOGIN PASSWORD %L', :'db_user', :'db_password') \\gexec" |
  "${psql_base[@]}" --set=db_user="${AUTOSMS_DB_USER}" --set=db_password="${AUTOSMS_DB_PASSWORD}"

database_exists="$(printf '%s\n' "SELECT 1 FROM pg_database WHERE datname = :'db_name';" | "${psql_base[@]}" --tuples-only --no-align --set=db_name="${AUTOSMS_DB_NAME}")"
if [[ "${database_exists}" != "1" ]]; then
  gosu postgres createdb \
    --host=/var/run/postgresql \
    --port="${db_port}" \
    --username="${AUTOSMS_DB_ADMIN_USER}" \
    --owner="${AUTOSMS_DB_USER}" \
    -- "${AUTOSMS_DB_NAME}"
else
  printf '%s\n' "SELECT format('ALTER DATABASE %I OWNER TO %I', :'db_name', :'db_user') \\gexec" |
    "${psql_base[@]}" --set=db_name="${AUTOSMS_DB_NAME}" --set=db_user="${AUTOSMS_DB_USER}"
fi

echo "============================================================"
echo "AutoSMS is starting with embedded PostgreSQL 16"
echo "DB host:     ${db_host}"
echo "DB port:     ${db_port} (container internal only)"
echo "DB name:     ${AUTOSMS_DB_NAME}"
echo "DB user:     ${AUTOSMS_DB_USER}"
echo "Web address: ${public_url}"
echo "Credentials file: ${db_env_file}"
echo "============================================================"

gosu postgres /app/autosms &
app_pid=$!

set +e
wait -n "${pg_pid}" "${app_pid}"
exit_code=$?
set -e
exit "${exit_code}"
