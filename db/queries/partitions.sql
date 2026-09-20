-- name: EnsureRecordPartitions :exec
SELECT ensure_record_partitions(sqlc.arg(from_ts), sqlc.arg(months)::integer);
