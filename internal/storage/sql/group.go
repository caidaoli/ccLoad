package sql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"ccLoad/internal/model"
)

func (s *SQLStore) ListGroups(ctx context.Context) ([]*model.Group, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, mode, match_regex, first_token_time_out, session_keep_time, created_at, updated_at
		FROM groups
		ORDER BY name ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var groups []*model.Group
	for rows.Next() {
		group, err := scanGroupRow(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate groups: %w", err)
	}

	for _, group := range groups {
		items, err := s.loadGroupItems(ctx, group.ID)
		if err != nil {
			return nil, err
		}
		group.Items = items
	}

	return groups, nil
}

func (s *SQLStore) GetGroup(ctx context.Context, id int64) (*model.Group, error) {
	group, err := s.loadSingleGroup(ctx, `
		SELECT id, name, mode, match_regex, first_token_time_out, session_keep_time, created_at, updated_at
		FROM groups
		WHERE id = ?
	`, id)
	if err != nil {
		return nil, err
	}

	items, err := s.loadGroupItems(ctx, group.ID)
	if err != nil {
		return nil, err
	}
	group.Items = items
	return group, nil
}

func (s *SQLStore) GetGroupByName(ctx context.Context, name string) (*model.Group, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("group name cannot be empty")
	}

	group, err := s.loadSingleGroup(ctx, `
		SELECT id, name, mode, match_regex, first_token_time_out, session_keep_time, created_at, updated_at
		FROM groups
		WHERE name = ?
	`, name)
	if err != nil {
		return nil, err
	}

	items, err := s.loadGroupItems(ctx, group.ID)
	if err != nil {
		return nil, err
	}
	group.Items = items
	return group, nil
}

func (s *SQLStore) CreateGroup(ctx context.Context, group *model.Group) (*model.Group, error) {
	if group == nil {
		return nil, errors.New("group cannot be nil")
	}

	name, err := model.ValidateGroupName(group.Name)
	if err != nil {
		return nil, err
	}
	mode := model.NormalizeGroupMode(group.Mode)
	matchRegex, err := model.ValidateGroupMatchRegex(group.MatchRegex)
	if err != nil {
		return nil, err
	}
	firstTokenTimeOut := model.NormalizeGroupFirstTokenTimeOut(group.FirstTokenTimeOut)
	sessionKeepTime := model.NormalizeGroupSessionKeepTime(group.SessionKeepTime)
	now := time.Now()
	nowUnix := timeToUnix(now)
	groupID := group.ID

	err = s.WithTransaction(ctx, func(tx *sql.Tx) error {
		if groupID > 0 {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO groups(id, name, mode, match_regex, first_token_time_out, session_keep_time, created_at, updated_at)
				VALUES(?, ?, ?, ?, ?, ?, ?, ?)
			`, groupID, name, mode, matchRegex, firstTokenTimeOut, sessionKeepTime, nowUnix, nowUnix)
			if err != nil {
				return fmt.Errorf("insert group with explicit id: %w", err)
			}
		} else {
			res, err := tx.ExecContext(ctx, `
				INSERT INTO groups(name, mode, match_regex, first_token_time_out, session_keep_time, created_at, updated_at)
				VALUES(?, ?, ?, ?, ?, ?, ?)
			`, name, mode, matchRegex, firstTokenTimeOut, sessionKeepTime, nowUnix, nowUnix)
			if err != nil {
				return fmt.Errorf("insert group: %w", err)
			}
			groupID, err = res.LastInsertId()
			if err != nil {
				return fmt.Errorf("get group id: %w", err)
			}
		}

		if err := insertGroupItemsTx(ctx, tx, groupID, group.Items, nowUnix); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return s.GetGroup(ctx, groupID)
}

func (s *SQLStore) UpdateGroup(ctx context.Context, id int64, req *model.GroupUpdateRequest) (*model.Group, error) {
	if req == nil {
		return nil, errors.New("group update request cannot be nil")
	}

	existing, err := s.GetGroup(ctx, id)
	if err != nil {
		return nil, err
	}

	name := existing.Name
	if req.Name != nil {
		name, err = model.ValidateGroupName(*req.Name)
		if err != nil {
			return nil, err
		}
	}

	mode := existing.Mode
	if req.Mode != nil {
		mode = model.NormalizeGroupMode(*req.Mode)
	}
	matchRegex := existing.MatchRegex
	if req.MatchRegex != nil {
		matchRegex, err = model.ValidateGroupMatchRegex(*req.MatchRegex)
		if err != nil {
			return nil, err
		}
	}
	firstTokenTimeOut := existing.FirstTokenTimeOut
	if req.FirstTokenTimeOut != nil {
		firstTokenTimeOut = model.NormalizeGroupFirstTokenTimeOut(*req.FirstTokenTimeOut)
	}
	sessionKeepTime := existing.SessionKeepTime
	if req.SessionKeepTime != nil {
		sessionKeepTime = model.NormalizeGroupSessionKeepTime(*req.SessionKeepTime)
	}

	now := time.Now()
	nowUnix := timeToUnix(now)

	err = s.WithTransaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE groups
			SET name = ?, mode = ?, match_regex = ?, first_token_time_out = ?, session_keep_time = ?, updated_at = ?
			WHERE id = ?
		`, name, mode, matchRegex, firstTokenTimeOut, sessionKeepTime, nowUnix, id)
		if err != nil {
			return fmt.Errorf("update group: %w", err)
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			return errors.New("not found")
		}

		for _, itemID := range req.ItemsToDelete {
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM group_items
				WHERE id = ? AND group_id = ?
			`, itemID, id); err != nil {
				return fmt.Errorf("delete group item %d: %w", itemID, err)
			}
		}

		for _, rawItem := range req.ItemsToUpdate {
			if rawItem.ID <= 0 {
				return errors.New("group item update id must be positive")
			}
			item := rawItem
			if err := model.ValidateGroupItem(&item); err != nil {
				return err
			}

			result, err := tx.ExecContext(ctx, `
				UPDATE group_items
				SET channel_id = ?, model_name = ?, priority = ?, weight = ?, updated_at = ?
				WHERE id = ? AND group_id = ?
			`, item.ChannelID, item.ModelName, item.Priority, item.Weight, nowUnix, item.ID, id)
			if err != nil {
				return fmt.Errorf("update group item %d: %w", item.ID, err)
			}
			if affected, _ := result.RowsAffected(); affected == 0 {
				return fmt.Errorf("group item not found: %d", item.ID)
			}
		}

		if err := insertGroupItemInputsTx(ctx, tx, id, req.ItemsToAdd, nowUnix); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return s.GetGroup(ctx, id)
}

func (s *SQLStore) DeleteGroup(ctx context.Context, id int64) error {
	return s.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM group_items WHERE group_id = ?`, id); err != nil {
			return fmt.Errorf("delete group items: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM groups WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete group: %w", err)
		}
		return nil
	})
}

func (s *SQLStore) ListGroupModelOptions(ctx context.Context) ([]model.GroupModelOption, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.name, cm.model
		FROM channels c
		INNER JOIN channel_models cm ON c.id = cm.channel_id
		WHERE c.enabled = 1
		ORDER BY c.name ASC, cm.model ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list group model options: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var options []model.GroupModelOption
	for rows.Next() {
		var option model.GroupModelOption
		if err := rows.Scan(&option.ChannelID, &option.ChannelName, &option.ModelName); err != nil {
			return nil, fmt.Errorf("scan group model option: %w", err)
		}
		options = append(options, option)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate group model options: %w", err)
	}

	return options, nil
}

func (s *SQLStore) loadSingleGroup(ctx context.Context, query string, args ...any) (*model.Group, error) {
	row := s.db.QueryRowContext(ctx, query, args...)

	group := &model.Group{}
	var matchRegex string
	var firstTokenTimeOut int
	var sessionKeepTime int
	var createdAt int64
	var updatedAt int64
	if err := row.Scan(&group.ID, &group.Name, &group.Mode, &matchRegex, &firstTokenTimeOut, &sessionKeepTime, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("not found")
		}
		return nil, fmt.Errorf("scan group: %w", err)
	}
	group.MatchRegex = matchRegex
	group.FirstTokenTimeOut = model.NormalizeGroupFirstTokenTimeOut(firstTokenTimeOut)
	group.SessionKeepTime = model.NormalizeGroupSessionKeepTime(sessionKeepTime)
	group.CreatedAt = model.JSONTime{Time: unixToTime(createdAt)}
	group.UpdatedAt = model.JSONTime{Time: unixToTime(updatedAt)}
	return group, nil
}

func (s *SQLStore) loadGroupItems(ctx context.Context, groupID int64) ([]model.GroupItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, group_id, channel_id, model_name, priority, weight, created_at, updated_at
		FROM group_items
		WHERE group_id = ?
		ORDER BY priority ASC, id ASC
	`, groupID)
	if err != nil {
		return nil, fmt.Errorf("query group items: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]model.GroupItem, 0)
	for rows.Next() {
		var item model.GroupItem
		var createdAt int64
		var updatedAt int64
		if err := rows.Scan(&item.ID, &item.GroupID, &item.ChannelID, &item.ModelName, &item.Priority, &item.Weight, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan group item: %w", err)
		}
		item.CreatedAt = model.JSONTime{Time: unixToTime(createdAt)}
		item.UpdatedAt = model.JSONTime{Time: unixToTime(updatedAt)}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate group items: %w", err)
	}
	return items, nil
}

func scanGroupRow(scanner interface{ Scan(dest ...any) error }) (*model.Group, error) {
	group := &model.Group{}
	var matchRegex string
	var firstTokenTimeOut int
	var sessionKeepTime int
	var createdAt int64
	var updatedAt int64
	if err := scanner.Scan(&group.ID, &group.Name, &group.Mode, &matchRegex, &firstTokenTimeOut, &sessionKeepTime, &createdAt, &updatedAt); err != nil {
		return nil, fmt.Errorf("scan group row: %w", err)
	}
	group.MatchRegex = matchRegex
	group.FirstTokenTimeOut = model.NormalizeGroupFirstTokenTimeOut(firstTokenTimeOut)
	group.SessionKeepTime = model.NormalizeGroupSessionKeepTime(sessionKeepTime)
	group.CreatedAt = model.JSONTime{Time: unixToTime(createdAt)}
	group.UpdatedAt = model.JSONTime{Time: unixToTime(updatedAt)}
	return group, nil
}

func insertGroupItemsTx(ctx context.Context, tx *sql.Tx, groupID int64, items []model.GroupItem, nowUnix int64) error {
	if len(items) == 0 {
		return nil
	}

	for _, rawItem := range items {
		item := model.GroupItemInput{
			ID:        rawItem.ID,
			ChannelID: rawItem.ChannelID,
			ModelName: rawItem.ModelName,
			Priority:  rawItem.Priority,
			Weight:    rawItem.Weight,
		}
		if err := insertSingleGroupItemTx(ctx, tx, groupID, item, nowUnix); err != nil {
			return err
		}
	}
	return nil
}

func insertGroupItemInputsTx(ctx context.Context, tx *sql.Tx, groupID int64, items []model.GroupItemInput, nowUnix int64) error {
	if len(items) == 0 {
		return nil
	}
	for _, item := range items {
		if err := insertSingleGroupItemTx(ctx, tx, groupID, item, nowUnix); err != nil {
			return err
		}
	}
	return nil
}

func insertSingleGroupItemTx(ctx context.Context, tx *sql.Tx, groupID int64, rawItem model.GroupItemInput, nowUnix int64) error {
	item := rawItem
	if err := model.ValidateGroupItem(&item); err != nil {
		return err
	}

	if item.ID > 0 {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO group_items(id, group_id, channel_id, model_name, priority, weight, created_at, updated_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		`, item.ID, groupID, item.ChannelID, item.ModelName, item.Priority, item.Weight, nowUnix, nowUnix)
		if err != nil {
			return fmt.Errorf("insert group item with explicit id: %w", err)
		}
		return nil
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO group_items(group_id, channel_id, model_name, priority, weight, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)
	`, groupID, item.ChannelID, item.ModelName, item.Priority, item.Weight, nowUnix, nowUnix)
	if err != nil {
		return fmt.Errorf("insert group item: %w", err)
	}
	return nil
}
