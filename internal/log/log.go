package log

import (
	"os"
	"path/filepath"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Init creates a new *zap.SugaredLogger from config values.
// consoleLevel overrides the console sink's level; if empty it defaults to level.
func Init(level, consoleLevel, file string) (*zap.SugaredLogger, error) {
	zapLevel, err := parseLevel(level)
	if err != nil {
		zapLevel = zapcore.InfoLevel
	}

	consoleZapLevel := zapLevel
	if consoleLevel != "" {
		if cl, err := parseLevel(consoleLevel); err == nil {
			consoleZapLevel = cl
		}
	}

	// Console encoder (human-readable)
	consoleEnc := zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig())
	consoleCore := zapcore.NewCore(consoleEnc, zapcore.AddSync(os.Stderr), consoleZapLevel)

	cores := []zapcore.Core{consoleCore}

	// File encoder (JSON) if configured
	if file != "" {
		if err := os.MkdirAll(filepath.Dir(file), 0755); err == nil {
			f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if err == nil {
				fileEnc := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
				fileCore := zapcore.NewCore(fileEnc, zapcore.AddSync(f), zapLevel)
				cores = append(cores, fileCore)
			}
		}
	}

	logger := zap.New(zapcore.NewTee(cores...), zap.AddCaller())
	return logger.Sugar(), nil
}

func parseLevel(s string) (zapcore.Level, error) {
	var l zapcore.Level
	return l, l.UnmarshalText([]byte(s))
}

// AddCore attaches an additional core to a logger and returns the augmented logger.
// Used by the gateway log broadcaster to fan out log lines to SSE clients.
func AddCore(logger *zap.SugaredLogger, core zapcore.Core) *zap.SugaredLogger {
	return logger.Desugar().WithOptions(zap.WrapCore(func(existing zapcore.Core) zapcore.Core {
		return zapcore.NewTee(existing, core)
	})).Sugar()
}
