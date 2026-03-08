package logger

import (
	"context"
	"os"
	"testing"
)

func TestInitialize(t *testing.T) {
	// Save and restore LOG_LEVEL
	original := os.Getenv("LOG_LEVEL")
	defer os.Setenv("LOG_LEVEL", original)

	os.Setenv("LOG_LEVEL", "debug")
	Initialize()

	// Should not panic after initialization
	Info("test info message")
	Debug("test debug message")
	Warn("test warn message")
	Error("test error message")
}

func TestInitialize_DefaultLevel(t *testing.T) {
	original := os.Getenv("LOG_LEVEL")
	defer os.Setenv("LOG_LEVEL", original)

	os.Unsetenv("LOG_LEVEL")
	Initialize()

	// Should use "info" level by default
	Info("info message after default init")
}

func TestInitialize_InvalidLevel(t *testing.T) {
	original := os.Getenv("LOG_LEVEL")
	defer os.Setenv("LOG_LEVEL", original)

	os.Setenv("LOG_LEVEL", "invalid_level")
	Initialize()

	// Should fall back to info level
	Info("info message after invalid level init")
}

func TestInitialize_AllLevels(t *testing.T) {
	original := os.Getenv("LOG_LEVEL")
	defer os.Setenv("LOG_LEVEL", original)

	levels := []string{"debug", "info", "warn", "error"}
	for _, level := range levels {
		t.Run(level, func(t *testing.T) {
			os.Setenv("LOG_LEVEL", level)
			Initialize()
			Info("test at " + level + " level")
		})
	}
}

func TestWithContext_WithRequestID(t *testing.T) {
	Initialize()

	ctx := context.WithValue(context.Background(), "requestID", "test-req-123")
	l := WithContext(ctx)
	if l == nil {
		t.Fatal("expected non-nil logger")
	}
}

func TestWithContext_WithoutRequestID(t *testing.T) {
	Initialize()

	ctx := context.Background()
	l := WithContext(ctx)
	if l == nil {
		t.Fatal("expected non-nil logger")
	}
}

func TestSync(t *testing.T) {
	Initialize()

	// Sync should not return a fatal error (stdout sync may return an error
	// on some systems, but it should not panic)
	_ = Sync()
}

func TestLevelMap(t *testing.T) {
	expectedLevels := []string{"debug", "info", "warn", "error", "fatal"}
	for _, level := range expectedLevels {
		if _, ok := levelMap[level]; !ok {
			t.Errorf("expected level %q in levelMap", level)
		}
	}
}
