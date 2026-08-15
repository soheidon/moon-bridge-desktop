package server

import (
	"context"

	"moonbridge/internal/config"
	"moonbridge/internal/protocol/anthropic"
	"moonbridge/internal/protocol/openai"
	"moonbridge/internal/service/egressobservation"
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
		CredentialState:  credentialStateFor(s.activeProviderManager(), candidate.ProviderKey),
	})
}

func (s *Server) recordProviderEgress(event egressobservation.Event) {
	if s.routingObservationSink == nil {
		return
	}
	kind := trafficanalysis.ObservationProviderRequestDispatched
	direction := trafficanalysis.DirectionClientToUpstream
	switch event.Kind {
	case egressobservation.ResponseReceived:
		kind = trafficanalysis.ObservationProviderResponseReceived
		direction = trafficanalysis.DirectionUpstreamToClient
	case egressobservation.ResponseForwarded:
		kind = trafficanalysis.ObservationProviderResponseForwarded
		direction = trafficanalysis.DirectionUpstreamToClient
	}
	s.routingObservationSink.RecordGatewayEvent(trafficanalysis.GatewayEventInput{
		Kind: kind, CorrelationKey: event.Correlation, ProfileID: event.ProfileID,
		RequestedModel: event.RequestedModel, RoutingSlot: event.RoutingSlot, Mode: event.Mode,
		Provider: event.Provider, UpstreamModel: event.UpstreamModel, ConfiguredEffort: event.ConfiguredEffort,
		CredentialState: event.CredentialState,
		Protocol:        event.Protocol, Model: event.Model, Thinking: event.Thinking, EffectiveEffort: event.EffectiveEffort,
		Direction:  direction,
		StatusCode: event.StatusCode, ExchangeIndex: event.ExchangeIndex, Streaming: event.Streaming,
	})
}

func (s *Server) withProviderEgress(ctx context.Context, requested string, candidate provider.ProviderCandidate, streaming bool, upstream any) context.Context {
	event := egressobservation.Event{
		Correlation: routingObservationKey(ctx), ProfileID: candidate.RoutingProfileID,
		RequestedModel: requested, RoutingSlot: candidate.RoutingSlot, Mode: candidate.ReasoningMode,
		Provider: candidate.ProviderKey, UpstreamModel: candidate.UpstreamModel,
		ConfiguredEffort: reasoningOverride(candidate), CredentialState: credentialStateFor(s.activeProviderManager(), candidate.ProviderKey), Protocol: string(candidate.Protocol), Model: candidate.UpstreamModel,
		Streaming: streaming,
	}
	applyEgressRequestDetails(&event, upstream)
	return egressobservation.WithMetadata(ctx, event, s.recordProviderEgress)
}

func reasoningOverride(candidate provider.ProviderCandidate) string {
	if candidate.ReasoningOverride == nil {
		return ""
	}
	return *candidate.ReasoningOverride
}

func applyEgressRequestDetails(event *egressobservation.Event, upstream any) {
	switch request := upstream.(type) {
	case *anthropic.MessageRequest:
		event.Model = request.Model
		event.Thinking = "none"
		if request.Thinking != nil {
			event.Thinking = request.Thinking.Type
		}
		if request.OutputConfig != nil {
			event.EffectiveEffort = request.OutputConfig.Effort
		}
	case anthropic.MessageRequest:
		copy := request
		applyEgressRequestDetails(event, &copy)
	case *openai.ResponsesRequest:
		event.Model = request.Model
		event.Thinking = "none"
		if effort, ok := request.Reasoning["effort"].(string); ok {
			event.Thinking = "enabled"
			event.EffectiveEffort = effort
		}
	}
	if event.Model == "" {
		event.Model = event.UpstreamModel
	}
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
		CredentialState: credentialStateFor(s.activeProviderManager(), candidate.ProviderKey),
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
