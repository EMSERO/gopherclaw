package log

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestInitInfoLevel(t *testing.T) {
	logger, err := Init("info", "", "")
	if err != nil {
		t.Fatalf("Init(info) returned error: %v", err)
	}
	if logger == nil {
		t.Fatal("logger is nil after Init")
	}
}

func TestInitDebugLevel(t *testing.T) {
	_, err := Init("debug", "", "")
	if err != nil {
		t.Fatalf("Init(debug) returned error: %v", err)
	}
}

func TestInitWarnLevel(t *testing.T) {
	_, err := Init("warn", "", "")
	if err != nil {
		t.Fatalf("Init(warn) returned error: %v", err)
	}
}

func TestInitErrorLevel(t *testing.T) {
	_, err := Init("error", "", "")
	if err != nil {
		t.Fatalf("Init(error) returned error: %v", err)
	}
}

func TestInitInvalidLevelDefaultsToInfo(t *testing.T) {
	// Invalid level should not return an error; it defaults to info internally.
	logger, err := Init("bogus_level", "", "")
	if err != nil {
		t.Fatalf("Init(bogus_level) returned error: %v", err)
	}
	// The logger should still be usable (defaulted to info).
	logger.Info("test message after invalid level")
}

func TestInitConsoleLevelOverride(t *testing.T) {
	_, err := Init("debug", "warn", "")
	if err != nil {
		t.Fatalf("Init with console override returned error: %v", err)
	}
}

func TestInitConsoleLevelInvalidFallsBack(t *testing.T) {
	// Invalid console level should silently fall back to the primary level.
	_, err := Init("info", "not_a_level", "")
	if err != nil {
		t.Fatalf("Init with invalid console level returned error: %v", err)
	}
}

func TestInitWithFileOutput(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "subdir", "test.log")

	logger, err := Init("info", "", logFile)
	if err != nil {
		t.Fatalf("Init with file returned error: %v", err)
	}

	logger.Info("file log message")
	_ = logger.Desugar().Sync()

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("log file is empty; expected at least one message")
	}
}

func TestInitWithFileAndConsoleLevel(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "test2.log")

	logger, err := Init("debug", "error", logFile)
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}

	logger.Info("should appear in file")
	_ = logger.Desugar().Sync()

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("log file is empty")
	}
}

func TestAddCoreDoesNotPanic(t *testing.T) {
	logger, err := Init("info", "", "")
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// Create a simple no-op core and add it.
	nopCore := zapcore.NewNopCore()
	logger = AddCore(logger, nopCore)

	// Logger should still work after adding a core.
	logger.Info("message after AddCore")
}

func TestAddCoreIntegration(t *testing.T) {
	logger, err := Init("info", "", "")
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// Use a WriteSyncer that writes to a buffer so we can verify the core
	// actually receives messages.
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "extra.log"))
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer f.Close()

	enc := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	extraCore := zapcore.NewCore(enc, zapcore.AddSync(f), zapcore.InfoLevel)
	logger = AddCore(logger, extraCore)

	logger.Info("routed to extra core")
	_ = logger.Desugar().Sync()

	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("failed to read extra log: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("extra core log file is empty")
	}
}

func TestParseLevelViaInit(t *testing.T) {
	// Verify various valid level strings are accepted without error.
	levels := []string{"debug", "info", "warn", "error", "DEBUG", "INFO", "WARN", "ERROR"}
	for _, lvl := range levels {
		t.Run(lvl, func(t *testing.T) {
			_, err := Init(lvl, "", "")
			if err != nil {
				t.Errorf("Init(%q) returned error: %v", lvl, err)
			}
		})
	}
}
