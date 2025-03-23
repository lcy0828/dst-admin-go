package middleware

import (
	"dont/middleware/cors"
	"dont/middleware/jwt"
	"github.com/gin-gonic/gin"
)

// CorsMiddleware 返回CORS中间件
func CorsMiddleware() gin.HandlerFunc {
	return cors.Cors()
}

// JWTAuth 返回JWT认证中间件
func JWTAuth() gin.HandlerFunc {
	return jwt.JWT()
} 