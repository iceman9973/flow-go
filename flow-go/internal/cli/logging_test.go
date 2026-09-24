package cli

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnifiedLoggingAppendsToLogFile(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test_flow.log")
	t.Setenv("FLOW_LOG_FILE", logPath)

	cleanup := initUnifiedLogging()
	if cleanup == nil {
		t.Fatalf("initUnifiedLogging() returned nil cleanup function")
	}

	testMsg := "unique-test-log-entry-12345"
	log.Println(testMsg)
	cleanup()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("could not read log file: %v", err)
	}

	if !strings.Contains(string(data), testMsg) {
		t.Fatalf("log file %s does not contain test message %q; got:\n%s", logPath, testMsg, string(data))
	}
}
