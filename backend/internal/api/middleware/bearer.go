package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Bearer is a constant-time token comparator middleware. It guards the
// approval endpoints — the only routes that can move money — so a
// publicly-bound backend is not implicitly a kill-switch for anyone.
//
// Standard, not a bandaid: a constant-time compare prevents timing
// attacks even though our threat model is small. The token comes from
// env (APPROVAL_TOKEN) so it never ends up in source or in shared
// config files. main.go fails fast at startup when EXECUTION_MODE=live
// without a token configured — there is no "default password" path.
//
// The user identifier ("operator") is stored on the context so the
// approval state machine can record `decided_by` in the audit trail.
func Bearer(token string) gin.HandlerFunc {
	expected := []byte(token)
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(header, prefix) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}
		provided := []byte(strings.TrimPrefix(header, prefix))
		if subtle.ConstantTimeCompare(provided, expected) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid bearer token"})
			return
		}
		c.Set("operator", "bearer")
		// Forensics: store the source IP and user-agent so the approval
		// state machine can persist them on every decision. Honours
		// X-Forwarded-For when behind a trusted proxy (operator-only
		// dashboards usually are).
		c.Set("operator_ip", clientIP(c))
		c.Set("operator_user_agent", c.GetHeader("User-Agent"))
		c.Next()
	}
}

// clientIP returns the trusted remote address. Gin's ClientIP() already
// reads X-Forwarded-For / X-Real-IP when the engine is configured with
// SetTrustedProxies — we just adopt that policy here.
func clientIP(c *gin.Context) string {
	if ip := c.ClientIP(); ip != "" {
		return ip
	}
	return c.Request.RemoteAddr
}
