package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/redis/go-redis/v9"
)

// RedisRateLimiter implements distributed rate limiting using a Redis sliding window algorithm.
//
// Redis Sorted Sets (ZSET) are used to track requests within a time window:
// - Each request is a member with a unique identifier
// - The timestamp is stored as the score for efficient time-based queries
// - ZRemRangeByScore removes expired requests older than the window
// - ZCard counts current requests in the window
// - ZAdd records new requests atomically
//
// This approach allows accurate rate limiting across distributed systems without
// a central counter that could become a bottleneck.
type RedisRateLimiter struct {
	client *redis.Client
	prefix string
}

func NewRedisRateLimiter(client *redis.Client, prefix string) *RedisRateLimiter {
	return &RedisRateLimiter{
		client: client,
		prefix: prefix,
	}
}

// IsAllowed checks if a request is within rate limits using a sliding window.
// It ensures requests are only added to Redis if they're within the limit to prevent
// rejected requests from being counted.
func (r *RedisRateLimiter) IsAllowed(ctx context.Context, key string, maxRequests int, windowSeconds int) (bool, error) {
	now := time.Now().Unix()
	windowStart := now - int64(windowSeconds)
	redisKey := fmt.Sprintf("%s:%s", r.prefix, key)

	// Remove expired entries and count current requests
	cleanupPipeline := r.client.Pipeline()
	cleanupPipeline.ZRemRangeByScore(ctx, redisKey, "0", fmt.Sprintf("%d", windowStart))
	countCmd := cleanupPipeline.ZCard(ctx, redisKey)

	_, err := cleanupPipeline.Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("redis pipeline error: %w", err)
	}

	requestCount := countCmd.Val()

	if requestCount >= int64(maxRequests) {
		return false, nil
	}

	// Record the new request with TTL
	recordPipeline := r.client.Pipeline()
	recordPipeline.ZAdd(ctx, redisKey, redis.Z{
		Score:  float64(now),
		Member: fmt.Sprintf("%d-%d", now, time.Now().UnixNano()),
	})
	recordPipeline.Expire(ctx, redisKey, time.Duration(windowSeconds)*time.Second)

	_, err = recordPipeline.Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("redis error adding request: %w", err)
	}

	return true, nil
}

// GetRemaining returns the number of requests remaining in the current window.
func (r *RedisRateLimiter) GetRemaining(ctx context.Context, key string, maxRequests int, windowSeconds int) (int64, error) {
	now := time.Now().Unix()
	windowStart := now - int64(windowSeconds)
	redisKey := fmt.Sprintf("%s:%s", r.prefix, key)

	// Clean expired entries and count remaining quota
	queryPipeline := r.client.Pipeline()
	queryPipeline.ZRemRangeByScore(ctx, redisKey, "0", fmt.Sprintf("%d", windowStart))
	countCmd := queryPipeline.ZCard(ctx, redisKey)

	_, err := queryPipeline.Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("redis error: %w", err)
	}

	currentCount := countCmd.Val()
	remaining := max(0, int64(maxRequests)-currentCount)

	return remaining, nil
}

type ClientConfig struct {
	Name      string
	Tier      string
	RateLimit int // requests per minute
}

// contextKey is used for storing values in context.
type contextKey string

const authHeaderKey contextKey = "authHeader"

// OAuth2Validator handles JWT validation using OIDC discovery.
type OAuth2Validator struct {
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	clientID string
}

// TokenClaims represents the claims extracted from a JWT token.
type TokenClaims struct {
	Subject string `json:"sub"`
	Email   string `json:"email"`
	Tier    string `json:"tier"` // Custom claim for rate limiting tier
}

// NewOAuth2Validator creates a new OAuth2Validator using OIDC discovery.
func NewOAuth2Validator(ctx context.Context, issuerURL, clientID string) (*OAuth2Validator, error) {
	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, fmt.Errorf("failed to create OIDC provider: %w", err)
	}

	verifier := provider.Verifier(&oidc.Config{
		ClientID: clientID,
	})

	return &OAuth2Validator{
		provider: provider,
		verifier: verifier,
		clientID: clientID,
	}, nil
}

// Validate verifies a JWT token and extracts claims.
// Returns ClientConfig, rate limit key (oauth2:<subject>), and error.
func (v *OAuth2Validator) Validate(ctx context.Context, rawToken string) (*ClientConfig, string, error) {
	idToken, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return nil, "", fmt.Errorf("token verification failed: %w", err)
	}

	var claims TokenClaims
	if err := idToken.Claims(&claims); err != nil {
		return nil, "", fmt.Errorf("failed to parse claims: %w", err)
	}

	// Default to "free" tier if not specified
	tier := claims.Tier
	if tier == "" {
		tier = "free"
	}

	clientName := claims.Email
	if clientName == "" {
		clientName = claims.Subject
	}

	config := &ClientConfig{
		Name: clientName,
		Tier: tier,
	}

	rateLimitKey := fmt.Sprintf("oauth2:%s", claims.Subject)

	return config, rateLimitKey, nil
}

type RateLimitTier struct {
	RequestsPerMinute int
	WindowSeconds     int
}

// AuthManager handles API key and OAuth2 authentication with distributed rate limiting.
type AuthManager struct {
	mu              sync.RWMutex
	apiKeys         map[string]*ClientConfig
	rateLimiter     *RedisRateLimiter
	tiers           map[string]RateLimitTier
	oauth2Validator *OAuth2Validator
	oauth2Enabled   bool
}

func NewAuthManager(redisClient *redis.Client) *AuthManager {
	am := &AuthManager{
		apiKeys:       make(map[string]*ClientConfig),
		rateLimiter:   NewRedisRateLimiter(redisClient, "ratelimit"),
		oauth2Enabled: false,
		tiers: map[string]RateLimitTier{
			"free": {
				RequestsPerMinute: 10,
				WindowSeconds:     60,
			},
			"pro": {
				RequestsPerMinute: 100,
				WindowSeconds:     60,
			},
			"enterprise": {
				RequestsPerMinute: 1000,
				WindowSeconds:     60,
			},
		},
	}

	// Example API keys for demonstration
	am.AddAPIKey("key_free_abc123", &ClientConfig{
		Name: "Free Tier Client",
		Tier: "free",
	})
	am.AddAPIKey("key_pro_xyz789", &ClientConfig{
		Name: "Pro Tier Client",
		Tier: "pro",
	})
	am.AddAPIKey("key_ent_def456", &ClientConfig{
		Name: "Enterprise Client",
		Tier: "enterprise",
	})

	// Initialize OAuth2 validator
	am.initOAuth2()

	return am
}

// initOAuth2 initializes the OAuth2 validator using environment configuration.
func (am *AuthManager) initOAuth2() {
	keycloakURL := os.Getenv("KEYCLOAK_URL")
	if keycloakURL == "" {
		keycloakURL = "http://localhost:8180"
	}
	keycloakRealm := os.Getenv("KEYCLOAK_REALM")
	if keycloakRealm == "" {
		keycloakRealm = "mcp-demo"
	}
	clientID := os.Getenv("OAUTH2_CLIENT_ID")
	if clientID == "" {
		clientID = "mcp-client"
	}

	issuerURL := fmt.Sprintf("%s/realms/%s", keycloakURL, keycloakRealm)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	validator, err := NewOAuth2Validator(ctx, issuerURL, clientID)
	if err != nil {
		log.Printf("Warning: OAuth2 validation unavailable - failed to initialize: %v", err)
		return
	}

	am.oauth2Validator = validator
	am.oauth2Enabled = true
	log.Printf("OAuth2 validation enabled with issuer: %s", issuerURL)
}

// SetOAuth2Validator allows setting a custom OAuth2 validator (for testing).
func (am *AuthManager) SetOAuth2Validator(validator *OAuth2Validator) {
	am.oauth2Validator = validator
	am.oauth2Enabled = validator != nil
}

// AddAPIKey registers an API key with its configuration. Unknown tiers default to "free".
func (am *AuthManager) AddAPIKey(apiKey string, config *ClientConfig) {
	am.mu.Lock()
	defer am.mu.Unlock()

	tier, exists := am.tiers[config.Tier]
	if !exists {
		tier = am.tiers["free"]
	}

	config.RateLimit = tier.RequestsPerMinute
	am.apiKeys[apiKey] = config
}

// Authenticate validates credentials (Bearer token or API key) and returns configuration.
// Returns ClientConfig, rate limit key, and error.
func (am *AuthManager) Authenticate(ctx context.Context, authHeader string) (*ClientConfig, string, error) {
	// Check for Bearer token first (OAuth2)
	if strings.HasPrefix(authHeader, "Bearer ") {
		if !am.oauth2Enabled || am.oauth2Validator == nil {
			return nil, "", fmt.Errorf("OAuth2 authentication is not enabled")
		}
		token := strings.TrimPrefix(authHeader, "Bearer ")
		return am.oauth2Validator.Validate(ctx, token)
	}

	// Fall back to API key authentication
	return am.authenticateAPIKey(authHeader)
}

// authenticateAPIKey validates an API key and returns its configuration.
func (am *AuthManager) authenticateAPIKey(apiKey string) (*ClientConfig, string, error) {
	am.mu.RLock()
	defer am.mu.RUnlock()

	config, exists := am.apiKeys[apiKey]
	if !exists {
		return nil, "", fmt.Errorf("invalid API key")
	}

	rateLimitKey := fmt.Sprintf("apikey:%s", apiKey)
	return config, rateLimitKey, nil
}

// CheckRateLimit verifies if a request is within the rate limit for the given client.
func (am *AuthManager) CheckRateLimit(ctx context.Context, rateLimitKey string, config *ClientConfig) error {
	tier, exists := am.tiers[config.Tier]
	if !exists {
		tier = am.tiers["free"]
	}

	// Set the rate limit on the config
	config.RateLimit = tier.RequestsPerMinute

	allowed, err := am.rateLimiter.IsAllowed(
		ctx,
		rateLimitKey,
		tier.RequestsPerMinute,
		tier.WindowSeconds,
	)
	if err != nil {
		return fmt.Errorf("rate limit check failed: %w", err)
	}

	if !allowed {
		return fmt.Errorf("rate limit exceeded: %d requests per %d seconds allowed for %s tier",
			tier.RequestsPerMinute, tier.WindowSeconds, config.Tier)
	}

	return nil
}

// GetRateLimitInfo returns the remaining requests and total limit for a client.
func (am *AuthManager) GetRateLimitInfo(ctx context.Context, rateLimitKey string, config *ClientConfig) (remaining int64, limit int, err error) {
	tier, exists := am.tiers[config.Tier]
	if !exists {
		tier = am.tiers["free"]
	}

	remaining, err = am.rateLimiter.GetRemaining(
		ctx,
		rateLimitKey,
		tier.RequestsPerMinute,
		tier.WindowSeconds,
	)
	if err != nil {
		return 0, 0, err
	}

	return remaining, tier.RequestsPerMinute, nil
}

// AuthenticatedMCPServer is an MCP server with API key authentication and rate limiting.
type AuthenticatedMCPServer struct {
	mcpServer   *mcp.Server
	authManager *AuthManager
	redisClient *redis.Client
}

func NewAuthenticatedMCPServer(redisAddr string) (*AuthenticatedMCPServer, error) {
	redisClient := redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Password: "",
		DB:       0,
	})

	ctx := context.Background()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	impl := &mcp.Implementation{
		Name:    "authenticated-mcp-server",
		Version: "1.0.0",
	}
	mcpServer := mcp.NewServer(impl, &mcp.ServerOptions{})

	server := &AuthenticatedMCPServer{
		mcpServer:   mcpServer,
		authManager: NewAuthManager(redisClient),
		redisClient: redisClient,
	}

	server.registerTools()

	return server, nil
}

func (s *AuthenticatedMCPServer) registerTools() {
	s.mcpServer.AddTool(&mcp.Tool{
		Name:        "echo",
		Description: "Echo back a message (requires authentication via Authorization header or api_key parameter)",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"api_key": map[string]any{
					"type":        "string",
					"description": "API key for authentication (optional if using Authorization header)",
				},
				"message": map[string]any{
					"type":        "string",
					"description": "Message to echo",
				},
			},
			"required": []string{"message"},
		},
	}, s.handleEchoTool)

	s.mcpServer.AddTool(&mcp.Tool{
		Name:        "get_time",
		Description: "Get the current server time (requires authentication via Authorization header or api_key parameter)",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"api_key": map[string]any{
					"type":        "string",
					"description": "API key for authentication (optional if using Authorization header)",
				},
			},
		},
	}, s.handleGetTimeTool)

	s.mcpServer.AddTool(&mcp.Tool{
		Name:        "rate_limit_status",
		Description: "Check your current rate limit status (requires authentication via Authorization header or api_key parameter)",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"api_key": map[string]any{
					"type":        "string",
					"description": "API key for authentication (optional if using Authorization header)",
				},
			},
		},
	}, s.handleRateLimitStatusTool)
}

// getAuthFromContext extracts the auth header or api_key from context and arguments.
// Returns the auth string (header value or api_key) and whether it's from the header.
func (s *AuthenticatedMCPServer) getAuthFromContext(ctx context.Context, arguments map[string]any) (string, bool) {
	// First, check for Authorization header in context
	if authHeader, ok := ctx.Value(authHeaderKey).(string); ok && authHeader != "" {
		return authHeader, true
	}

	// Fall back to api_key parameter
	if apiKey, ok := arguments["api_key"].(string); ok && apiKey != "" {
		return apiKey, false
	}

	return "", false
}

// authenticateAndCheckRateLimit performs authentication and rate limit checks.
// Returns ClientConfig, rate limit key, and error.
func (s *AuthenticatedMCPServer) authenticateAndCheckRateLimit(ctx context.Context, arguments map[string]any) (*ClientConfig, string, error) {
	auth, isHeader := s.getAuthFromContext(ctx, arguments)
	if auth == "" {
		return nil, "", fmt.Errorf("authentication required: provide Authorization header or api_key parameter")
	}

	var clientConfig *ClientConfig
	var rateLimitKey string
	var err error

	if isHeader {
		clientConfig, rateLimitKey, err = s.authManager.Authenticate(ctx, auth)
	} else {
		clientConfig, rateLimitKey, err = s.authManager.authenticateAPIKey(auth)
	}

	if err != nil {
		log.Printf("Authentication failed: %v", err)
		return nil, "", fmt.Errorf("authentication failed: %w", err)
	}

	if err := s.authManager.CheckRateLimit(ctx, rateLimitKey, clientConfig); err != nil {
		log.Printf("Rate limit exceeded for %s: %v", clientConfig.Name, err)
		return nil, "", err
	}

	return clientConfig, rateLimitKey, nil
}

func (s *AuthenticatedMCPServer) handleEchoTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var arguments map[string]any
	if err := json.Unmarshal(req.Params.Arguments, &arguments); err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Invalid arguments"}},
			IsError: true,
		}, nil
	}

	clientConfig, rateLimitKey, err := s.authenticateAndCheckRateLimit(ctx, arguments)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Error: %v", err)}},
			IsError: true,
		}, nil
	}

	message, ok := arguments["message"].(string)
	if !ok {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Message is required"}},
			IsError: true,
		}, nil
	}

	remaining, limit, _ := s.authManager.GetRateLimitInfo(ctx, rateLimitKey, clientConfig)

	responseText := fmt.Sprintf(
		"Echo from %s: %s\n\nRate Limit: %d/%d remaining",
		clientConfig.Name,
		message,
		remaining,
		limit,
	)

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: responseText}},
	}, nil
}

func (s *AuthenticatedMCPServer) handleGetTimeTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var arguments map[string]any
	if err := json.Unmarshal(req.Params.Arguments, &arguments); err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Invalid arguments"}},
			IsError: true,
		}, nil
	}

	clientConfig, rateLimitKey, err := s.authenticateAndCheckRateLimit(ctx, arguments)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Error: %v", err)}},
			IsError: true,
		}, nil
	}

	remaining, limit, _ := s.authManager.GetRateLimitInfo(ctx, rateLimitKey, clientConfig)

	responseText := fmt.Sprintf(
		"Current time: %s\nClient: %s\nRate Limit: %d/%d remaining",
		time.Now().Format(time.RFC3339),
		clientConfig.Name,
		remaining,
		limit,
	)

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: responseText}},
	}, nil
}

// handleRateLimitStatusTool returns rate limit information without consuming a request.
func (s *AuthenticatedMCPServer) handleRateLimitStatusTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var arguments map[string]any
	if err := json.Unmarshal(req.Params.Arguments, &arguments); err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Invalid arguments"}},
			IsError: true,
		}, nil
	}

	// Get auth from context or parameters
	auth, isHeader := s.getAuthFromContext(ctx, arguments)
	if auth == "" {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Authentication required: provide Authorization header or api_key parameter"}},
			IsError: true,
		}, nil
	}

	var clientConfig *ClientConfig
	var rateLimitKey string
	var err error

	if isHeader {
		clientConfig, rateLimitKey, err = s.authManager.Authenticate(ctx, auth)
	} else {
		clientConfig, rateLimitKey, err = s.authManager.authenticateAPIKey(auth)
	}

	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Error: %v", err)}},
			IsError: true,
		}, nil
	}

	// Set rate limit on config based on tier
	tier, exists := s.authManager.tiers[clientConfig.Tier]
	if !exists {
		tier = s.authManager.tiers["free"]
	}
	clientConfig.RateLimit = tier.RequestsPerMinute

	remaining, limit, err := s.authManager.GetRateLimitInfo(ctx, rateLimitKey, clientConfig)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Error: %v", err)}},
			IsError: true,
		}, nil
	}

	status := map[string]any{
		"client":    clientConfig.Name,
		"tier":      clientConfig.Tier,
		"limit":     limit,
		"remaining": remaining,
		"used":      limit - int(remaining),
	}

	statusJSON, _ := json.MarshalIndent(status, "", "  ")
	responseText := fmt.Sprintf("Rate Limit Status:\n%s", string(statusJSON))

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: responseText}},
	}, nil
}

func (s *AuthenticatedMCPServer) Close() error {
	return s.redisClient.Close()
}

// authMiddleware wraps an HTTP handler to inject the Authorization header into context.
func (s *AuthenticatedMCPServer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader != "" {
			ctx := context.WithValue(r.Context(), authHeaderKey, authHeader)
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

// Handler returns an HTTP handler for streamable MCP sessions.
func (s *AuthenticatedMCPServer) Handler() http.Handler {
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(req *http.Request) *mcp.Server {
			// Return the same server instance for all requests
			// The MCP server handles multiple concurrent sessions
			return s.mcpServer
		},
		&mcp.StreamableHTTPOptions{
			SessionTimeout: 30 * time.Minute,
		},
	)
	return s.authMiddleware(mcpHandler)
}

// Run starts the HTTP server on the given address.
func (s *AuthenticatedMCPServer) Run(addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/mcp", s.Handler())

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("MCP server listening on http://%s/mcp", addr)
	return server.ListenAndServe()
}

func main() {
	redisAddr := "localhost:6379"
	httpAddr := "localhost:8080"

	server, err := NewAuthenticatedMCPServer(redisAddr)
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}
	defer server.Close()

	log.Printf("Starting authenticated MCP server with Redis at %s", redisAddr)
	log.Println("Authentication methods:")
	log.Println("  1. API Key (via api_key parameter):")
	log.Println("     - Free tier: key_free_abc123 (10 req/min)")
	log.Println("     - Pro tier: key_pro_xyz789 (100 req/min)")
	log.Println("     - Enterprise: key_ent_def456 (1000 req/min)")
	if server.authManager.oauth2Enabled {
		log.Println("  2. OAuth2 (via Authorization: Bearer <token> header)")
		log.Println("     Token tier claim determines rate limit")
	} else {
		log.Println("  2. OAuth2: unavailable (Keycloak not reachable)")
	}

	if err := server.Run(httpAddr); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
