package handles

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	_ "github.com/OpenListTeam/OpenList/v4/drivers/local"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/transcode"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestTranscodePlayAppliesProbedFiltersBeforeSubmitting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake ffprobe uses a shell script")
	}
	gin.SetMode(gin.TestMode)
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	conf.Conf = conf.DefaultConfig(t.TempDir())
	db.Init(database)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "movie.mkv"), []byte("test source"), 0o600); err != nil {
		t.Fatal(err)
	}
	addition, _ := json.Marshal(map[string]string{"root_folder_path": root})
	mount := "/transcode-filter-test"
	id, err := op.CreateStorage(context.Background(), model.Storage{Driver: "Local", MountPath: mount, Addition: string(addition)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = op.DeleteStorageById(context.Background(), id)
		op.Cache.ClearAll()
	})
	probePath := filepath.Join(t.TempDir(), "ffprobe")
	for key, value := range map[string]string{
		conf.TranscodeEnabled: "true", conf.TranscodeRunMode: "remote",
		conf.TranscodeMinSizeGB: "0", conf.TranscodeMinBitrateMbps: "20",
		conf.TranscodeSourceCodecs: "hevc", conf.TranscodeSourceExtensions: "mkv",
		conf.TranscodeFFprobePath: probePath, conf.TranscodeCachePath: t.TempDir(),
	} {
		op.Cache.SetSetting(key, &model.SettingItem{Key: key, Value: value})
	}
	user := &model.User{Username: "test-admin", BasePath: "/", Role: model.ADMIN}
	for _, tc := range []struct {
		codec, bitrate, reason string
	}{
		{"h264", "25000000", "codec not in list"},
		{"hevc", "10000000", "bitrate below threshold"},
		{"hevc", "25000000", ""},
	} {
		payload := `{"format":{"duration":"84"},"streams":[{"codec_type":"video","codec_name":"` + tc.codec + `","bit_rate":"` + tc.bitrate + `","pix_fmt":"yuv420p","width":1920,"height":1080}]}`
		if err := os.WriteFile(probePath, []byte("#!/bin/sh\nprintf '%s\\n' '"+payload+"'\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		before := len(transcode.Default().Scheduler.ListJobs())
		response := invokeHandler(t, user, map[string]string{"path": mount + "/movie.mkv"}, TranscodePlay)
		var result struct {
			Code int `json:"code"`
			Data struct {
				Transcode bool   `json:"transcode"`
				Reason    string `json:"reason"`
				JobID     string `json:"job_id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Code != 200 {
			t.Fatalf("unexpected response: %s (error %v)", response.Body.String(), err)
		}
		if tc.reason != "" {
			if result.Data.Transcode || result.Data.Reason != tc.reason || len(transcode.Default().Scheduler.ListJobs()) != before {
				t.Fatalf("filtered source was submitted: %s", response.Body.String())
			}
		} else {
			if !result.Data.Transcode || result.Data.JobID == "" {
				t.Fatalf("matching source was rejected: %s", response.Body.String())
			}
			job, ok := transcode.Default().Scheduler.Get(result.Data.JobID)
			if !ok || !strings.HasPrefix(job.SourceURL, "http") || strings.Contains(job.SourceURL, root) {
				t.Fatal("remote worker received a Master-only file path")
			}
			transcode.Default().Scheduler.Cancel(result.Data.JobID)
		}
	}
}
