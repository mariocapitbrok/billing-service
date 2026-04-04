package billing

// PlanTier represents the subscription level of an organization.
type PlanTier string

const (
	PlanFree       PlanTier = "free"
	PlanStarter    PlanTier = "starter"
	PlanPro        PlanTier = "pro"
	PlanEnterprise PlanTier = "enterprise"
	PlanTrial      PlanTier = "trial"
)

// GetFeaturesForTier returns a slice of feature slugs that should be enabled for a given tier.
func GetFeaturesForTier(tier PlanTier) []string {
	// Base features for all tiers
	base := []string{"sales"}

	switch tier {
	case PlanFree:
		return base
	case PlanStarter:
		return append(base, "marketing")
	case PlanPro, PlanTrial:
		return append(base, "marketing", "telephony", "ai-scoring")
	case PlanEnterprise:
		return append(base, "marketing", "telephony", "ai-scoring", "sso-saml")
	default:
		return base
	}
}
