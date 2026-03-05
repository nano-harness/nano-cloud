package main

import (
	"flag"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/nano-harness/nano-cloud/pkg/server"
	"github.com/sirupsen/logrus"
	"gopkg.in/natefinch/lumberjack.v2"
)

func main() {
	addr := flag.String("addr", ":8081", "Gateway server address")
	token := flag.String("token", "", "Authentication token")
	configStoreDir := flag.String("config-store-dir", "./data", "Worker config store directory")
	logFile := flag.String("log-file", "", "Log file path (optional)")
	logLevel := flag.String("log-level", "info", "Log level (debug, info, warn, error)")
	logMaxSizeMB := flag.Int("log-max-size", 50, "Max log size in MB before rotation")
	logMaxBackups := flag.Int("log-max-backups", 10, "Max number of rotated log files to keep")
	logMaxAgeDays := flag.Int("log-max-age", 14, "Max age in days to keep rotated logs")
	logCompress := flag.Bool("log-compress", true, "Compress rotated logs")
	flag.Parse()

	logger := logrus.New()
	level, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		level = logrus.InfoLevel
	}
	logger.SetLevel(level)
	logger.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})

	var out io.Writer = os.Stdout
	if lf := filepath.Clean(*logFile); lf != "" && lf != "." {
		if err := os.MkdirAll(filepath.Dir(lf), 0o755); err != nil {
			log.Fatal(err)
		}
		out = io.MultiWriter(os.Stdout, &lumberjack.Logger{
			Filename:   lf,
			MaxSize:    *logMaxSizeMB,
			MaxBackups: *logMaxBackups,
			MaxAge:     *logMaxAgeDays,
			Compress:   *logCompress,
		})
	}
	logger.SetOutput(out)

	srv := server.NewGatewayServerWithLogger(*addr, *token, *configStoreDir, logger)
	log.Fatal(srv.Start())
}
