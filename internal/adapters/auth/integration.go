package auth

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"dev.helix.agent/internal/localfirst"
	genericoauth "digital.vasic.auth/pkg/oauth"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/sirupsen/logrus"
)

type APIKeyValidator struct {
	userService UserService
	logger      *logrus.Logger
}

type UserService interface {
	AuthenticateByAPIKey(ctx context.Context, apiKey string) (*User, error)
}

type User struct {
	ID       int
	Username string
	Role     string
}

func NewAPIKeyValidator(userService UserService, logger *logrus.Logger) *APIKeyValidator {
	return &APIKeyValidator{
		userService: userService,
		logger:      logger,
	}
}

func (v *APIKeyValidator) ValidateAPIKey(apiKey string) (bool, map[string]interface{}) {
	if v.userService == nil {
		return false, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	user, err := v.userService.AuthenticateByAPIKey(ctx, apiKey)
	if err != nil {
		return false, nil
	}

	return true, map[string]interface{}{
		"user_id":  user.ID,
		"username": user.Username,
		"role":     user.Role,
	}
}

func APIKeyAuthMiddleware(validator *APIKeyValidator, headerName string) gin.HandlerFunc {
	return func(c *gin.Context) {
		apiKey := c.GetHeader(headerName)
		if apiKey == "" {
			c.Next()
			return
		}

		valid, claims := validator.ValidateAPIKey(apiKey)
		if !valid {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error":   "unauthorized",
				"message": "Invalid API key",
			})
			c.Abort()
			return
		}

		for key, value := range claims {
			c.Set(key, value)
		}
		c.Set("authenticated", true)
		c.Set("auth_method", "api_key")

		c.Next()
	}
}

type OAuthCredentialManager struct {
	paths     map[string]string
	reader    genericoauth.CredentialReader
	refresher genericoauth.TokenRefresher
	auto      *genericoauth.AutoRefresher
	logger    *logrus.Logger
}

func NewOAuthCredentialManager(
	credentialPaths map[string]string,
	clientID string,
	logger *logrus.Logger,
) (*OAuthCredentialManager, error) {
	if len(credentialPaths) == 0 {
		return nil, fmt.Errorf("no credential paths provided")
	}

	reader := NewFileCredentialReader(credentialPaths)

	client := &http.Client{Timeout: 30 * time.Second}
	refresher := NewHTTPTokenRefresher(client, clientID, map[string]string{
		"grant_type": "refresh_token",
	})

	config := DefaultOAuthConfig()
	endpoints := make(map[string]string)
	for provider := range credentialPaths {
		endpoints[provider] = getTokenEndpoint(provider)
	}

	auto := NewAutoRefresher(reader, refresher, config, endpoints)

	return &OAuthCredentialManager{
		paths:     credentialPaths,
		reader:    reader,
		refresher: refresher,
		auto:      auto,
		logger:    logger,
	}, nil
}

func getTokenEndpoint(provider string) string {
	switch provider {
	case "claude":
		return "https://api.anthropic.com/oauth/token"
	case "qwen":
		return "https://dashscope.aliyuncs.com/api/token"
	default:
		return ""
	}
}

func (m *OAuthCredentialManager) Start(ctx context.Context) {
	if m.auto == nil {
		return
	}

	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := m.RefreshAll(ctx); err != nil {
					m.logger.WithError(err).Warn("Failed to refresh OAuth credentials")
				}
			}
		}
	}()
}

func (m *OAuthCredentialManager) RefreshAll(ctx context.Context) error {
	if m.auto == nil {
		return nil
	}

	for provider := range m.paths {
		_, err := m.auto.GetCredentials(provider)
		if err != nil {
			// Empty/missing tokens are expected when OAuth credentials are not
			// configured for a provider — log at Debug to avoid log noise.
			errMsg := err.Error()
			if strings.Contains(errMsg, "empty access token") ||
				strings.Contains(errMsg, "no such file") ||
				strings.Contains(errMsg, "does not exist") {
				m.logger.WithError(err).WithField("provider", provider).Debug("Skipping credential refresh — credentials not configured")
			} else {
				m.logger.WithError(err).WithField("provider", provider).Warn("Failed to refresh credentials")
			}
		}
	}

	return nil
}

func (m *OAuthCredentialManager) GetAccessToken(provider string) (string, error) {
	if m.auto == nil {
		return "", fmt.Errorf("auto refresher not initialized")
	}

	creds, err := m.auto.GetCredentials(provider)
	if err != nil {
		return "", err
	}

	return creds.AccessToken, nil
}

func (m *OAuthCredentialManager) HasValidCredentials(provider string) bool {
	if m.auto == nil {
		return false
	}

	creds, err := m.auto.GetCredentials(provider)
	if err != nil {
		return false
	}

	return !creds.IsExpired()
}

type BearerTokenValidator struct {
	jwtSecret []byte
	issuer    string
}

func NewBearerTokenValidator(jwtSecret string, issuer string) *BearerTokenValidator {
	if issuer == "" {
		issuer = "helixagent"
	}
	return &BearerTokenValidator{
		jwtSecret: []byte(jwtSecret),
		issuer:    issuer,
	}
}

func (v *BearerTokenValidator) ValidateToken(token string) (map[string]interface{}, error) {
	return ValidateJWTToken(token, v.jwtSecret, v.issuer)
}

func BearerTokenAuthMiddleware(validator *BearerTokenValidator) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.Next()
			return
		}

		token, err := extractBearerToken(authHeader)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error":   "unauthorized",
				"message": err.Error(),
			})
			c.Abort()
			return
		}

		claims, err := validator.ValidateToken(token)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error":   "unauthorized",
				"message": "Invalid token",
			})
			c.Abort()
			return
		}

		for key, value := range claims {
			c.Set(key, value)
		}
		c.Set("authenticated", true)
		c.Set("auth_method", "bearer")

		c.Next()
	}
}

func extractBearerToken(authHeader string) (string, error) {
	const prefix = "Bearer "
	if len(authHeader) < len(prefix) || authHeader[:len(prefix)] != prefix {
		return "", fmt.Errorf("invalid authorization header format")
	}
	return authHeader[len(prefix):], nil
}

// Claims represents the JWT claims structure
type Claims struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	jwt.RegisteredClaims
}

// ValidateJWTToken validates a JWT token and returns the claims as a map.
// It verifies the token signature, expiration, and issuer.
func ValidateJWTToken(tokenString string, secret []byte, issuer string) (map[string]interface{}, error) {
	// Parse the token with claims
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		// Validate signing method
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return secret, nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to parse token: %w", err)
	}

	// Validate token claims
	if claims, ok := token.Claims.(*Claims); ok && token.Valid {
		// Check issuer if specified
		if issuer != "" && claims.Issuer != issuer {
			return nil, fmt.Errorf("invalid issuer: expected %s, got %s", issuer, claims.Issuer)
		}

		// Check expiration
		if claims.ExpiresAt != nil && claims.ExpiresAt.Time.Before(time.Now()) {
			return nil, fmt.Errorf("token has expired")
		}

		// Return claims as map
		result := map[string]interface{}{
			"user_id":  claims.UserID,
			"username": claims.Username,
			"role":     claims.Role,
			"issuer":   claims.Issuer,
			"subject":  claims.Subject,
		}

		if claims.ExpiresAt != nil {
			result["expires_at"] = claims.ExpiresAt.Time
		}

		return result, nil
	}

	return nil, fmt.Errorf("invalid token claims")
}

func RequireScopes(scopes ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !isAuthenticated(c) {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error":   "unauthorized",
				"message": "Authentication required",
			})
			c.Abort()
			return
		}

		userRole := getUserRole(c)
		if userRole == "admin" {
			c.Next()
			return
		}

		for _, scope := range scopes {
			if !hasScope(c, scope) {
				c.JSON(http.StatusForbidden, gin.H{
					"error":   "forbidden",
					"message": fmt.Sprintf("Missing required scope: %s", scope),
				})
				c.Abort()
				return
			}
		}

		c.Next()
	}
}

func isAuthenticated(c *gin.Context) bool {
	if auth, exists := c.Get("authenticated"); exists {
		return auth.(bool) //nolint:errcheck
	}
	return false
}

func getUserRole(c *gin.Context) string {
	if role, exists := c.Get("role"); exists {
		if r, ok := role.(string); ok {
			return r
		}
	}
	return ""
}

func hasScope(c *gin.Context, scope string) bool {
	return getUserRole(c) == scope || getUserRole(c) == "admin"
}

func GetOAuthCredentialPaths() map[string]string {
	paths := make(map[string]string)

	if home, err := os.UserHomeDir(); err == nil {
		claudePath := home + "/.claude/.credentials.json"
		if _, err := os.Stat(claudePath); err == nil {
			paths["claude"] = claudePath
		}

		qwenPath := home + "/.qwen/oauth_creds.json"
		if _, err := os.Stat(qwenPath); err == nil {
			paths["qwen"] = qwenPath
		}
	}

	return paths
}

func InitializeAuthIntegration(router *gin.Engine, userService UserService, jwtSecret string, logger *logrus.Logger) error {
	apiKeyValidator := NewAPIKeyValidator(userService, logger)
	bearerValidator := NewBearerTokenValidator(jwtSecret, "helixagent")

	router.Use(APIKeyAuthMiddleware(apiKeyValidator, "X-API-Key"))
	router.Use(BearerTokenAuthMiddleware(bearerValidator))

	// Local-first default (spec 002, HA-F2-002): starting the refresh loop from
	// the mere PRESENCE of ~/.claude/.credentials.json or ~/.qwen/oauth_creds.json
	// is an implicit cloud acquisition and obeys the same opt-in as every other
	// one. Mirrors the gate in internal/router/oauth_credentials.go; this copy
	// has no production caller today, so gating it closes the shape rather than a
	// live route.
	oauthPaths := GetOAuthCredentialPaths()
	if localfirst.CloudProvidersOptedIn() && len(oauthPaths) > 0 {
		oauthManager, err := NewOAuthCredentialManager(oauthPaths, "helixagent", logger)
		if err != nil {
			logger.WithError(err).Warn("Failed to initialize OAuth credential manager")
		} else {
			oauthManager.Start(context.Background())
			logger.WithField("providers", len(oauthPaths)).Info("OAuth credential manager initialized")
		}
	}

	return nil
}
