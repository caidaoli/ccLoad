package model

import (
	"errors"
	"regexp"
	"strings"
)

type GroupMode int

const (
	GroupModeRoundRobin GroupMode = 1
	GroupModeRandom     GroupMode = 2
	GroupModeFailover   GroupMode = 3
	GroupModeWeighted   GroupMode = 4
)

type Group struct {
	ID                int64       `json:"id"`
	Name              string      `json:"name"`
	Mode              GroupMode   `json:"mode"`
	MatchRegex        string      `json:"match_regex"`
	FirstTokenTimeOut int         `json:"first_token_time_out"`
	SessionKeepTime   int         `json:"session_keep_time"`
	Items             []GroupItem `json:"items,omitempty"`
	CreatedAt         JSONTime    `json:"created_at"`
	UpdatedAt         JSONTime    `json:"updated_at"`
}

type GroupItem struct {
	ID        int64    `json:"id"`
	GroupID   int64    `json:"group_id"`
	ChannelID int64    `json:"channel_id"`
	ModelName string   `json:"model_name"`
	Priority  int      `json:"priority"`
	Weight    int      `json:"weight"`
	CreatedAt JSONTime `json:"created_at"`
	UpdatedAt JSONTime `json:"updated_at"`
}

type GroupUpdateRequest struct {
	Name              *string          `json:"name,omitempty"`
	Mode              *GroupMode       `json:"mode,omitempty"`
	MatchRegex        *string          `json:"match_regex,omitempty"`
	FirstTokenTimeOut *int             `json:"first_token_time_out,omitempty"`
	SessionKeepTime   *int             `json:"session_keep_time,omitempty"`
	ItemsToAdd        []GroupItemInput `json:"items_to_add,omitempty"`
	ItemsToUpdate     []GroupItemInput `json:"items_to_update,omitempty"`
	ItemsToDelete     []int64          `json:"items_to_delete,omitempty"`
}

type GroupItemInput struct {
	ID        int64  `json:"id,omitempty"`
	ChannelID int64  `json:"channel_id"`
	ModelName string `json:"model_name"`
	Priority  int    `json:"priority"`
	Weight    int    `json:"weight"`
}

type GroupModelOption struct {
	ChannelID   int64  `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	ModelName   string `json:"model_name"`
}

func NormalizeGroupMode(mode GroupMode) GroupMode {
	switch mode {
	case GroupModeRoundRobin, GroupModeRandom, GroupModeFailover, GroupModeWeighted:
		return mode
	default:
		return GroupModeFailover
	}
}

func ValidateGroupName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("group name cannot be empty")
	}
	if strings.ContainsAny(name, "\x00\r\n") {
		return "", errors.New("group name contains illegal characters")
	}
	return name, nil
}

func ValidateGroupMatchRegex(matchRegex string) (string, error) {
	matchRegex = strings.TrimSpace(matchRegex)
	if matchRegex == "" {
		return "", nil
	}
	if _, err := regexp.Compile(matchRegex); err != nil {
		return "", err
	}
	return matchRegex, nil
}

func NormalizeGroupFirstTokenTimeOut(seconds int) int {
	if seconds < 0 {
		return 0
	}
	return seconds
}

func NormalizeGroupSessionKeepTime(seconds int) int {
	if seconds < 0 {
		return 0
	}
	return seconds
}

func ValidateGroupItem(item *GroupItemInput) error {
	if item == nil {
		return errors.New("group item cannot be nil")
	}
	item.ModelName = strings.TrimSpace(item.ModelName)
	if item.ChannelID <= 0 {
		return errors.New("channel_id must be positive")
	}
	if item.ModelName == "" {
		return errors.New("model_name cannot be empty")
	}
	if strings.ContainsAny(item.ModelName, "\x00\r\n") {
		return errors.New("model_name contains illegal characters")
	}
	if item.Weight <= 0 {
		item.Weight = 1
	}
	return nil
}
