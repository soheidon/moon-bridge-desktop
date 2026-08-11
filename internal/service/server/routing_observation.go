package server

import (
	"context"

	"moonbridge/internal/config"
	"moonbridge/internal/protocol/anthropic"
	"moonbridge/internal/service/provider"
	"moonbridge/internal/service/trafficanalysis"
)

func (s *Server) recordRoutingResolved(ctx context.Context, requested string, candidate provider.ProviderCandidate) {
	if s.routingObservationSink == nil {
		return
	}
	configured := ""
	if candidate.ReasoningOverride != nil {
		configured = *candidate.ReasoningOverride
	}
	s.routingObservationSink.RecordGatewayEvent(trafficanalysis.GatewayEventInput{
		Kind:             trafficanalysis.ObservationRoutingResolved,
		CorrelationKey:   routingObservationKey(ctx),
		ProfileID:        candidate.RoutingProfileID,
		RequestedModel:   requested,
		RoutingSlot:      candidate.RoutingSlot,
		Provider:         candidate.ProviderKey,
		UpstreamModel:    candidate.UpstreamModel,
		Mode:             candidate.ReasoningMode,
		ConfiguredEffort: configured,
	})
}

func (s *Server) recordProviderRequestPrepared(ctx context.Context, requested string, candidate provider.ProviderCandidate, upstream any) {
	if s.routingObservationSink == nil {
		return
	}
	event := trafficanalysis.GatewayEventInput{
		Kind:            trafficanalysis.ObservationProviderRequestPrepared,
		CorrelationKey:  routingObservationKey(ctx),
		ProfileID:       candidate.RoutingProfileID,
		RequestedModel:  requested,
		RoutingSlot:     candidate.RoutingSlot,
		Provider:        candidate.ProviderKey,
		Protocol:        candidate.Protocol,
		Model:           candidate.UpstreamModel,
		Mode:            candidate.ReasoningMode,
		Thinking:        "not_applicable",
		EffectiveEffort: "",
	}
	if candidate.ReasoningOverride != nil {
		event.ConfiguredEffort = *candidate.ReasoningOverride
	}
	switch request := upstream.(type) {
	case *anthropic.MessageRequest:
		event.Protocol = config.ProtocolAnthropic
		event.Model = request.Model
		event.Thinking = "none"
		if request.Thinking != nil {
			event.Thinking = request.Thinking.Type
		}
		if request.OutputConfig != nil {
			event.EffectiveEffort = request.OutputConfig.Effort
		}
	case anthropic.MessageRequest:
		event.Protocol = config.ProtocolAnthropic
		event.Model = request.Model
		event.Thinking = "none"
		if request.Thinking != nil {
			event.Thinking = request.Thinking.Type
		}
		if request.OutputConfig != nil {
			event.EffectiveEffort = request.OutputConfig.Effort
		}
	}
	if event.UpstreamModel == "" {
		event.UpstreamModel = event.Model
	}
	s.routingObservationSink.RecordGatewayEvent(event)
}
