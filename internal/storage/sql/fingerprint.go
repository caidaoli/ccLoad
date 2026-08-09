package sql

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"ccLoad/internal/model"
)

// ListModelFingerprints 查询全部指纹基线，按 created_at DESC 排序。
func (s *SQLStore) ListModelFingerprints(ctx context.Context) ([]*model.ModelFingerprint, error) {
	rows, err := s.QueryContext(ctx, `
		SELECT id, name, channel_id, channel_name, model, actual_model, client_protocol,
		       sample_count, distribution, stats, raw_data, prompt_version, created_at, updated_at
		FROM model_fingerprints
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("query model_fingerprints: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var fps []*model.ModelFingerprint
	for rows.Next() {
		fp, err := scanFingerprint(rows)
		if err != nil {
			return nil, fmt.Errorf("scan model_fingerprints row: %w", err)
		}
		fps = append(fps, fp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate model_fingerprints: %w", err)
	}
	return fps, nil
}

// GetModelFingerprint 按 ID 查询指纹基线。
func (s *SQLStore) GetModelFingerprint(ctx context.Context, id int64) (*model.ModelFingerprint, error) {
	row := s.QueryRowContext(ctx, `
		SELECT id, name, channel_id, channel_name, model, actual_model, client_protocol,
		       sample_count, distribution, stats, raw_data, prompt_version, created_at, updated_at
		FROM model_fingerprints
		WHERE id = ?
	`, id)

	fp, err := scanFingerprintRow(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("model fingerprint %d not found", id)
		}
		return nil, fmt.Errorf("query model_fingerprints by id: %w", err)
	}
	return fp, nil
}

// ModelFingerprintNameExists 判断基准名称是否已被占用。
func (s *SQLStore) ModelFingerprintNameExists(ctx context.Context, name string) (bool, error) {
	var count int
	if err := s.QueryRowContext(ctx, `SELECT COUNT(*) FROM model_fingerprints WHERE name = ?`, name).Scan(&count); err != nil {
		return false, fmt.Errorf("query model_fingerprints by name: %w", err)
	}
	return count > 0, nil
}

// CreateModelFingerprint 插入新指纹基线，返回含 ID 的完整记录。
func (s *SQLStore) CreateModelFingerprint(ctx context.Context, fp *model.ModelFingerprint) (*model.ModelFingerprint, error) {
	exists, err := s.ModelFingerprintNameExists(ctx, fp.Name)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, fmt.Errorf("model fingerprint name %q already exists", fp.Name)
	}

	createdAt := time.Now()
	if !fp.CreatedAt.IsZero() {
		createdAt = fp.CreatedAt.Time
	}
	updatedAt := createdAt
	if !fp.UpdatedAt.IsZero() {
		updatedAt = fp.UpdatedAt.Time
	}
	createdAtUnix := timeToUnix(createdAt)
	updatedAtUnix := timeToUnix(updatedAt)

	distJSON, err := marshalJSON("distribution", fp.Distribution)
	if err != nil {
		return nil, err
	}
	statsJSON, err := marshalJSON("stats", fp.Stats)
	if err != nil {
		return nil, err
	}
	rawJSON, err := marshalJSON("raw_data", fp.RawData)
	if err != nil {
		return nil, err
	}

	promptVer := fp.PromptVersion
	if promptVer == "" {
		promptVer = "v1"
	}

	var channelID sql.NullInt64
	if fp.ChannelID != nil {
		channelID = sql.NullInt64{Int64: *fp.ChannelID, Valid: true}
	}

	if fp.ID != 0 {
		err := s.WithTransaction(ctx, func(tx *sql.Tx) error {
			if err := s.lockPostgresExplicitIDTable(ctx, tx, "model_fingerprints"); err != nil {
				return err
			}
			if _, err := s.execTx(ctx, tx, `
				INSERT INTO model_fingerprints
					(id, name, channel_id, channel_name, model, actual_model, client_protocol,
					 sample_count, distribution, stats, raw_data, prompt_version, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			`, fp.ID, fp.Name, channelID, fp.ChannelName, fp.Model, fp.ActualModel, fp.ClientProtocol,
				fp.SampleCount, distJSON, statsJSON, rawJSON, promptVer, createdAtUnix, updatedAtUnix); err != nil {
				return err
			}
			return s.syncPostgresIDSequence(ctx, tx, "model_fingerprints")
		})
		if err != nil {
			return nil, fmt.Errorf("insert model_fingerprints with id=%d: %w", fp.ID, s.modelFingerprintInsertError(ctx, fp.Name, err))
		}
		return s.GetModelFingerprint(ctx, fp.ID)
	}

	if s.IsPostgres() {
		var newID int64
		err := s.QueryRowContext(ctx, `
			INSERT INTO model_fingerprints
				(name, channel_id, channel_name, model, actual_model, client_protocol,
				 sample_count, distribution, stats, raw_data, prompt_version, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			RETURNING id
		`, fp.Name, channelID, fp.ChannelName, fp.Model, fp.ActualModel, fp.ClientProtocol,
			fp.SampleCount, distJSON, statsJSON, rawJSON, promptVer, createdAtUnix, updatedAtUnix).Scan(&newID)
		if err != nil {
			return nil, fmt.Errorf("insert model_fingerprints: %w", s.modelFingerprintInsertError(ctx, fp.Name, err))
		}
		return s.GetModelFingerprint(ctx, newID)
	}

	res, err := s.ExecContext(ctx, `
		INSERT INTO model_fingerprints
			(name, channel_id, channel_name, model, actual_model, client_protocol,
			 sample_count, distribution, stats, raw_data, prompt_version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, fp.Name, channelID, fp.ChannelName, fp.Model, fp.ActualModel, fp.ClientProtocol,
		fp.SampleCount, distJSON, statsJSON, rawJSON, promptVer, createdAtUnix, updatedAtUnix)
	if err != nil {
		return nil, fmt.Errorf("insert model_fingerprints: %w", s.modelFingerprintInsertError(ctx, fp.Name, err))
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("get last insert id for model_fingerprints: %w", err)
	}
	return s.GetModelFingerprint(ctx, newID)
}

// UpsertModelFingerprintReplica writes the SQLite-assigned final state to a
// primary replica. Replaying it after an ambiguous commit is safe.
func (s *SQLStore) UpsertModelFingerprintReplica(ctx context.Context, fp *model.ModelFingerprint) error {
	if fp == nil || fp.ID <= 0 {
		return fmt.Errorf("invalid model fingerprint replica")
	}
	distJSON, err := marshalJSON("distribution", fp.Distribution)
	if err != nil {
		return err
	}
	statsJSON, err := marshalJSON("stats", fp.Stats)
	if err != nil {
		return err
	}
	rawJSON, err := marshalJSON("raw_data", fp.RawData)
	if err != nil {
		return err
	}
	promptVersion := fp.PromptVersion
	if promptVersion == "" {
		promptVersion = "v1"
	}
	createdAt := fp.CreatedAt.Time
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	updatedAt := fp.UpdatedAt.Time
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}
	var channelID sql.NullInt64
	if fp.ChannelID != nil {
		channelID = sql.NullInt64{Int64: *fp.ChannelID, Valid: true}
	}

	args := []any{fp.ID, fp.Name, channelID, fp.ChannelName, fp.Model, fp.ActualModel, fp.ClientProtocol,
		fp.SampleCount, distJSON, statsJSON, rawJSON, promptVersion, timeToUnix(createdAt), timeToUnix(updatedAt)}
	query := `
		INSERT INTO model_fingerprints
			(id, name, channel_id, channel_name, model, actual_model, client_protocol,
			 sample_count, distribution, stats, raw_data, prompt_version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if s.supportsONConflict() {
		query += ` ON CONFLICT(id) DO UPDATE SET
			name = excluded.name, channel_id = excluded.channel_id, channel_name = excluded.channel_name,
			model = excluded.model, actual_model = excluded.actual_model, client_protocol = excluded.client_protocol,
			sample_count = excluded.sample_count, distribution = excluded.distribution, stats = excluded.stats,
			raw_data = excluded.raw_data, prompt_version = excluded.prompt_version, created_at = excluded.created_at,
			updated_at = excluded.updated_at`
	} else {
		query += ` ON DUPLICATE KEY UPDATE
			name = VALUES(name), channel_id = VALUES(channel_id), channel_name = VALUES(channel_name),
			model = VALUES(model), actual_model = VALUES(actual_model), client_protocol = VALUES(client_protocol),
			sample_count = VALUES(sample_count), distribution = VALUES(distribution), stats = VALUES(stats),
			raw_data = VALUES(raw_data), prompt_version = VALUES(prompt_version), created_at = VALUES(created_at),
			updated_at = VALUES(updated_at)`
	}
	return s.WithTransaction(ctx, func(tx *sql.Tx) error {
		if err := s.lockPostgresExplicitIDTable(ctx, tx, "model_fingerprints"); err != nil {
			return err
		}
		if _, err := s.execTx(ctx, tx, `DELETE FROM model_fingerprints WHERE name = ? AND id <> ?`, fp.Name, fp.ID); err != nil {
			return fmt.Errorf("delete stale model fingerprint replica: %w", err)
		}
		if _, err := s.execTx(ctx, tx, query, args...); err != nil {
			return fmt.Errorf("upsert model fingerprint replica: %w", err)
		}
		return s.syncPostgresIDSequence(ctx, tx, "model_fingerprints")
	})
}

func (s *SQLStore) modelFingerprintInsertError(ctx context.Context, name string, insertErr error) error {
	exists, err := s.ModelFingerprintNameExists(ctx, name)
	if err == nil && exists {
		return fmt.Errorf("model fingerprint name %q already exists", name)
	}
	return insertErr
}

// DeleteModelFingerprint 删除指定指纹基线。
func (s *SQLStore) DeleteModelFingerprint(ctx context.Context, id int64) error {
	if _, err := s.ExecContext(ctx, `DELETE FROM model_fingerprints WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete model_fingerprints id=%d: %w", id, err)
	}
	return nil
}

// ClearFingerprintChannelID 将属于指定渠道的所有指纹基线的 channel_id 置空。
// 在 DeleteConfig 事务内调用，保留基线数据，仅解除渠道关联。
func (s *SQLStore) ClearFingerprintChannelID(ctx context.Context, channelID int64) error {
	if _, err := s.ExecContext(ctx, `UPDATE model_fingerprints SET channel_id = NULL WHERE channel_id = ?`, channelID); err != nil {
		return fmt.Errorf("clear fingerprint channel_id for channel %d: %w", channelID, err)
	}
	return nil
}

// ==================== 扫描辅助 ====================

type fingerprintScanner interface {
	Scan(dest ...any) error
}

func scanFingerprintRow(row *sql.Row) (*model.ModelFingerprint, error) {
	return scanFingerprintImpl(row)
}

func scanFingerprint(rows *sql.Rows) (*model.ModelFingerprint, error) {
	return scanFingerprintImpl(rows)
}

func scanFingerprintImpl(s fingerprintScanner) (*model.ModelFingerprint, error) {
	var fp model.ModelFingerprint
	var channelID sql.NullInt64
	var distJSON, statsJSON, rawJSON string
	var createdAt, updatedAt int64

	if err := s.Scan(
		&fp.ID, &fp.Name, &channelID, &fp.ChannelName, &fp.Model, &fp.ActualModel, &fp.ClientProtocol,
		&fp.SampleCount, &distJSON, &statsJSON, &rawJSON, &fp.PromptVersion, &createdAt, &updatedAt,
	); err != nil {
		return nil, err
	}

	if channelID.Valid {
		v := channelID.Int64
		fp.ChannelID = &v
	}
	fp.CreatedAt = model.JSONTime{Time: unixToTime(createdAt)}
	fp.UpdatedAt = model.JSONTime{Time: unixToTime(updatedAt)}

	if err := json.Unmarshal([]byte(distJSON), &fp.Distribution); err != nil {
		return nil, fmt.Errorf("unmarshal distribution: %w", err)
	}
	if err := json.Unmarshal([]byte(statsJSON), &fp.Stats); err != nil {
		return nil, fmt.Errorf("unmarshal stats: %w", err)
	}
	if err := json.Unmarshal([]byte(rawJSON), &fp.RawData); err != nil {
		return nil, fmt.Errorf("unmarshal raw_data: %w", err)
	}

	return &fp, nil
}

// marshalJSON 将值序列化为 JSON 字符串；空 slice 序列化为 "[]"，nil 同理。
func marshalJSON(field string, v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal %s: %w", field, err)
	}
	return string(data), nil
}

// ==================== 对比历史 ====================

// CreateFingerprintTestResult 插入一条对比结果。
func (s *SQLStore) CreateFingerprintTestResult(ctx context.Context, rec *model.FingerprintTestRecord) error {
	distribution := rec.Distribution
	if distribution == nil {
		distribution = []float64{}
	}
	distributionJSON, err := marshalJSON("distribution", distribution)
	if err != nil {
		return err
	}
	createdAt := time.Now()
	if !rec.CreatedAt.IsZero() {
		createdAt = rec.CreatedAt.Time
	}
	createdAtUnix := timeToUnix(createdAt)
	var channelID sql.NullInt64
	if rec.ChannelID != nil {
		channelID = sql.NullInt64{Int64: *rec.ChannelID, Valid: true}
	}
	if rec.ID != 0 {
		err := s.WithTransaction(ctx, func(tx *sql.Tx) error {
			if err := s.lockPostgresExplicitIDTable(ctx, tx, "fingerprint_test_results"); err != nil {
				return err
			}
			if _, err := s.execTx(ctx, tx, `
				INSERT INTO fingerprint_test_results
					(id, channel_id, channel_name, model, sample_count, best_score, distribution, matches_json, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			`, rec.ID, channelID, rec.ChannelName, rec.Model, rec.SampleCount, rec.BestScore, distributionJSON, rec.MatchesJSON, createdAtUnix); err != nil {
				return err
			}
			return s.syncPostgresIDSequence(ctx, tx, "fingerprint_test_results")
		})
		if err != nil {
			return fmt.Errorf("insert fingerprint_test_results with id=%d: %w", rec.ID, err)
		}
		rec.CreatedAt = model.JSONTime{Time: createdAt}
		return nil
	}

	if s.IsPostgres() {
		if err := s.QueryRowContext(ctx, `
			INSERT INTO fingerprint_test_results
				(channel_id, channel_name, model, sample_count, best_score, distribution, matches_json, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			RETURNING id
		`, channelID, rec.ChannelName, rec.Model, rec.SampleCount, rec.BestScore, distributionJSON, rec.MatchesJSON, createdAtUnix).Scan(&rec.ID); err != nil {
			return fmt.Errorf("insert fingerprint_test_results: %w", err)
		}
		rec.CreatedAt = model.JSONTime{Time: createdAt}
		return nil
	}

	result, err := s.ExecContext(ctx, `
		INSERT INTO fingerprint_test_results
			(channel_id, channel_name, model, sample_count, best_score, distribution, matches_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, channelID, rec.ChannelName, rec.Model, rec.SampleCount, rec.BestScore, distributionJSON, rec.MatchesJSON, createdAtUnix)
	if err != nil {
		return fmt.Errorf("insert fingerprint_test_results: %w", err)
	}
	rec.ID, err = result.LastInsertId()
	if err != nil {
		return fmt.Errorf("get last insert id for fingerprint_test_results: %w", err)
	}
	rec.CreatedAt = model.JSONTime{Time: createdAt}
	return nil
}

// UpsertFingerprintTestResultReplica is the idempotent replica form of
// CreateFingerprintTestResult.
func (s *SQLStore) UpsertFingerprintTestResultReplica(ctx context.Context, rec *model.FingerprintTestRecord) error {
	if rec == nil || rec.ID <= 0 {
		return fmt.Errorf("invalid fingerprint test result replica")
	}
	distributionJSON, err := marshalJSON("distribution", rec.Distribution)
	if err != nil {
		return err
	}
	createdAt := rec.CreatedAt.Time
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	var channelID sql.NullInt64
	if rec.ChannelID != nil {
		channelID = sql.NullInt64{Int64: *rec.ChannelID, Valid: true}
	}
	query := `
		INSERT INTO fingerprint_test_results
			(id, channel_id, channel_name, model, sample_count, best_score, distribution, matches_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if s.supportsONConflict() {
		query += ` ON CONFLICT(id) DO UPDATE SET
			channel_id = excluded.channel_id, channel_name = excluded.channel_name, model = excluded.model,
			sample_count = excluded.sample_count, best_score = excluded.best_score,
			distribution = excluded.distribution, matches_json = excluded.matches_json, created_at = excluded.created_at`
	} else {
		query += ` ON DUPLICATE KEY UPDATE
			channel_id = VALUES(channel_id), channel_name = VALUES(channel_name), model = VALUES(model),
			sample_count = VALUES(sample_count), best_score = VALUES(best_score),
			distribution = VALUES(distribution), matches_json = VALUES(matches_json), created_at = VALUES(created_at)`
	}
	return s.WithTransaction(ctx, func(tx *sql.Tx) error {
		if err := s.lockPostgresExplicitIDTable(ctx, tx, "fingerprint_test_results"); err != nil {
			return err
		}
		if _, err := s.execTx(ctx, tx, query, rec.ID, channelID, rec.ChannelName, rec.Model,
			rec.SampleCount, rec.BestScore, distributionJSON, rec.MatchesJSON, timeToUnix(createdAt)); err != nil {
			return fmt.Errorf("upsert fingerprint test result replica: %w", err)
		}
		return s.syncPostgresIDSequence(ctx, tx, "fingerprint_test_results")
	})
}

// ListFingerprintTestResults 查询最近 limit 条对比结果。
func (s *SQLStore) ListFingerprintTestResults(ctx context.Context, limit int) ([]*model.FingerprintTestRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.listFingerprintTestResults(ctx, `ORDER BY created_at DESC LIMIT ?`, limit)
}

// ListFingerprintTestResultsReplicaPage scans a bounded ID-ordered page for reconciliation.
func (s *SQLStore) ListFingerprintTestResultsReplicaPage(
	ctx context.Context,
	afterID int64,
	limit int,
) ([]*model.FingerprintTestRecord, error) {
	if limit <= 0 {
		limit = 500
	}
	return s.listFingerprintTestResults(ctx, `WHERE id > ? ORDER BY id ASC LIMIT ?`, afterID, limit)
}

func (s *SQLStore) listFingerprintTestResults(
	ctx context.Context,
	suffix string,
	args ...any,
) ([]*model.FingerprintTestRecord, error) {
	rows, err := s.QueryContext(ctx, `
		SELECT id, channel_id, channel_name, model, sample_count, best_score, distribution, matches_json, created_at
		FROM fingerprint_test_results
		`+suffix, args...)
	if err != nil {
		return nil, fmt.Errorf("query fingerprint_test_results: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []*model.FingerprintTestRecord
	for rows.Next() {
		var rec model.FingerprintTestRecord
		var channelID sql.NullInt64
		var createdAt int64
		var distributionJSON string
		if err := rows.Scan(&rec.ID, &channelID, &rec.ChannelName, &rec.Model,
			&rec.SampleCount, &rec.BestScore, &distributionJSON, &rec.MatchesJSON, &createdAt); err != nil {
			return nil, fmt.Errorf("scan fingerprint_test_results row: %w", err)
		}
		if channelID.Valid {
			v := channelID.Int64
			rec.ChannelID = &v
		}
		if err := json.Unmarshal([]byte(rec.MatchesJSON), &rec.Matches); err != nil {
			return nil, fmt.Errorf("unmarshal fingerprint_test_results matches_json id=%d: %w", rec.ID, err)
		}
		if err := json.Unmarshal([]byte(distributionJSON), &rec.Distribution); err != nil {
			return nil, fmt.Errorf("unmarshal fingerprint_test_results distribution id=%d: %w", rec.ID, err)
		}
		rec.CreatedAt = model.JSONTime{Time: unixToTime(createdAt)}
		results = append(results, &rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate fingerprint_test_results: %w", err)
	}
	return results, nil
}

// DeleteFingerprintTestResult 删除一条对比结果。
func (s *SQLStore) DeleteFingerprintTestResult(ctx context.Context, id int64) error {
	if _, err := s.ExecContext(ctx, `DELETE FROM fingerprint_test_results WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete fingerprint_test_results id=%d: %w", id, err)
	}
	return nil
}
