package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

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

type RateLimitTier struct {
	RequestsPerMinute int
	WindowSeconds     int
}

// AuthManager handles API key authentication and distributed rate limiting.
type AuthManager struct {
	mu          sync.RWMutex
	apiKeys     map[string]*ClientConfig
	rateLimiter *RedisRateLimiter
	tiers       map[string]RateLimitTier
}

func NewAuthManager(redisClient *redis.Client) *AuthManager {
	am := &AuthManager{
		apiKeys:     make(map[string]*ClientConfig),
		rateLimiter: NewRedisRateLimiter(redisClient, "ratelimit"),
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

	return am
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

// Authenticate validates an API key and returns its configuration.
func (am *AuthManager) Authenticate(apiKey string) (*ClientConfig, error) {
	am.mu.RLock()
	defer am.mu.RUnlock()

	config, exists := am.apiKeys[apiKey]
	if !exists {
		return nil, fmt.Errorf("invalid API key")
	}

	return config, nil
}

// CheckRateLimit verifies if a request is within the rate limit for the given API key.
func (am *AuthManager) CheckRateLimit(ctx context.Context, apiKey string) error {
	am.mu.RLock()
	config, exists := am.apiKeys[apiKey]
	am.mu.RUnlock()

	if !exists {
		return fmt.Errorf("API key not found")
	}

	tier := am.tiers[config.Tier]

	allowed, err := am.rateLimiter.IsAllowed(
		ctx,
		apiKey,
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

// GetRateLimitInfo returns the remaining requests and total limit for an API key.
func (am *AuthManager) GetRateLimitInfo(ctx context.Context, apiKey string) (remaining int64, limit int, err error) {
	am.mu.RLock()
	config, exists := am.apiKeys[apiKey]
	am.mu.RUnlock()

	if !exists {
		return 0, 0, fmt.Errorf("API key not found")
	}

	tier := am.tiers[config.Tier]

	remaining, err = am.rateLimiter.GetRemaining(
		ctx,
		apiKey,
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
		Description: "Echo back a message (requires authentication)",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"api_key": map[string]any{
					"type":        "string",
					"description": "API key for authentication",
				},
				"message": map[string]any{
					"type":        "string",
					"description": "Message to echo",
				},
			},
			"required": []string{"api_key", "message"},
		},
	}, s.handleEchoTool)

	s.mcpServer.AddTool(&mcp.Tool{
		Name:        "get_time",
		Description: "Get the current server time (requires authentication)",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"api_key": map[string]any{
					"type":        "string",
					"description": "API key for authentication",
				},
			},
			"required": []string{"api_key"},
		},
	}, s.handleGetTimeTool)

	s.mcpServer.AddTool(&mcp.Tool{
		Name:        "rate_limit_status",
		Description: "Check your current rate limit status",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"api_key": map[string]any{
					"type":        "string",
					"description": "API key for authentication",
				},
			},
			"required": []string{"api_key"},
		},
	}, s.handleRateLimitStatusTool)
}

// authenticateAndCheckRateLimit performs authentication and rate limit checks.
func (s *AuthenticatedMCPServer) authenticateAndCheckRateLimit(ctx context.Context, apiKey string) (*ClientConfig, error) {
	clientConfig, err := s.authManager.Authenticate(apiKey)
	if err != nil {
		log.Printf("Authentication failed: %v", err)
		return nil, fmt.Errorf("authentication failed: %w", err)
	}

	if err := s.authManager.CheckRateLimit(ctx, apiKey); err != nil {
		log.Printf("Rate limit exceeded for %s: %v", clientConfig.Name, err)
		return nil, err
	}

	return clientConfig, nil
}

func (s *AuthenticatedMCPServer) handleEchoTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var arguments map[string]any
	if err := json.Unmarshal(req.Params.Arguments, &arguments); err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "Invalid arguments"}},
			IsError: true,
		}, nil
	}

	apiKey, ok := arguments["api_key"].(string)
	if !ok {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "API key is required"}},
			IsError: true,
		}, nil
	}

	clientConfig, err := s.authenticateAndCheckRateLimit(ctx, apiKey)
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

	remaining, limit, _ := s.authManager.GetRateLimitInfo(ctx, apiKey)

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

	apiKey, ok := arguments["api_key"].(string)
	if !ok {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "API key is required"}},
			IsError: true,
		}, nil
	}

	clientConfig, err := s.authenticateAndCheckRateLimit(ctx, apiKey)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Error: %v", err)}},
			IsError: true,
		}, nil
	}

	remaining, limit, _ := s.authManager.GetRateLimitInfo(ctx, apiKey)

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

	apiKey, ok := arguments["api_key"].(string)
	if !ok {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "API key is required"}},
			IsError: true,
		}, nil
	}

	// Only authenticate, don't check rate limit for status queries
	clientConfig, err := s.authManager.Authenticate(apiKey)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Error: %v", err)}},
			IsError: true,
		}, nil
	}

	remaining, limit, err := s.authManager.GetRateLimitInfo(ctx, apiKey)
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

// Handler returns an HTTP handler for streamable MCP sessions.
func (s *AuthenticatedMCPServer) Handler() http.Handler {
	return mcp.NewStreamableHTTPHandler(
		func(req *http.Request) *mcp.Server {
			// Return the same server instance for all requests
			// The MCP server handles multiple concurrent sessions
			return s.mcpServer
		},
		&mcp.StreamableHTTPOptions{
			SessionTimeout: 30 * time.Minute,
		},
	)
}

// Run starts the HTTP server on the given address.
func (s *AuthenticatedMCPServer) Run(addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/mcp", s.Handler())

	log.Printf("MCP server listening on http://%s/mcp", addr)
	return http.ListenAndServe(addr, mux)
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
	log.Println("Available API keys:")
	log.Println("  - Free tier: key_free_abc123 (10 req/min)")
	log.Println("  - Pro tier: key_pro_xyz789 (100 req/min)")
	log.Println("  - Enterprise: key_ent_def456 (1000 req/min)")

	if err := server.Run(httpAddr); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
