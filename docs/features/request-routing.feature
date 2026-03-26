Feature: Route Authenticated Requests to Upstream Services

  The Gateway resolves the Tenant from the request, validates the caller's JWT,
  injects standard headers, and proxies the request to the correct upstream service.
  No business logic lives here — the Gateway is a thin routing and auth enforcement layer.

  Background:
    Given the Gateway is running
    And the upstream Assets service is healthy

  # ─── Tenant resolution ────────────────────────────────────────────────────

  Scenario: Tenant is resolved from subdomain in production
    Given the request Host is "acme-corp.fsi-platform.com"
    And the environment is "production"
    When the request reaches the Gateway
    Then the Gateway sets X-Tenant-ID to "acme-corp" on the upstream request

  Scenario: Tenant is resolved from X-Tenant-ID header in local development
    Given the environment is "local"
    And the request includes header X-Tenant-ID "dev-tenant"
    When the request reaches the Gateway
    Then the Gateway sets X-Tenant-ID to "dev-tenant" on the upstream request

  Scenario: Request is rejected when Tenant cannot be resolved
    Given the environment is "production"
    And the request Host is "fsi-platform.com" with no subdomain
    When the request reaches the Gateway
    Then I receive a 400 Bad Request response
    And the response body contains error type "missing-tenant"
    And the response body contains title "Cannot Resolve Tenant"

  # ─── JWT validation ───────────────────────────────────────────────────────

  Scenario: Authenticated request is proxied with user context headers
    Given I am authenticated as Platform Role "FacilityManager" for Tenant "acme-corp"
    When I send a GET request to "/v1/assets"
    Then the Gateway sets X-User-ID on the upstream request
    And the Gateway sets X-Platform-Role to "FacilityManager" on the upstream request
    And the upstream Assets service receives the proxied request

  Scenario: Request with missing Authorization header is rejected
    Given the request has no Authorization header
    When I send a GET request to "/v1/assets"
    Then I receive a 401 Unauthorized response
    And the response body contains error type "invalid-jwt"

  Scenario: Request with malformed Bearer token is rejected
    Given the request Authorization header is "Bearer not-a-valid-token"
    When I send a GET request to "/v1/assets"
    Then I receive a 401 Unauthorized response
    And the response body contains error type "invalid-jwt"

  # ─── Upstream routing ─────────────────────────────────────────────────────

  Scenario Outline: Requests are routed to the correct upstream service by path prefix
    Given I am authenticated as Platform Role "FacilityManager" for Tenant "dev-tenant"
    When I send a GET request to "<path>"
    Then the Gateway proxies the request to the "<upstream>" upstream service

    Examples:
      | path                | upstream          |
      | /v1/assets          | Assets            |
      | /v1/assets/asset-1  | Assets            |
      | /v1/workorders      | Work Orders       |
      | /v1/pm              | Preventive Maintenance |
      | /v1/pm-schedules    | PM Schedules      |
      | /v1/floorplans      | Building Floorplans |
      | /v1/admin           | Tenant Admin      |
      | /v1/analytics       | Analytics         |

  Scenario: Upstream service unavailable returns 503
    Given the upstream Assets service is not configured
    And I am authenticated as Platform Role "FacilityManager" for Tenant "dev-tenant"
    When I send a GET request to "/v1/assets"
    Then I receive a 503 Service Unavailable response

  # ─── Health check ─────────────────────────────────────────────────────────

  Scenario: Health check endpoint is unauthenticated and always available
    When I send a GET request to "/healthz"
    Then I receive a 200 OK response
    And the response body is "ok"
