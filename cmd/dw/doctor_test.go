package main

import (
	"errors"
	"strings"
	"testing"
)

func TestDoctorWarnsWhenRcloneMissing(t *testing.T) {
	line := toolStatus("rclone", func(string) (string, error) {
		return "", errors.New("not found")
	})
	if !strings.Contains(line, "rclone: warning: not found") {
		t.Fatalf("line: %q", line)
	}
	ok := toolStatus("rclone", func(string) (string, error) {
		return "/usr/bin/rclone", nil
	})
	if !strings.Contains(ok, "/usr/bin/rclone") || strings.Contains(ok, "warning") {
		t.Fatalf("line: %q", ok)
	}
}
