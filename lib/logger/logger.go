package logger

import (
	"flag"

	"go.uber.org/zap"
)

var logger *zap.Logger

var (
	logLevel = flag.String("logLevel", "info", "Set the log level (debug, info, warn, error)")
)

func Init() {
	if logger != nil {
		return
	}

	cfg := zap.NewProductionConfig()
	switch *logLevel {
	case "debug":
		cfg.Level.SetLevel(zap.DebugLevel)
	case "info":
		cfg.Level.SetLevel(zap.InfoLevel)
	case "warn":
		cfg.Level.SetLevel(zap.WarnLevel)
	case "error":
		cfg.Level.SetLevel(zap.ErrorLevel)
	default:
		panic("unknown log level: " + *logLevel)
	}

	l, _ := cfg.Build()
	logger = l
}

func Error(msg string, fields ...zap.Field) {
	Init()
	logger.WithOptions(zap.AddCallerSkip(1)).Error(msg, fields...)
}

func Info(msg string, fields ...zap.Field) {
	Init()

	logger.WithOptions(zap.AddCallerSkip(1)).Info(msg, fields...)
}

func Debug(msg string, fields ...zap.Field) {
	Init()

	logger.WithOptions(zap.AddCallerSkip(1)).Debug(msg, fields...)
}

func Warn(msg string, fields ...zap.Field) {
	Init()

	logger.WithOptions(zap.AddCallerSkip(1)).Warn(msg, fields...)
}

func With(fields ...zap.Field) *zap.Logger {
	return logger.WithOptions(zap.AddCallerSkip(1)).With(fields...)
}

func Stop() {
	if logger != nil {
		_ = logger.Sync()
	}
}
