package app

import (
	"fmt"
	"net/http"
	"strings"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

type groupCreateRequest struct {
	Name              string                 `json:"name" binding:"required"`
	Mode              model.GroupMode        `json:"mode"`
	MatchRegex        string                 `json:"match_regex"`
	FirstTokenTimeOut int                    `json:"first_token_time_out"`
	SessionKeepTime   int                    `json:"session_keep_time"`
	Items             []model.GroupItemInput `json:"items"`
}

func (r *groupCreateRequest) Validate() error {
	if _, err := model.ValidateGroupName(r.Name); err != nil {
		return err
	}
	r.Mode = model.NormalizeGroupMode(r.Mode)
	matchRegex, err := model.ValidateGroupMatchRegex(r.MatchRegex)
	if err != nil {
		return err
	}
	r.MatchRegex = matchRegex
	r.FirstTokenTimeOut = model.NormalizeGroupFirstTokenTimeOut(r.FirstTokenTimeOut)
	r.SessionKeepTime = model.NormalizeGroupSessionKeepTime(r.SessionKeepTime)
	for i := range r.Items {
		if err := model.ValidateGroupItem(&r.Items[i]); err != nil {
			return fmt.Errorf("items[%d]: %w", i, err)
		}
	}
	return nil
}

type groupUpdateRequest struct {
	Name              *string                `json:"name,omitempty"`
	Mode              *model.GroupMode       `json:"mode,omitempty"`
	MatchRegex        *string                `json:"match_regex,omitempty"`
	FirstTokenTimeOut *int                   `json:"first_token_time_out,omitempty"`
	SessionKeepTime   *int                   `json:"session_keep_time,omitempty"`
	ItemsToAdd        []model.GroupItemInput `json:"items_to_add,omitempty"`
	ItemsToUpdate     []model.GroupItemInput `json:"items_to_update,omitempty"`
	ItemsToDelete     []int64                `json:"items_to_delete,omitempty"`
}

func (r *groupUpdateRequest) Validate() error {
	if r.Name != nil {
		if _, err := model.ValidateGroupName(*r.Name); err != nil {
			return err
		}
	}
	if r.Mode != nil {
		mode := model.NormalizeGroupMode(*r.Mode)
		r.Mode = &mode
	}
	if r.MatchRegex != nil {
		matchRegex, err := model.ValidateGroupMatchRegex(*r.MatchRegex)
		if err != nil {
			return err
		}
		r.MatchRegex = &matchRegex
	}
	if r.FirstTokenTimeOut != nil {
		value := model.NormalizeGroupFirstTokenTimeOut(*r.FirstTokenTimeOut)
		r.FirstTokenTimeOut = &value
	}
	if r.SessionKeepTime != nil {
		value := model.NormalizeGroupSessionKeepTime(*r.SessionKeepTime)
		r.SessionKeepTime = &value
	}
	for i := range r.ItemsToAdd {
		if err := model.ValidateGroupItem(&r.ItemsToAdd[i]); err != nil {
			return fmt.Errorf("items_to_add[%d]: %w", i, err)
		}
	}
	for i := range r.ItemsToUpdate {
		if err := model.ValidateGroupItem(&r.ItemsToUpdate[i]); err != nil {
			return fmt.Errorf("items_to_update[%d]: %w", i, err)
		}
	}
	return nil
}

func (r *groupUpdateRequest) ToModel() *model.GroupUpdateRequest {
	if r == nil {
		return nil
	}
	return &model.GroupUpdateRequest{
		Name:              r.Name,
		Mode:              r.Mode,
		MatchRegex:        r.MatchRegex,
		FirstTokenTimeOut: r.FirstTokenTimeOut,
		SessionKeepTime:   r.SessionKeepTime,
		ItemsToAdd:        r.ItemsToAdd,
		ItemsToUpdate:     r.ItemsToUpdate,
		ItemsToDelete:     r.ItemsToDelete,
	}
}

func (s *Server) HandleGroups(c *gin.Context) {
	switch c.Request.Method {
	case http.MethodGet:
		s.handleListGroups(c)
	case http.MethodPost:
		s.handleCreateGroup(c)
	default:
		RespondErrorMsg(c, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) HandleGroupByID(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid group id")
		return
	}

	switch c.Request.Method {
	case http.MethodGet:
		s.handleGetGroup(c, id)
	case http.MethodPut:
		s.handleUpdateGroup(c, id)
	case http.MethodDelete:
		s.handleDeleteGroup(c, id)
	default:
		RespondErrorMsg(c, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) HandleGroupModelOptions(c *gin.Context) {
	options, err := s.store.ListGroupModelOptions(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	if options == nil {
		options = []model.GroupModelOption{}
	}
	RespondJSON(c, http.StatusOK, options)
}

func (s *Server) handleListGroups(c *gin.Context) {
	groups, err := s.store.ListGroups(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	if groups == nil {
		groups = []*model.Group{}
	}
	RespondJSON(c, http.StatusOK, groups)
}

func (s *Server) handleCreateGroup(c *gin.Context) {
	var req groupCreateRequest
	if err := BindAndValidate(c, &req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if err := s.validateGroupItemsAgainstStore(c, req.Items, "items"); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, err.Error())
		return
	}

	items := make([]model.GroupItem, 0, len(req.Items))
	for _, item := range req.Items {
		items = append(items, model.GroupItem{
			ChannelID: item.ChannelID,
			ModelName: item.ModelName,
			Priority:  item.Priority,
			Weight:    item.Weight,
		})
	}

	group, err := s.store.CreateGroup(c.Request.Context(), &model.Group{
		Name:              req.Name,
		Mode:              req.Mode,
		MatchRegex:        req.MatchRegex,
		FirstTokenTimeOut: req.FirstTokenTimeOut,
		SessionKeepTime:   req.SessionKeepTime,
		Items:             items,
	})
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	RespondJSON(c, http.StatusCreated, group)
}

func (s *Server) handleGetGroup(c *gin.Context, id int64) {
	group, err := s.store.GetGroup(c.Request.Context(), id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			RespondErrorMsg(c, http.StatusNotFound, "group not found")
			return
		}
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	RespondJSON(c, http.StatusOK, group)
}

func (s *Server) handleUpdateGroup(c *gin.Context, id int64) {
	var req groupUpdateRequest
	if err := BindAndValidate(c, &req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if err := s.validateGroupItemsAgainstStore(c, req.ItemsToAdd, "items_to_add"); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.validateGroupItemsAgainstStore(c, req.ItemsToUpdate, "items_to_update"); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, err.Error())
		return
	}

	group, err := s.store.UpdateGroup(c.Request.Context(), id, req.ToModel())
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			RespondErrorMsg(c, http.StatusNotFound, "group not found")
			return
		}
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	RespondJSON(c, http.StatusOK, group)
}

func (s *Server) handleDeleteGroup(c *gin.Context, id int64) {
	if err := s.store.DeleteGroup(c.Request.Context(), id); err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"id": id})
}

func (s *Server) validateGroupItemsAgainstStore(c *gin.Context, items []model.GroupItemInput, field string) error {
	ctx := c.Request.Context()
	for i := range items {
		cfg, err := s.store.GetConfig(ctx, items[i].ChannelID)
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				return fmt.Errorf("%s[%d]: channel_id %d not found", field, i, items[i].ChannelID)
			}
			return err
		}
		if !cfg.SupportsModel(items[i].ModelName) {
			return fmt.Errorf("%s[%d]: model %q not found in channel %d", field, i, items[i].ModelName, items[i].ChannelID)
		}
	}
	return nil
}
