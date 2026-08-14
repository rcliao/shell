#!/usr/bin/env bash
# provenance-census.sh — per-path provenance coverage and canonical
# convergence for one agent DB (ghost source-identity-design.md eval hook).
#
# Usage: provenance-census.sh <db-path> <agent-ns> [since-iso8601]
# Example:
#   scripts/provenance-census.sh ~/.shell/agents/pikamini/memory.db agent:pikamini 2026-08-13
#
# Reads only. Reports, for memories created since the cutoff:
#   1. per-path coverage: how many writes carry source_kind, by key prefix
#   2. source_user distribution: which ids new writes actually record
#   3. canonical convergence: rows whose source_user is a canonical id vs a
#      declared alias (repairable) vs an undeclared variant (drift)
set -euo pipefail

DB="${1:?db path required}"
NS="${2:?agent ns required}"
SINCE="${3:-$(date -u -v-7d +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '7 days ago' +%Y-%m-%dT%H:%M:%SZ)}"

Q() { sqlite3 -readonly "$DB" "$1"; }

echo "== provenance census: $NS since $SINCE =="

echo "-- per-path coverage (path | total | with_provenance) --"
Q "SELECT CASE
     WHEN key LIKE 'exchange-%'   THEN 'exchange'
     WHEN key LIKE 'hygiene-%'    THEN 'hygiene'
     WHEN key LIKE 'media-note-%' THEN 'media-note'
     WHEN key LIKE 'learning-%'   THEN 'learning'
     WHEN key LIKE 'session-%'    THEN 'summary'
     WHEN key LIKE 'heartbeat%'   THEN 'heartbeat'
     ELSE 'other' END,
   COUNT(*),
   SUM(CASE WHEN COALESCE(source_kind,'') != '' THEN 1 ELSE 0 END)
   FROM memories
   WHERE ns='$NS' AND created_at >= '$SINCE' AND deleted_at IS NULL
   GROUP BY 1 ORDER BY 2 DESC;"

echo "-- source_user distribution (user | kind | count) --"
Q "SELECT COALESCE(NULLIF(source_user,''),'(none)'),
          COALESCE(NULLIF(source_kind,''),'(empty)'), COUNT(*)
   FROM memories
   WHERE ns='$NS' AND created_at >= '$SINCE' AND deleted_at IS NULL
   GROUP BY 1,2 ORDER BY 3 DESC;"

echo "-- canonical convergence (class | count) --"
Q "SELECT CASE
     WHEN m.source_user IS NULL OR m.source_user = '' THEN 'no-person'
     WHEN EXISTS (SELECT 1 FROM source_aliases a
                  WHERE a.ns = m.ns AND a.canonical = m.source_user COLLATE NOCASE)
          THEN 'canonical'
     WHEN EXISTS (SELECT 1 FROM source_aliases a
                  WHERE a.ns = m.ns AND a.alias = m.source_user)
          THEN 'declared-alias (repairable)'
     ELSE 'undeclared variant (DRIFT)' END,
   COUNT(*)
   FROM memories m
   WHERE m.ns='$NS' AND m.created_at >= '$SINCE' AND m.deleted_at IS NULL
   GROUP BY 1 ORDER BY 2 DESC;"

echo "-- undeclared variants, if any --"
Q "SELECT m.source_user, COUNT(*), MIN(m.key)
   FROM memories m
   WHERE m.ns='$NS' AND m.created_at >= '$SINCE' AND m.deleted_at IS NULL
     AND COALESCE(m.source_user,'') != ''
     AND NOT EXISTS (SELECT 1 FROM source_aliases a
                     WHERE a.ns = m.ns AND (a.alias = m.source_user
                        OR a.canonical = m.source_user COLLATE NOCASE))
   GROUP BY 1 ORDER BY 2 DESC LIMIT 10;"
