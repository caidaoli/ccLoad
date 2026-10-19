package qoderauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

const authCampaignPath = "/sash/api/v1/me/campaigns"

// CheckinResult reports the confirmed campaign state, never an assumed reward.
type CheckinResult struct {
	Status     string  `json:"status"`
	CampaignID string  `json:"campaign_id,omitempty"`
	Message    string  `json:"message,omitempty"`
	Unit       string  `json:"unit,omitempty"`
	Reward     float64 `json:"reward,omitempty"`
}

type authCampaign struct {
	CampaignID  string `json:"campaignId"`
	ActionType  string `json:"actionType"`
	ClaimStatus string `json:"claimStatus"`
}

func (s *Service) authCampaigns(ctx context.Context, c *Credential, headers http.Header) (map[string]json.RawMessage, []authCampaign, error) {
	data, _, err := s.authDo(ctx, http.MethodGet, s.authEndpoint(c, authCampaignPath+"?forceRefresh=true"), nil, headers)
	if err != nil {
		return nil, nil, err
	}
	var campaigns []authCampaign
	if raw, exists := data["campaigns"]; !exists || json.Unmarshal(raw, &campaigns) != nil {
		return nil, nil, errors.New("invalid Qoder campaigns response")
	}
	return data, campaigns, nil
}

// Checkin claims an available benefit campaign. A failed or ambiguous claim is
// verified against that same campaign before reporting an already-claimed state.
func (s *Service) Checkin(ctx context.Context, credential *Credential) (*CheckinResult, error) {
	c, err := authCopy(credential)
	if err != nil {
		return nil, err
	}
	if c.AccessToken == "" {
		return nil, errors.New("qoder access_token is required")
	}
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+c.AccessToken)
	headers.Set("Cosy-ClientType", "10")
	headers.Set("Accept", "application/json")
	headers.Set("Accept-Language", "zh-CN")
	headers.Set("User-Agent", "Qoder")
	missing := &CheckinResult{Status: "missing_component", Message: "Qoder international check-in requires the official machine risk component"}
	if c.Region == "global" && s.RiskHeaders == nil {
		return missing, nil
	}
	data, campaigns, err := s.authCampaigns(ctx, c, headers)
	if err != nil {
		return nil, err
	}
	if c.Region == "global" {
		riskCredential := *c
		// Sash's UID is the identity expected by the machine component.
		riskCredential.UserID = authText(data, "uid")
		if riskCredential.UserID == "" {
			return nil, errors.New("qoder campaigns response is missing uid")
		}
		risk, riskErr := s.RiskHeaders(ctx, &riskCredential)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if riskErr != nil {
			return missing, nil
		}
		for _, key := range []string{"Cosy-MachineToken", "Cosy-MachineCode", "Cosy-MachineType"} {
			value := strings.TrimSpace(risk.Get(key))
			if value == "" {
				return missing, nil
			}
			headers.Set(key, value)
		}
		_, campaigns, err = s.authCampaigns(ctx, c, headers)
		if err != nil {
			return nil, err
		}
	}
	var target *authCampaign
	var claimed *authCampaign
	for i := range campaigns {
		campaign := &campaigns[i]
		if campaign.ActionType != "CLAIM_BENEFIT" {
			continue
		}
		switch campaign.ClaimStatus {
		case "CLAIMABLE":
			if target == nil {
				target = campaign
			}
		case "CLAIMED":
			if claimed == nil {
				claimed = campaign
			}
		}
	}
	if target == nil {
		if claimed != nil {
			return &CheckinResult{Status: "already_claimed", CampaignID: claimed.CampaignID}, nil
		}
		return &CheckinResult{Status: "unavailable", Message: "No claimable Qoder benefit campaign"}, nil
	}
	if target.CampaignID == "" {
		return nil, errors.New("qoder claimable campaign is missing campaignId")
	}
	result := &CheckinResult{CampaignID: target.CampaignID}
	claim, claimStatus, claimErr := s.authDo(ctx, http.MethodPost, s.authEndpoint(c, authCampaignPath+"/"+url.PathEscape(target.CampaignID)+"/claim"), struct{}{}, headers)
	status := authText(claim, "status")
	if claimErr == nil && status == "CLAIMED" {
		var replayed bool
		_ = json.Unmarshal(claim["replayed"], &replayed)
		result.Status = "claimed"
		if replayed {
			result.Status = "already_claimed"
			return result, nil
		}
		var benefit map[string]json.RawMessage
		if json.Unmarshal(claim["benefit"], &benefit) == nil {
			if reward, ok := authNumber(benefit["amount"]); ok && reward >= 0 {
				result.Reward = reward
			}
			result.Unit = authText(benefit, "kind")
			if result.Unit == "CREDITS" {
				result.Unit = "credits"
			}
		}
		return result, nil
	}
	if (claimErr == nil || claimStatus == http.StatusConflict) && authText(claim, "result") == "ALREADY_CLAIMED" {
		result.Status = "already_claimed"
		return result, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	_, confirmed, verifyErr := s.authCampaigns(ctx, c, headers)
	if verifyErr == nil {
		for _, campaign := range confirmed {
			if campaign.CampaignID == target.CampaignID && campaign.ActionType == "CLAIM_BENEFIT" && campaign.ClaimStatus == "CLAIMED" {
				result.Status = "already_claimed"
				result.Message = "Campaign claim confirmed after an uncertain response"
				return result, nil
			}
		}
	}
	if claimErr != nil {
		return nil, claimErr
	}
	return nil, errors.New("qoder did not confirm the campaign claim")
}
