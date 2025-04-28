package logger

import (
	"context"
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	// Global logger instance
	log *zap.Logger
)

// Log levels mapped from string to zapcore.Level
var levelMap = map[string]zapcore.Level{
	"debug": zapcore.DebugLevel,
	"info":  zapcore.InfoLevel,
	"warn":  zapcore.WarnLevel,
	"error": zapcore.ErrorLevel,
	"fatal": zapcore.FatalLevel,
}

// Initialize sets up the logger with JSON encoding and the appropriate level
func Initialize() {
	// Get log level from environment variable, default to "info"
	levelStr := os.Getenv("LOG_LEVEL")
	if levelStr == "" {
		levelStr = "info"
	}

	// Map string level to zap level
	level, exists := levelMap[levelStr]
	if !exists {
		level = zapcore.InfoLevel
	}

	// Create JSON encoder config
	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.TimeKey = "timestamp"
	encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder

	// Create JSON encoder
	config := zap.Config{
		Level:             zap.NewAtomicLevelAt(level),
		Development:       false,
		DisableCaller:     false,
		DisableStacktrace: false,
		Sampling:          nil,
		Encoding:          "json",
		EncoderConfig:     encoderConfig,
		OutputPaths:       []string{"stdout"},
		ErrorOutputPaths:  []string{"stderr"},
	}

	// Build the logger
	var err error
	log, err = config.Build()
	if err != nil {
		panic("Failed to initialize logger: " + err.Error())
	}

	Info("Logger initialized", zap.String("level", levelStr))
}

// Helper functions for logging
func Info(msg string, fields ...zap.Field) {
	log.Info(msg, fields...)
}

func Debug(msg string, fields ...zap.Field) {
	log.Debug(msg, fields...)
}

func Warn(msg string, fields ...zap.Field) {
	log.Warn(msg, fields...)
}

func Error(msg string, fields ...zap.Field) {
	log.Error(msg, fields...)
}

func Fatal(msg string, fields ...zap.Field) {
	log.Fatal(msg, fields...)
}

// WithContext returns a logger with request context
func WithContext(ctx context.Context) *zap.Logger {
	// Extract request ID or other context values
	if requestID, ok := ctx.Value("requestID").(string); ok {
		return log.With(zap.String("request_id", requestID))
	}
	return log
}

// Sync flushes any buffered log entries
func Sync() error {
	return log.Sync()
}
