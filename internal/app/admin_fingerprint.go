package app

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
)

// HandleListFingerprints GET /admin/fingerprints
func (s *Server) HandleListFingerprints(c *gin.Context) {
	fps, err := s.store.ListModelFingerprints(c.Request.Context())
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	if fps == nil {
		fps = []*model.ModelFingerprint{}
	}
	RespondJSON(c, http.StatusOK, fps)
}

// HandleGetFingerprint GET /admin/fingerprints/:id
func (s *Server) HandleGetFingerprint(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid fingerprint id")
		return
	}
	fp, err := s.store.GetModelFingerprint(c.Request.Context(), id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			RespondErrorMsg(c, http.StatusNotFound, fmt.Sprintf("fingerprint %d not found", id))
		} else {
			RespondError(c, http.StatusInternalServerError, err)
		}
		return
	}
	if fp == nil {
		RespondErrorMsg(c, http.StatusNotFound, fmt.Sprintf("fingerprint %d not found", id))
		return
	}
	RespondJSON(c, http.StatusOK, fp)
}

// HandleDeleteFingerprint DELETE /admin/fingerprints/:id
func (s *Server) HandleDeleteFingerprint(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid fingerprint id")
		return
	}
	if err := s.store.DeleteModelFingerprint(c.Request.Context(), id); err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"deleted": true})
}

// HandleCalibrateFingerprint POST /admin/fingerprints/calibrate
func (s *Server) HandleCalibrateFingerprint(c *gin.Context) {
	var req calibrateReq
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Model = strings.TrimSpace(req.Model)
	if req.Name == "" {
		RespondErrorMsg(c, http.StatusBadRequest, "name is required")
		return
	}
	if req.Model == "" {
		RespondErrorMsg(c, http.StatusBadRequest, "model is required")
		return
	}

	cfg, err := s.store.GetConfig(c.Request.Context(), req.ChannelID)
	if err != nil || cfg == nil {
		RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("channel %d not found", req.ChannelID))
		return
	}

	keys, err := s.store.GetAPIKeys(c.Request.Context(), req.ChannelID)
	if err != nil || len(keys) == 0 {
		RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("channel %d has no API keys", req.ChannelID))
		return
	}

	if !cfg.SupportsModel(req.Model) {
		RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("channel %d does not support model %q", req.ChannelID, req.Model))
		return
	}

	// ClampFingerprintParams is called inside StartCalibrate; params errors surface as non-limit errors.
	jobID, err := s.fingerprintJobs.StartCalibrate(s, req)
	if err != nil {
		if isFPJobLimitError(err) {
			RespondErrorMsg(c, http.StatusTooManyRequests, err.Error())
			return
		}
		RespondErrorMsg(c, http.StatusBadRequest, err.Error())
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"job_id": jobID})
}

// HandleTestFingerprint POST /admin/fingerprints/test
func (s *Server) HandleTestFingerprint(c *gin.Context) {
	var req testFingerprintReq
	if err := c.ShouldBindJSON(&req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	req.Model = strings.TrimSpace(req.Model)
	if req.Model == "" {
		RespondErrorMsg(c, http.StatusBadRequest, "model is required")
		return
	}

	cfg, err := s.store.GetConfig(c.Request.Context(), req.ChannelID)
	if err != nil || cfg == nil {
		RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("channel %d not found", req.ChannelID))
		return
	}

	keys, err := s.store.GetAPIKeys(c.Request.Context(), req.ChannelID)
	if err != nil || len(keys) == 0 {
		RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("channel %d has no API keys", req.ChannelID))
		return
	}

	if !cfg.SupportsModel(req.Model) {
		RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("channel %d does not support model %q", req.ChannelID, req.Model))
		return
	}

	if err := s.validateFingerprintBaseline(c, req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, err.Error())
		return
	}

	jobID, err := s.fingerprintJobs.StartTest(s, req)
	if err != nil {
		if isFPJobLimitError(err) {
			RespondErrorMsg(c, http.StatusTooManyRequests, err.Error())
			return
		}
		RespondErrorMsg(c, http.StatusBadRequest, err.Error())
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"job_id": jobID})
}

// HandleFingerprintJob GET /admin/fingerprints/jobs/:id
func (s *Server) HandleFingerprintJob(c *gin.Context) {
	jobID := c.Param("id")
	view, ok := s.fingerprintJobs.Get(jobID)
	if !ok {
		RespondErrorMsg(c, http.StatusNotFound, fmt.Sprintf("job %s not found", jobID))
		return
	}
	RespondJSON(c, http.StatusOK, view)
}

// HandleCancelFingerprintJob POST /admin/fingerprints/jobs/:id/cancel
func (s *Server) HandleCancelFingerprintJob(c *gin.Context) {
	jobID := c.Param("id")
	if err := s.fingerprintJobs.Cancel(jobID); err != nil {
		RespondErrorMsg(c, http.StatusNotFound, err.Error())
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"cancelled": true})
}

// validateFingerprintBaseline ensures at least one v1 baseline exists (or the specified id is v1).
func (s *Server) validateFingerprintBaseline(c *gin.Context, req testFingerprintReq) error {
	if req.FingerprintID != nil {
		fp, err := s.store.GetModelFingerprint(c.Request.Context(), *req.FingerprintID)
		if err != nil || fp == nil {
			return fmt.Errorf("fingerprint_id %d not found", *req.FingerprintID)
		}
		if fp.PromptVersion != util.FingerprintPromptVersion {
			return fmt.Errorf("fingerprint_id %d is not prompt_version=%s", *req.FingerprintID, util.FingerprintPromptVersion)
		}
		return nil
	}
	all, err := s.store.ListModelFingerprints(c.Request.Context())
	if err != nil {
		return fmt.Errorf("list fingerprints: %v", err)
	}
	for _, fp := range all {
		if fp.PromptVersion == util.FingerprintPromptVersion {
			return nil
		}
	}
	return fmt.Errorf("no %s baselines found; run calibrate first", util.FingerprintPromptVersion)
}

// isFPJobLimitError detects "too many running" from FingerprintJobManager.
func isFPJobLimitError(err error) bool {
	return errors.Is(err, ErrFingerprintJobsBusy)
}

// HandleListFingerprintTestResults GET /admin/fingerprints/test-results
func (s *Server) HandleListFingerprintTestResults(c *gin.Context) {
	results, err := s.store.ListFingerprintTestResults(c.Request.Context(), 50)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	if results == nil {
		results = []*model.FingerprintTestRecord{}
	}
	RespondJSON(c, http.StatusOK, results)
}

// HandleDeleteFingerprintTestResult DELETE /admin/fingerprints/test-results/:id
func (s *Server) HandleDeleteFingerprintTestResult(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid test result id")
		return
	}
	if err := s.store.DeleteFingerprintTestResult(c.Request.Context(), id); err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"deleted": true})
}
