package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func postMergeTestVideo(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.mp4")
	cmd := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=yellow:s=200x300:r=10",
		"-t", "0.4", "-c:v", "libx264", "-pix_fmt", "yuv420p", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("video fixture: %v %s", err, out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
