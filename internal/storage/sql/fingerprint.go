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
		SELECT id, name, channel_id, channel_name, model, actual_model, channel_type,
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
		SELECT id, name, channel_id, channel_name, model, actual_model, channel_type,
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

// CreateModelFingerprint 插入新指纹基线，返回含 ID 的完整记录。
func (s *SQLStore) CreateModelFingerprint(ctx context.Context, fp *model.ModelFingerprint) (*model.ModelFingerprint, error) {
	now := timeToUnix(time.Now())

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

	if s.IsPostgres() {
		var newID int64
		err := s.QueryRowContext(ctx, `
			INSERT INTO model_fingerprints
				(name, channel_id, channel_name, model, actual_model, channel_type,
				 sample_count, distribution, stats, raw_data, prompt_version, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			RETURNING id
		`, fp.Name, channelID, fp.ChannelName, fp.Model, fp.ActualModel, fp.ChannelType,
			fp.SampleCount, distJSON, statsJSON, rawJSON, promptVer, now, now).Scan(&newID)
		if err != nil {
			return nil, fmt.Errorf("insert model_fingerprints: %w", err)
		}
		return s.GetModelFingerprint(ctx, newID)
	}

	res, err := s.ExecContext(ctx, `
		INSERT INTO model_fingerprints
			(name, channel_id, channel_name, model, actual_model, channel_type,
			 sample_count, distribution, stats, raw_data, prompt_version, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, fp.Name, channelID, fp.ChannelName, fp.Model, fp.ActualModel, fp.ChannelType,
		fp.SampleCount, distJSON, statsJSON, rawJSON, promptVer, now, now)
	if err != nil {
		return nil, fmt.Errorf("insert model_fingerprints: %w", err)
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("get last insert id for model_fingerprints: %w", err)
	}
	return s.GetModelFingerprint(ctx, newID)
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
		&fp.ID, &fp.Name, &channelID, &fp.ChannelName, &fp.Model, &fp.ActualModel, &fp.ChannelType,
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
