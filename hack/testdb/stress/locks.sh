#!/bin/sh
# stress-locks: open N long-lived transactions that hold row-level write
# locks on the same set of users, producing blocking chains when the normal
# workload tries to UPDATE the same rows. Simulates a badly-written
# application that forgets to commit.
#
# Metrics to watch:
#   pgwatch_backends_longest_tx_seconds                           — grows to holder_secs
#   pgwatch_backends_longest_query_seconds                        — grows while waiters pile up
#   pgwatch_backends_idleintransaction                            — equals holders count
#   pgwatch_locks_count{locktype="transactionid"}                 — grows with waiters
#   pgwatch_locks_mode_count{lockmode="RowExclusiveLock"}         — same
#   pgwatch_db_stats_deadlocks                                    — should NOT grow (no cycles)
#
# On shutdown, all holder backends exit and locks release.

set -eu

PGPASSWORD="${POSTGRES_PASSWORD:-postgres}"
export PGPASSWORD
HOST="${TARGET_HOST:-postgres-primary}"
DB="${TARGET_DB:-app_primary}"
USER="${TARGET_USER:-postgres}"
HOLDERS="${HOLDERS:-5}"
HOLDER_SECS="${HOLDER_SECS:-600}"
TARGET_IDS="${TARGET_IDS:-1 2 3 4 5 6 7 8 9 10}"

echo "[stress-locks] starting ${HOLDERS} lock holders × ${HOLDER_SECS}s on users(${TARGET_IDS})"

holders=""
for i in $(seq 1 "$HOLDERS"); do
    (
        # \! sleep keeps the backend idle-in-transaction (see idle-tx.sh).
        command psql -h "$HOST" -U "$USER" -d "$DB" -v ON_ERROR_STOP=0 <<SQL
BEGIN;
SELECT id, name FROM users WHERE id IN (${TARGET_IDS// /,}) FOR UPDATE;
\! sleep ${HOLDER_SECS}
ROLLBACK;
SQL
        echo "[stress-locks] holder #${i} finished"
    ) >/dev/null 2>&1 &
    holders="${holders} $!"
done

cleanup() {
    echo "[stress-locks] killing holders:${holders}"
    for pid in $holders; do kill "$pid" 2>/dev/null || true; done
    exit 0
}
trap cleanup INT TERM

echo "[stress-locks] holders running; waiting for them"
# shellcheck disable=SC2086
wait $holders
echo "[stress-locks] all holders done"
