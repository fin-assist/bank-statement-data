package postgres

import "context"

// ReferencedBatch uses the primary and the existing (user_id, object_key) index.
// Keys have already passed the usecase's canonical namespace/grace checks. The
// caller must still lock and recheck each unreferenced candidate before erasure.
// Retired users remain candidates even when a partial purge still retains upload rows.
func (r *Imports) ReferencedBatch(ctx context.Context, keys []string) (map[string]bool, error) {
	result := make(map[string]bool)
	if len(keys) == 0 {
		return result, nil
	}
	rows, err := r.db.Writer(ctx).Query(ctx, `SELECT u.object_key FROM unnest($1::text[]) AS k(object_key)
 JOIN upload u ON u.user_id=split_part(k.object_key,'/',1)::uuid AND u.object_key=k.object_key
 WHERE u.status IN('staging','processing','done')
 AND NOT EXISTS(SELECT 1 FROM tombstone_users t WHERE t.user_id=u.user_id)`, keys)
	if err != nil {
		return nil, importError(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, importError(ctx, err)
		}
		result[key] = true
	}
	if err := rows.Err(); err != nil {
		return nil, importError(ctx, err)
	}
	return result, nil
}
