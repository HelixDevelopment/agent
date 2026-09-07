package services

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"digital.vasic.debate/comprehensive"
)

// upstreamStubMarkers are the substrings the upstream orchestrator writes into
// agent content when NO LLM provider is wired. digital.vasic.debate's
// synthesiseContent() emits
//
//	[synthesised round=N agent=... digest=...] Position on "<topic>":
//	deterministic-stub-content awaiting provider wiring.
//
// and its own README documents the marker as the detection mechanism. Content
// carrying it is a placeholder, never an answer, and must never reach a user.
var upstreamStubMarkers = []string{
	"[synthesised ",
	"deterministic-stub-content",
	"[invoker-error ",
}

// isUpstreamStubContent reports whether content is upstream placeholder text
// rather than model output.
func isUpstreamStubContent(content string) bool {
	for _, marker := range upstreamStubMarkers {
		if strings.Contains(content, marker) {
			return true
		}
	}
	return false
}

// comprehensiveAgentContent extracts genuine model-generated text from a
// comprehensive DebateResponse, returning the final position and the
// per-agent responses behind it.
//
// It returns ("", nil) when the response contains no usable content — either
// because no agent produced any, or because everything produced is upstream
// stub text. Callers MUST treat that as a failure rather than substituting a
// status line (§11.4 — a status line presented as an answer is a PASS-bluff).
func comprehensiveAgentContent(resp *comprehensive.DebateResponse) (string, []ParticipantResponse) {
	if resp == nil {
		return "", nil
	}

	participants := make([]ParticipantResponse, 0)
	var best string
	var bestScore float64 = -1

	for _, phase := range resp.Phases {
		if phase == nil {
			continue
		}
		for _, ar := range phase.Responses {
			if ar == nil {
				continue
			}
			content := strings.TrimSpace(ar.Content)
			if content == "" || isUpstreamStubContent(content) {
				continue
			}
			participants = append(participants, ParticipantResponse{
				ParticipantID:   ar.AgentID,
				ParticipantName: ar.AgentID,
				Role:            ar.Role,
				Round:           phase.Round,
				RoundNumber:     phase.Round,
				Response:        content,
				Content:         content,
				Confidence:      ar.Confidence,
				QualityScore:    ar.Score,
				ResponseTime:    ar.Latency,
				LLMProvider:     ar.Provider,
				LLMModel:        ar.Model,
			})
			if ar.Score > bestScore {
				bestScore = ar.Score
				best = content
			}
		}
	}

	if best == "" {
		return "", nil
	}
	return best, participants
}

// SetComprehensiveIntegration sets the comprehensive debate integration and enables it
func (ds *DebateService) SetComprehensiveIntegration(integration *comprehensive.IntegrationManager) {
	ds.comprehensiveIntegration = integration
	ds.useComprehensiveSystem = true
	ds.logger.Info("[Comprehensive Debate] Integration configured and enabled")
}

// EnableComprehensiveSystem enables the comprehensive debate system
func (ds *DebateService) EnableComprehensiveSystem(enable bool) {
	ds.useComprehensiveSystem = enable
	ds.logger.WithField("enabled", enable).Info("[Comprehensive Debate] System toggled")
}

// conductComprehensiveDebate executes a debate using the new comprehensive multi-agent system
func (ds *DebateService) conductComprehensiveDebate(
	ctx context.Context,
	config *DebateConfig,
	startTime time.Time,
	sessionID string,
) (*DebateResult, error) {
	ds.logger.WithFields(logrus.Fields{
		"debate_id": config.DebateID,
		"topic":     config.Topic,
	}).Info("[Comprehensive Debate] Starting multi-agent debate")

	// Convert service DebateConfig to comprehensive DebateRequest
	compReq := &comprehensive.DebateRequest{
		ID:        config.DebateID,
		Topic:     config.Topic,
		Context:   config.Topic, // Use topic as context for now
		Language:  "go",         // Default to Go, could be detected from topic
		MaxRounds: 3,            // Default rounds
	}

	// Execute debate through comprehensive system
	compResp, err := ds.comprehensiveIntegration.ExecuteDebate(ctx, compReq)
	if err != nil {
		ds.logger.WithError(err).Error("[Comprehensive Debate] Debate execution failed")
		return nil, fmt.Errorf("comprehensive debate failed: %w", err)
	}

	endTime := time.Now()

	// The comprehensive DebateResponse carries NO final-answer field; the only
	// model-produced text lives in Phases[].Responses[].Content. Earlier this
	// function discarded that entirely and substituted the fixed string
	// "Comprehensive debate completed with N rounds", which was returned
	// verbatim to callers for every prompt — a status line presented as an
	// answer. Surface the real agent content instead, and refuse rather than
	// fabricate when there is none.
	finalPosition, participants := comprehensiveAgentContent(compResp)
	if finalPosition == "" {
		ds.logger.WithField("debate_id", config.DebateID).
			Error("[Comprehensive Debate] no usable agent content returned; refusing to " +
				"substitute a status line for an answer")
		return nil, fmt.Errorf(
			"comprehensive debate produced no model-generated content for debate %s: "+
				"its orchestrator has no LLM provider wired, so it cannot answer a prompt "+
				"(unset %s to use the provider-backed debate path)",
			config.DebateID, envEnableComprehensiveDebate)
	}

	// Convert comprehensive response to service DebateResult
	result := &DebateResult{
		DebateID:        config.DebateID,
		SessionID:       sessionID,
		Topic:           config.Topic,
		StartTime:       startTime,
		EndTime:         endTime,
		Duration:        endTime.Sub(startTime),
		TotalRounds:     compResp.RoundsConducted,
		RoundsConducted: compResp.RoundsConducted,
		Participants:    participants,
		Consensus: &ConsensusResult{
			Reached:        compResp.Success,
			Achieved:       compResp.Success,
			Confidence:     compResp.QualityScore,
			AgreementLevel: compResp.QualityScore,
			FinalPosition:  finalPosition,
			Summary:        finalPosition,
			KeyPoints:      []string{},
			Disagreements:  []string{},
			Timestamp:      endTime,
			QualityScore:   compResp.QualityScore,
		},
		QualityScore: compResp.QualityScore,
		FinalScore:   compResp.QualityScore,
		Success:      compResp.Success,
		Metadata: map[string]any{
			"comprehensive_debate": true,
			"rounds_conducted":     compResp.RoundsConducted,
			"phases":               len(compResp.Phases),
		},
	}

	ds.logger.WithFields(logrus.Fields{
		"debate_id":     config.DebateID,
		"success":       result.Success,
		"total_rounds":  result.TotalRounds,
		"quality_score": result.QualityScore,
		"duration":      result.Duration,
	}).Info("[Comprehensive Debate] Multi-agent debate completed")

	return result, nil
}

// conductComprehensiveDebateStreaming executes a streaming debate using the comprehensive system
func (ds *DebateService) conductComprehensiveDebateStreaming(
	ctx context.Context,
	config *DebateConfig,
	startTime time.Time,
	sessionID string,
	streamHandler comprehensive.StreamHandler,
) (*DebateResult, error) {
	ds.logger.WithFields(logrus.Fields{
		"debate_id": config.DebateID,
		"topic":     config.Topic,
	}).Info("[Comprehensive Debate] Starting streaming multi-agent debate")

	// Convert service DebateConfig to comprehensive DebateStreamRequest
	compReq := &comprehensive.DebateStreamRequest{
		DebateRequest: &comprehensive.DebateRequest{
			ID:        config.DebateID,
			Topic:     config.Topic,
			Context:   config.Topic,
			Language:  "go",
			MaxRounds: 3,
		},
		Stream:        true,
		StreamHandler: streamHandler,
	}

	// Execute streaming debate through comprehensive system
	compResp, err := ds.comprehensiveIntegration.StreamDebateRequest(ctx, compReq)
	if err != nil {
		ds.logger.WithError(err).Error("[Comprehensive Debate] Streaming debate execution failed")
		return nil, fmt.Errorf("comprehensive streaming debate failed: %w", err)
	}

	endTime := time.Now()

	// Build participant responses from comprehensive agents
	participants := make([]ParticipantResponse, 0, len(compResp.Participants))
	for _, agentID := range compResp.Participants {
		// Note: In a full implementation, we'd look up the actual agent details
		participants = append(participants, ParticipantResponse{
			ParticipantID: agentID,
			Response:      "Contributed to comprehensive debate",
			Timestamp:     endTime,
		})
	}

	// Convert comprehensive response to service DebateResult
	result := &DebateResult{
		DebateID:        config.DebateID,
		SessionID:       sessionID,
		Topic:           config.Topic,
		StartTime:       startTime,
		EndTime:         endTime,
		Duration:        endTime.Sub(startTime),
		TotalRounds:     compResp.RoundsConducted,
		RoundsConducted: compResp.RoundsConducted,
		Participants:    participants,
		AllResponses:    participants,
		Consensus: &ConsensusResult{
			Reached:        compResp.Success,
			Achieved:       compResp.Success,
			Confidence:     compResp.QualityScore,
			AgreementLevel: compResp.QualityScore,
			FinalPosition:  fmt.Sprintf("Comprehensive streaming debate completed with %d rounds", compResp.RoundsConducted),
			Summary:        fmt.Sprintf("Comprehensive streaming debate completed with %d rounds", compResp.RoundsConducted),
			KeyPoints:      []string{},
			Disagreements:  []string{},
			Timestamp:      endTime,
			QualityScore:   compResp.QualityScore,
		},
		QualityScore: compResp.QualityScore,
		FinalScore:   compResp.QualityScore,
		Success:      compResp.Success,
		Metadata: map[string]any{
			"comprehensive_debate": true,
			"streaming":            true,
			"rounds_conducted":     compResp.RoundsConducted,
			"phases":               len(compResp.Phases),
			"participants":         len(compResp.Participants),
		},
	}

	ds.logger.WithFields(logrus.Fields{
		"debate_id":     config.DebateID,
		"success":       result.Success,
		"total_rounds":  result.TotalRounds,
		"quality_score": result.QualityScore,
		"duration":      result.Duration,
		"participants":  len(participants),
	}).Info("[Comprehensive Debate] Streaming multi-agent debate completed")

	return result, nil
}
