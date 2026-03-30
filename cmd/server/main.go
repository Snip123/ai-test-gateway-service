package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/joho/godotenv"
)

// upstreamEnvKeys maps path prefixes to the env var holding the upstream URL (ADR-0011).
var upstreamEnvKeys = map[string]string{
	"/v1/assets":       "UPSTREAM_ASSETS_URL",
	"/v1/workorders":   "UPSTREAM_WORKORDERS_URL",
	"/v1/pm":           "UPSTREAM_PM_URL",
	"/v1/pm-schedules": "UPSTREAM_PMSCHEDULES_URL",
	"/v1/floorplans":   "UPSTREAM_FLOORPLANS_URL",
	"/v1/admin":        "UPSTREAM_TENANTADMIN_URL",
	"/v1/analytics":    "UPSTREAM_ANALYTICS_URL",
}

func main() {
	_ = godotenv.Load() // load .env if present (local dev only — no-op in production)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	port := env("PORT", "8000")
	environment := env("ENVIRONMENT", "local")
	authDisabled := os.Getenv("AUTH_DISABLED") == "true"

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(corsMiddleware)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})

	for prefix, envKey := range upstreamEnvKeys {
		upstreamURL := os.Getenv(envKey)
		if upstreamURL == "" {
			log.Printf("WARN: %s not set — %s will return 503", envKey, prefix)
			continue
		}
		upstream, err := url.Parse(upstreamURL)
		if err != nil {
			return fmt.Errorf("parse %s: %w", envKey, err)
		}
		proxy := httputil.NewSingleHostReverseProxy(upstream)
		mw := gatewayMiddleware(environment, authDisabled)
		p := prefix
		r.Handle(p, mw(proxy))
		r.Handle(p+"/*", mw(proxy))
	}

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("ai-test-gateway-service :%s  env=%s  auth_disabled=%v", port, environment, authDisabled)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		return srv.Shutdown(shutCtx)
	case err := <-errCh:
		return err
	}
}

// gatewayMiddleware resolves Tenant and injects standard headers before proxying (ADR-0011).
func gatewayMiddleware(environment string, authDisabled bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Tenant resolution (ADR-0004)
			tenantID := resolveTenant(r, environment)
			if tenantID == "" {
				writeProblem(w, http.StatusBadRequest, "missing-tenant",
					"Cannot Resolve Tenant",
					"Tenant could not be resolved from subdomain or X-Tenant-ID header")
				return
			}

			// JWT validation (ADR-0008)
			if !authDisabled {
				userID, role, err := validateJWT(r)
				if err != nil {
					writeProblem(w, http.StatusUnauthorized, "invalid-jwt", "Unauthorized", err.Error())
					return
				}
				r.Header.Set("X-User-ID", userID)
				r.Header.Set("X-Platform-Role", role)
			} else {
				// Local dev defaults — override with explicit headers in requests
				if r.Header.Get("X-User-ID") == "" {
					r.Header.Set("X-User-ID", "dev-user")
				}
				if r.Header.Get("X-Platform-Role") == "" {
					r.Header.Set("X-Platform-Role", "FacilityManager")
				}
			}

			r.Header.Set("X-Tenant-ID", tenantID)
			next.ServeHTTP(w, r)
		})
	}
}

// resolveTenant returns the tenant ID from subdomain (prod) or X-Tenant-ID header (local).
func resolveTenant(r *http.Request, environment string) string {
	if environment == "local" {
		return r.Header.Get("X-Tenant-ID")
	}
	host := r.Host
	if i := strings.Index(host, "."); i > 0 {
		return host[:i]
	}
	return ""
}

// validateJWT validates the Bearer token and returns userID + platformRole.
// TODO: implement full JWKS validation against Keycloak (ADR-0008).
// Use github.com/lestrrat-go/jwx/v2 with cached JWKS from:
// {KEYCLOAK_BASE_URL}/realms/{tenantID}/protocol/openid-connect/certs
func validateJWT(r *http.Request) (userID, role string, err error) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return "", "", fmt.Errorf("missing or malformed Authorization header")
	}
	// Placeholder — replace with real JWKS validation
	return "usr-placeholder", "FacilityManager", nil
}

func writeProblem(w http.ResponseWriter, status int, errType, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"type":"https://cmms-platform.example/errors/%s","title":%q,"status":%d,"detail":%q}`,
		errType, title, status, detail)
}

// corsMiddleware allows Flutter web (and other browser clients) to call the gateway.
// Permissive in local dev; production should restrict AllowedOrigins to known domains.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			origin = "*"
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Tenant-ID, X-Request-ID")
		w.Header().Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
