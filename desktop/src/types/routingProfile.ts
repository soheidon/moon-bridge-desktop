// Routing profile DTO — mirrors the Wails DesktopSnapshot.routingProfiles
// payload. The backend sources activeProfileId from routing_profiles
// config.active_profile (the route provider only as a bootstrap fallback when
// the extension is absent); it is never a local/optimistic selection. Slots
// carry no persistent active state.

export const ROUTING_SLOT_SOL = "sol";
export const ROUTING_SLOT_TERRA = "terra";
export const ROUTING_SLOT_LUNA = "luna";

export type RoutingSlotId = "sol" | "terra" | "luna";
export type RoutingMode = "normal" | "thinking";
export type RoutingReasoning = "low" | "high" | "max";

export interface RoutingModelCapability {
  modelId: string;
  supportedReasoning: RoutingReasoning[];
  defaultReasoning?: RoutingReasoning;
}

export interface RoutingSlot {
  id: RoutingSlotId;
  displayName: string; // Sol / Terra / Luna
  providerId: string;
  providerLabel: string;
  upstreamModel: string;
  mode?: RoutingMode;
  // undefined when the slot carries no reasoning override (Luna).
  reasoning?: RoutingReasoning;
}

export interface RoutingProfileCard {
  id: string;
  displayName: string;
  active: boolean; // backend-confirmed active profile (config.active_profile)
  configured: boolean;
  slots: RoutingSlot[];
}

// ActiveProfileID is "" on the wire when no profile is active.
export interface RoutingProfileSnapshot {
  profiles: RoutingProfileCard[];
  activeProfileId: string;
  gatewayRunning: boolean;
}

export interface RoutingSlotInput {
  provider: string;
  upstreamModel: string;
  mode?: RoutingMode;
  reasoning?: RoutingReasoning | null; // null = no override (normal)
}

export interface RoutingProfileInput {
  id: string;
  displayName: string;
  slots: Record<RoutingSlotId, RoutingSlotInput>;
}
