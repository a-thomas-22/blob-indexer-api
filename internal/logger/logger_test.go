package logger

import (
	"context"
	"os"
	"testing"
)

func TestInitialize(t *testing.T) {
	// Initialize should not panic
	Initialize()
	if log == nil {
		t.Fatal("expected logger to be initialized")
	}
}

func TestInitialize_DebugLevel(t *testing.T) {
	t.Setenv("LOG_LEVEL", "debug")
	Initialize()
	if log == nil {
		t.Fatal("expected logger to be initialized")
	}
}

func TestInitialize_InvalidLevel(t *testing.T) {
	t.Setenv("LOG_LEVEL", "invalid_level")
	Initialize()
	// Should fall back to info level without error
	if log == nil {
		t.Fatal("expected logger to be initialized")
	}
}

func TestInitialize_AllLevels(t *testing.T) {
	levels := []string{"debug", "info", "warn", "error"}
	for _, level := range levels {
		t.Run(level, func(t *testing.T) {
			os.Setenv("LOG_LEVEL", level)
			defer os.Setenv("LOG_LEVEL", "")
			Initialize()
			if log == nil {
				t.Fatalf("expected logger to be initialized at level %s", level)
			}
		})
	}
}

func TestLogFunctions(t *testing.T) {
	Initialize()
	// These should not panic
	Info("test info message")
	Debug("test debug message")
	Warn("test warn message")
	Error("test error message")
}

func TestWithContext_WithRequestID(t *testing.T) {
	Initialize()
	ctx := context.WithValue(context.Background(), RequestIDKey, "test-123")
	l := WithContext(ctx)
	if l == nil {
		t.Fatal("expected non-nil logger")
	}
}

func TestWithContext_WithoutRequestID(t *testing.T) {
	Initialize()
	l := WithContext(context.Background())
	if l == nil {
		t.Fatal("expected non-nil logger")
	}
}

func TestSync(t *testing.T) {
	Initialize()
	// Sync may return an error for stdout, which is acceptable
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
