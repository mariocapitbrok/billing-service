package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/stripe/stripe-go/v76"
	"github.com/stripe/stripe-go/v76/billingportal/session"
	"github.com/stripe/stripe-go/v76/customer"
	"github.com/stripe/stripe-go/v76/webhook"
)

type Config struct {
	SecretKey     string
	WebhookSecret string
}

type StripeService struct {
	config Config
}

func NewStripeService(cfg Config) *StripeService {
	stripe.Key = cfg.SecretKey
	return &StripeService{
		config: cfg,
	}
}

// CreatePortalSession creates a billing portal session for a stripe customer ID.
func (s *StripeService) CreatePortalSession(ctx context.Context, stripeCustomerID string, returnURL string) (string, error) {
	if stripeCustomerID == "" {
		return "", fmt.Errorf("no stripe customer id provided")
	}

	params := &stripe.BillingPortalSessionParams{
		Customer:  stripe.String(stripeCustomerID),
		ReturnURL: stripe.String(returnURL),
	}

	sess, err := session.New(params)
	if err != nil {
		log.Printf("[StripeService] Failed to create portal session: %v", err)
		return "", err
	}

	return sess.URL, nil
}

// CreateCustomer creates a new Stripe customer.
func (s *StripeService) CreateCustomer(ctx context.Context, email, name string) (string, error) {
	params := &stripe.CustomerParams{
		Email: stripe.String(email),
		Name:  stripe.String(name),
	}
	c, err := customer.New(params)
	if err != nil {
		log.Printf("[StripeService] Failed to create customer: %v", err)
		return "", err
	}
	return c.ID, nil
}

// WebhookHandlers groups the DB callbacks used by HandleWebhook to stay DB-agnostic.
type WebhookHandlers struct {
	// InsertBillingEvent writes an idempotent billing_events row.
	// The implementation must use ON CONFLICT (stripe_event_id) DO NOTHING.
	InsertBillingEvent func(ctx context.Context, orgID, eventType, stripeEventID string, payload []byte) error

	// UpdateOrgStatus sets orgs.status for a given stripe_customer_id.
	UpdateOrgStatus func(ctx context.Context, stripeCustomerID, status string) error

	// UpdateOrgPlan syncs orgs.plan + stripe_subscription_id.
	UpdateOrgPlan func(ctx context.Context, stripeCustomerID, plan, stripeSubscriptionID string) error
}

// mapStripePlanNickname converts a Stripe plan nickname to an internal tier slug.
// Unknown values default to "starter" (logged as a warning).
func mapStripePlanNickname(nickname string) string {
	switch strings.ToLower(strings.TrimSpace(nickname)) {
	case "free":
		return "free"
	case "starter":
		return "starter"
	case "pro":
		return "pro"
	case "enterprise":
		return "enterprise"
	default:
		log.Printf("[StripeWebhook] Unknown plan nickname %q — defaulting to starter", nickname)
		return "starter"
	}
}

// HandleWebhook verifies the Stripe signature and dispatches lifecycle events.
// It is the caller's responsibility to pass a configured WebhookHandlers.
func (s *StripeService) HandleWebhook(payload []byte, signature string, handlers WebhookHandlers) error {
	event, err := webhook.ConstructEvent(payload, signature, s.config.WebhookSecret)
	if err != nil {
		return fmt.Errorf("failed to verify webhook signature: %w", err)
	}

	ctx := context.Background()

	// Helper: write audit record (idempotent).
	writeAudit := func(orgID, eventType string) {
		if handlers.InsertBillingEvent != nil {
			if aerr := handlers.InsertBillingEvent(ctx, orgID, eventType, event.ID, payload); aerr != nil {
				log.Printf("[StripeWebhook] billing_events insert failed: %v", aerr)
			}
		}
	}

	switch event.Type {

	case "invoice.paid":
		var inv stripe.Invoice
		if err := json.Unmarshal(event.Data.Raw, &inv); err != nil {
			return fmt.Errorf("parse invoice.paid: %w", err)
		}
		customerID := ""
		if inv.Customer != nil {
			customerID = inv.Customer.ID
		}
		subID := ""
		if inv.Subscription != nil {
			subID = inv.Subscription.ID
		}
		log.Printf("[StripeWebhook] invoice.paid customer=%s sub=%s", customerID, subID)
		if customerID != "" {
			if handlers.UpdateOrgPlan != nil {
				// invoice.paid → activate + sync subscription ID (plan unchanged here)
				if err := handlers.UpdateOrgPlan(ctx, customerID, "", subID); err != nil {
					log.Printf("[StripeWebhook] UpdateOrgPlan failed: %v", err)
				}
			}
			if handlers.UpdateOrgStatus != nil {
				if err := handlers.UpdateOrgStatus(ctx, customerID, "active"); err != nil {
					log.Printf("[StripeWebhook] UpdateOrgStatus(active) failed: %v", err)
				}
			}
		}
		writeAudit("", string(event.Type))

	case "invoice.payment_failed":
		var inv stripe.Invoice
		if err := json.Unmarshal(event.Data.Raw, &inv); err != nil {
			return fmt.Errorf("parse invoice.payment_failed: %w", err)
		}
		customerID := ""
		if inv.Customer != nil {
			customerID = inv.Customer.ID
		}
		log.Printf("[StripeWebhook] invoice.payment_failed customer=%s", customerID)
		if customerID != "" && handlers.UpdateOrgStatus != nil {
			if err := handlers.UpdateOrgStatus(ctx, customerID, "suspended"); err != nil {
				log.Printf("[StripeWebhook] UpdateOrgStatus(suspended) failed: %v", err)
			}
		}
		writeAudit("", string(event.Type))

	case "customer.subscription.deleted":
		var sub stripe.Subscription
		if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
			return fmt.Errorf("parse subscription.deleted: %w", err)
		}
		customerID := ""
		if sub.Customer != nil {
			customerID = sub.Customer.ID
		}
		log.Printf("[StripeWebhook] subscription.deleted customer=%s", customerID)
		if customerID != "" && handlers.UpdateOrgStatus != nil {
			if err := handlers.UpdateOrgStatus(ctx, customerID, "churned"); err != nil {
				log.Printf("[StripeWebhook] UpdateOrgStatus(churned) failed: %v", err)
			}
		}
		writeAudit("", string(event.Type))

	case "customer.subscription.created", "customer.subscription.updated":
		var sub stripe.Subscription
		if err := json.Unmarshal(event.Data.Raw, &sub); err != nil {
			return fmt.Errorf("parse subscription event: %w", err)
		}
		customerID := ""
		if sub.Customer != nil {
			customerID = sub.Customer.ID
		}
		planSlug := "starter"
		if sub.Items != nil && len(sub.Items.Data) > 0 {
			nickname := ""
			if sub.Items.Data[0].Price != nil && sub.Items.Data[0].Price.Nickname != "" {
				nickname = sub.Items.Data[0].Price.Nickname
			}
			planSlug = mapStripePlanNickname(nickname)
		}
		log.Printf("[StripeWebhook] %s customer=%s plan=%s", event.Type, customerID, planSlug)
		if customerID != "" && handlers.UpdateOrgPlan != nil {
			if err := handlers.UpdateOrgPlan(ctx, customerID, planSlug, sub.ID); err != nil {
				log.Printf("[StripeWebhook] UpdateOrgPlan failed: %v", err)
			}
		}
		writeAudit("", string(event.Type))

	default:
		log.Printf("[StripeWebhook] Unhandled event type: %s", event.Type)
	}

	return nil
}
