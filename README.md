# MCP Authentication & Rate Limiting Demo

[![Go CI](https://github.com/telton/mcp-authn-ratelimit-go/actions/workflows/go.yaml/badge.svg)](https://github.com/telton/mcp-authn-ratelimit-go/actions/workflows/go.yaml)
[![Go Report Card](https://goreportcard.com/badge/github.com/telton/mcp-authn-ratelimit-go)](https://goreportcard.com/report/github.com/telton/mcp-authn-ratelimit-go)

A demonstration of how to implement **authentication** (OAuth2/OIDC and API keys) and **distributed rate limiting** for [Model Context Protocol (MCP)](https://modelcontextprotocol.io/) servers in Go.

## Features

- **Dual Authentication Methods**
  - OAuth2/OIDC with JWT validation (via Keycloak or any OIDC provider)
  - API key authentication

- **Distributed Rate Limiting**
  - Redis-based sliding window algorithm
  - Per-client rate limits based on tier
  - Three tiers: `free` (10/min), `pro` (100/min), `enterprise` (1000/min)

- **MCP Server Integration**
  - Streamable HTTP transport
  - Authentication middleware
  - Three demo tools: `echo`, `get_time`, `rate_limit_status`

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                        HTTP Request                             │
│                 (Authorization header or api_key)               │
└─────────────────────────────────────────────────────────────────┘
                                │
                                ▼
┌─────────────────────────────────────────────────────────────────┐
│                      Auth Middleware                            │
│              (Extracts auth header into context)                │
└─────────────────────────────────────────────────────────────────┘
                                │
                                ▼
┌─────────────────────────────────────────────────────────────────┐
│                       AuthManager                               │
│  ┌─────────────────┐              ┌─────────────────────────┐   │
│  │ Bearer token?   │──── Yes ────▶│   OAuth2Validator       │   │
│  │                 │              │   (OIDC JWT validation) │   │
│  └─────────────────┘              └─────────────────────────┘   │
│          │ No                                                   │
│          ▼                                                      │
│  ┌─────────────────────────────────────────────────────────┐    │
│  │              API Key Lookup                             │    │
│  └─────────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────────┘
                                │
                                ▼
┌─────────────────────────────────────────────────────────────────┐
│                    RedisRateLimiter                             │
│           (Sliding window using Redis sorted sets)              │
│                                                                 │
│   Key: ratelimit:{oauth2|apikey}:{identifier}                   │
│   Score: Unix timestamp                                         │
│   Member: timestamp-nanoseconds (unique per request)            │
└─────────────────────────────────────────────────────────────────┘
                                │
                                ▼
┌─────────────────────────────────────────────────────────────────┐
│                       MCP Tools                                 │
│         (echo, get_time, rate_limit_status)                     │
└─────────────────────────────────────────────────────────────────┘
```

## Prerequisites

- Go 1.25+
- Docker and Docker Compose
- (Optional) An MCP client for testing

## Quick Start

### 1. Start Infrastructure

```bash
docker compose up -d
```

This starts:
- **Redis** on port `6379`
- **Keycloak** on port `8180`

### 2. Run the Server

```bash
go run main.go
```

The server starts on `http://localhost:8080/mcp`.

### 3. Test with API Keys

The server includes pre-configured API keys for testing:

| API Key | Tier | Rate Limit |
|---------|------|------------|
| `key_free_abc123` | free | 10 req/min |
| `key_pro_xyz789` | pro | 100 req/min |
| `key_ent_def456` | enterprise | 1000 req/min |

## Running Tests

### Run All Tests

```bash
go test -v ./...
```

Tests use [testcontainers](https://testcontainers.com/) to automatically spin up a Redis container.

### Run Specific Test Categories

```bash
# Rate limiter tests
go test -v -run TestRedisRateLimiter ./...

# Auth manager tests
go test -v -run TestAuthManager ./...

# MCP tool handler tests
go test -v -run TestHandle ./...

# Property-based tests
go test -v -run TestRateLimiter_Property ./...
```

### Run Benchmarks

```bash
go test -bench=. -benchmem ./...
```

## Keycloak Setup (OAuth2/OIDC)

### 1. Access Keycloak Admin Console

Open http://localhost:8180 and log in:
- Username: `admin`
- Password: `admin`

### 2. Create a Realm

1. Click the dropdown in the top-left (shows "master")
2. Click **Create realm**
3. Set **Realm name** to `mcp-demo`
4. Click **Create**

### 3. Create a Client

1. Go to **Clients** → **Create client**
2. Set:
   - **Client ID**: `mcp-client`
   - **Client type**: OpenID Connect
3. Click **Next**
4. Enable:
   - **Client authentication**: ON
   - **Authorization**: ON
5. Click **Next**, then **Save**

### 4. Configure Client Credentials

1. Go to **Clients** → `mcp-client` → **Credentials** tab
2. Note the **Client secret** (you'll need this to obtain tokens)

### 5. Create a User

1. Go to **Users** → **Add user**
2. Set:
   - **Username**: `testuser`
   - **Email**: `testuser@example.com`
   - **Email verified**: ON
3. Click **Create**
4. Go to **Credentials** tab → **Set password**
5. Set password and disable **Temporary**

### 6. Add Custom Tier Claim (Optional)

To include the `tier` claim in tokens:

1. Go to **Clients** → `mcp-client` → **Client scopes** tab
2. Click `mcp-client-dedicated`
3. Click **Add mapper** → **By configuration** → **User Attribute**
4. Configure:
   - **Name**: `tier`
   - **User Attribute**: `tier`
   - **Token Claim Name**: `tier`
   - **Claim JSON Type**: String
   - **Add to ID token**: ON
   - **Add to access token**: ON
5. Click **Save**

Then add the tier attribute to users:
1. Go to **Users** → select user → **Attributes** tab
2. Add attribute: `tier` = `pro` (or `free`, `enterprise`)

### 7. Obtain a Token

```bash
# Get an access token
curl -X POST "http://localhost:8180/realms/mcp-demo/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=password" \
  -d "client_id=mcp-client" \
  -d "client_secret=YOUR_CLIENT_SECRET" \
  -d "username=testuser" \
  -d "password=YOUR_PASSWORD"
```

## API Usage Examples

### Using API Key (via parameter)

```bash
# Initialize MCP session
curl -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -d '{
    "jsonrpc": "2.0",
    "id": 1,
    "method": "initialize",
    "params": {
      "protocolVersion": "2024-11-05",
      "capabilities": {},
      "clientInfo": {"name": "test", "version": "1.0"}
    }
  }'

# Call echo tool with API key
curl -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -H "Mcp-Session-Id: YOUR_SESSION_ID" \
  -d '{
    "jsonrpc": "2.0",
    "id": 2,
    "method": "tools/call",
    "params": {
      "name": "echo",
      "arguments": {
        "api_key": "key_pro_xyz789",
        "message": "Hello, MCP!"
      }
    }
  }'
```

### Using OAuth2 Bearer Token

```bash
curl -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer YOUR_ACCESS_TOKEN" \
  -H "Mcp-Session-Id: YOUR_SESSION_ID" \
  -d '{
    "jsonrpc": "2.0",
    "id": 2,
    "method": "tools/call",
    "params": {
      "name": "echo",
      "arguments": {
        "message": "Hello with OAuth2!"
      }
    }
  }'
```

### Check Rate Limit Status

```bash
curl -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -H "Mcp-Session-Id: YOUR_SESSION_ID" \
  -d '{
    "jsonrpc": "2.0",
    "id": 3,
    "method": "tools/call",
    "params": {
      "name": "rate_limit_status",
      "arguments": {
        "api_key": "key_pro_xyz789"
      }
    }
  }'
```

## Configuration

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `KEYCLOAK_URL` | `http://localhost:8180` | Keycloak server URL |
| `KEYCLOAK_REALM` | `mcp-demo` | Keycloak realm name |
| `OAUTH2_CLIENT_ID` | `mcp-client` | OAuth2 client ID for token validation |

### Rate Limit Tiers

Tiers are configured in `main.go`:

```go
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
```

## Project Structure

```
.
├── main.go              # Server implementation
├── main_test.go         # Comprehensive test suite
├── docker-compose.yaml  # Redis + Keycloak setup
├── go.mod               # Go module definition
├── go.sum               # Dependency checksums
└── README.md            # This file
```

## Key Components

### RedisRateLimiter

Implements sliding window rate limiting using Redis sorted sets:

- **ZRemRangeByScore**: Removes expired entries outside the window
- **ZCard**: Counts current requests in the window
- **ZAdd**: Records new requests with timestamp as score
- **Expire**: Sets TTL on the key for automatic cleanup

### OAuth2Validator

Validates JWT tokens using OIDC discovery:

- Fetches JWKS from the OIDC provider
- Validates token signature, expiration, and audience
- Extracts claims (`sub`, `email`, `tier`)

### AuthManager

Orchestrates authentication and rate limiting:

- Routes Bearer tokens to OAuth2 validation
- Falls back to API key lookup for other auth headers
- Enforces tier-based rate limits

## Known Limitations

1. **Non-atomic rate limiting**: Under extreme concurrent load to a single key, slight over-admission may occur due to the race window between count check and request recording. For strict rate limiting, consider using Redis Lua scripts.

2. **In-memory API keys**: Demo uses hardcoded API keys. In production, use a database or external service.

3. **No token refresh handling**: This is by design. In OAuth2 architecture, the MCP server acts as a **resource server** that only validates tokens - it never issues or refreshes them. Token refresh is the **client's responsibility**. When a token expires, the server returns an auth error, and the client should refresh with the authorization server (Keycloak) before retrying.

## Dependencies

| Package | Purpose |
|---------|---------|
| `github.com/modelcontextprotocol/go-sdk` | MCP server implementation |
| `github.com/redis/go-redis/v9` | Redis client |
| `github.com/coreos/go-oidc/v3` | OIDC/OAuth2 token validation |
| `github.com/testcontainers/testcontainers-go` | Test infrastructure |
| `pgregory.net/rapid` | Property-based testing |

## Specifications & References

### MCP (Model Context Protocol)

- [MCP Specification](https://spec.modelcontextprotocol.io/) - Full protocol specification
- [MCP Authentication](https://spec.modelcontextprotocol.io/specification/2025-03-26/basic/authentication/) - Authentication requirements for MCP servers
- [MCP Transports](https://spec.modelcontextprotocol.io/specification/2025-03-26/basic/transports/) - HTTP and Streamable HTTP transport specs
- [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk) - Official Go SDK for MCP

### OAuth 2.0 & OpenID Connect

- [RFC 6749 - OAuth 2.0](https://datatracker.ietf.org/doc/html/rfc6749) - OAuth 2.0 Authorization Framework
- [RFC 6750 - Bearer Tokens](https://datatracker.ietf.org/doc/html/rfc6750) - Bearer Token Usage
- [OpenID Connect Core 1.0](https://openid.net/specs/openid-connect-core-1_0.html) - OIDC specification
- [OpenID Connect Discovery](https://openid.net/specs/openid-connect-discovery-1_0.html) - OIDC Discovery specification
- [RFC 7519 - JWT](https://datatracker.ietf.org/doc/html/rfc7519) - JSON Web Token specification

### Rate Limiting

- [Redis Sorted Sets](https://redis.io/docs/data-types/sorted-sets/) - Data structure used for sliding window
- [Sliding Window Rate Limiting](https://blog.cloudflare.com/counting-things-a-lot-of-different-things/) - Cloudflare's explanation of rate limiting algorithms

### Related Tools

- [Keycloak Documentation](https://www.keycloak.org/documentation) - Identity and access management
- [JSON-RPC 2.0 Specification](https://www.jsonrpc.org/specification) - Protocol used by MCP

## License

MIT
