package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/mariocapitbrok/billing-service/internal/billing"
)

func main() {
	port := envOr("PORT", "8092")
	stripeSecret := os.Getenv("STRIPE_SECRET_KEY")
	stripeWebhookSecret := os.Getenv("STRIPE_WEBHOOK_SECRET")
	crmBackendURL := envOr("CRM_BACKEND_URL", "http://localhost:8080")
	internalAPISecret := os.Getenv("INTERNAL_API_SECRET")

	if stripeSecret == "" {
		log.Fatal("STRIPE_SECRET_KEY is required")
	}

	stripeSvc := billing.NewStripeService(billing.Config{
		SecretKey:     stripeSecret,
		WebhookSecret: stripeWebhookSecret,
	})

	mux := http.NewServeMux()

	// Create Customer
	mux.HandleFunc("/customers", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Email string `json:"email"`
			Name  string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		customerID, err := stripeSvc.CreateCustomer(r.Context(), req.Email, req.Name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		json.NewEncoder(w).Encode(map[string]string{"id": customerID})
	})

	// Create Portal Session
	mux.HandleFunc("/portal-sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			CustomerID string `json:"customer_id"`
			ReturnURL  string `json:"return_url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		url, err := stripeSvc.CreatePortalSession(r.Context(), req.CustomerID, req.ReturnURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		json.NewEncoder(w).Encode(map[string]string{"url": url})
	})

	// Stripe Webhook
	mux.HandleFunc("/webhooks/stripe", func(w http.ResponseWriter, r *http.Request) {
		const maxBodyBytes = int64(65536)
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Error reading request body", http.StatusServiceUnavailable)
			return
		}

		sigHeader := r.Header.Get("Stripe-Signature")
		
		handlers := billing.WebhookHandlers{
			InsertBillingEvent: func(ctx context.Context, orgID, eventType, stripeEventID string, rawPayload []byte) error {
				return notifyCRM(crmBackendURL, internalAPISecret, "billing.event", map[string]any{
					"stripe_customer_id": orgID,
					"event_type":         eventType,
					"stripe_event_id":    stripeEventID,
					"payload":            string(rawPayload),
				})
			},
			UpdateOrgStatus: func(ctx context.Context, stripeCustomerID, status string) error {
				return notifyCRM(crmBackendURL, internalAPISecret, "org.status", map[string]any{
					"stripe_customer_id": stripeCustomerID,
					"status":             status,
				})
			},
			UpdateOrgPlan: func(ctx context.Context, stripeCustomerID, plan, stripeSubscriptionID string) error {
				return notifyCRM(crmBackendURL, internalAPISecret, "org.plan", map[string]any{
					"stripe_customer_id":     stripeCustomerID,
					"plan":                   plan,
					"stripe_subscription_id": stripeSubscriptionID,
				})
			},
		}

		if err := stripeSvc.HandleWebhook(payload, sigHeader, handlers); err != nil {
			log.Printf("Webhook error: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		w.WriteHeader(http.StatusOK)
	})

	log.Printf("Billing service listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

func notifyCRM(baseURL, secret, action string, data map[string]any) error {
	url := fmt.Sprintf("%s/internal/billing-events", baseURL)
	
	body := map[string]any{
		"action": action,
		"data":   data,
	}
	
	jsonBody, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return err
	}
	
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-API-Secret", secret)
	
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("CRM returned error %d: %s", resp.StatusCode, string(b))
	}
	
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
