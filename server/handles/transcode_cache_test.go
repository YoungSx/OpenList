package handles

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/transcode"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestTranscodeReadsDoNotRestartTerminalJobs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	db.Init(database)
	conf.Conf = conf.DefaultConfig(t.TempDir())
	for key, value := range map[string]string{conf.TranscodeCachePath: t.TempDir(), conf.TranscodeCacheMaxGB: "1"} {
		op.Cache.SetSetting(key, &model.SettingItem{Key: key, Value: value})
	}
	t.Cleanup(op.Cache.ClearAll)
	mgr := transcode.Default()
	oldCache, oldScheduler, oldChunks := mgr.Cache, mgr.Scheduler, mgr.Chunks
	mgr.Cache = transcode.NewCache(t.TempDir(), 1)
	mgr.Scheduler = transcode.NewScheduler(transcode.NewWorkerRegistry())
	mgr.Chunks = transcode.NewChunkManager(mgr)
	t.Cleanup(func() { mgr.Cache, mgr.Scheduler, mgr.Chunks = oldCache, oldScheduler, oldChunks })
	for _, tc := range []struct {
		handler gin.HandlerFunc
		status  transcode.JobStatus
		cached  bool
		code    int
	}{
		{TCSegment, transcode.JobFinished, true, http.StatusOK},
		{TCPlaylist, transcode.JobFinished, true, http.StatusOK},
		{TCSegment, transcode.JobFailed, true, http.StatusOK},
		{TCPlaylist, transcode.JobFailed, false, http.StatusInternalServerError},
		{TCSegment, transcode.JobFailed, false, http.StatusInternalServerError},
	} {
		job := transcode.NewJob()
		job.Profiles = []transcode.Profile{{Name: "source"}}
		job.Output.SegmentDuration = 2
		job.Probe.Duration = 2
		job = mgr.Scheduler.Submit(job)
		content := []byte("cached transport stream")
		if tc.cached {
			if _, err := mgr.Cache.PutSegment(job.ID, "source", 0, 2, bytes.NewReader(content), true); err != nil {
				t.Fatal(err)
			}
		}
		if err := mgr.Scheduler.Finish(&transcode.FinishRequest{JobID: job.ID, Status: tc.status, Error: "rate budget exceeded"}); err != nil {
			t.Fatal(err)
		}
		mgr.Cache.MarkFinal(job.ID, "source")
		ready := job.WaitReady()
		for range 2 {
			response := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(response)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/tc/cached", nil)
			ctx.Params = gin.Params{{Key: "job", Value: job.ID}, {Key: "token", Value: job.PlayToken},
				{Key: "profile", Value: "source"}, {Key: "seg", Value: "seg-0.ts"}}
			tc.handler(ctx)
			if response.Code != tc.code || job.GetStatus() != tc.status || job.WaitReady() != ready {
				t.Fatalf("read restarted its encoder: code=%d status=%s", response.Code, job.GetStatus())
			}
			if tc.code == http.StatusInternalServerError && !strings.Contains(response.Body.String(), "rate budget exceeded") {
				t.Fatalf("encoding failure was lost: %s", response.Body.String())
			}
		}
	}
}
