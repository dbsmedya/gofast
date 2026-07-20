package api

// HTTP middleware for the machine API: bearer auth, request-id generation,
// the contract error envelope, and gzip response compression.

import (
	"compress/gzip"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func unauthorized(c *gin.Context) {
	contractError(c, http.StatusUnauthorized, "invalid_token", "Missing or invalid bearer token.")
}

func bearerAuthMiddleware(apiKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") || apiKey == "" {
			unauthorized(c)
			return
		}

		token := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
		if token != apiKey {
			unauthorized(c)
			return
		}
		c.Next()
	}
}

func generateRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		now := time.Now().UnixNano()
		return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", now&0xffffffff, (now>>32)&0xffff, (now>>48)&0xffff, now&0xffff, now&0xffffffffffff)
	}

	// UUIDv4 bits
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8|uint32(b[3]),
		uint16(b[4])<<8|uint16(b[5]),
		uint16(b[6])<<8|uint16(b[7]),
		uint16(b[8])<<8|uint16(b[9]),
		uint64(b[10])<<40|uint64(b[11])<<32|uint64(b[12])<<24|uint64(b[13])<<16|uint64(b[14])<<8|uint64(b[15]),
	)
}

func getRequestID(c *gin.Context) string {
	if v, exists := c.Get("request_id"); exists {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func contractError(c *gin.Context, status int, code, message string) {
	slog.Warn("api error response",
		"request_id", getRequestID(c),
		"method", c.Request.Method,
		"path", c.Request.URL.Path,
		"status", status,
		"code", code,
		"message", message,
	)
	c.AbortWithStatusJSON(status, gin.H{
		"error":      code,
		"message":    message,
		"request_id": getRequestID(c),
	})
}

type gzipResponseWriter struct {
	gin.ResponseWriter
	writer *gzip.Writer
}

func (g *gzipResponseWriter) Write(data []byte) (int, error) {
	return g.writer.Write(data)
}

func (g *gzipResponseWriter) WriteString(s string) (int, error) {
	return g.writer.Write([]byte(s))
}

func gzipMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
			c.Next()
			return
		}

		c.Header("Content-Encoding", "gzip")
		c.Header("Vary", "Accept-Encoding")

		gz := gzip.NewWriter(c.Writer)
		defer gz.Close()

		c.Writer = &gzipResponseWriter{ResponseWriter: c.Writer, writer: gz}
		c.Next()
	}
}
