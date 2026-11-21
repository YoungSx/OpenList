package handles

import (
	"bytes"
	"net/http"
	"net/http/httptest"
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

func TestCachedTranscodeReadsKeepCompletedJob(t *testing.T) {
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
	for _, handler := range []gin.HandlerFunc{TCSegment, TCPlaylist} {
		job := transcode.NewJob()
		job.Profiles = []transcode.Profile{{Name: "source"}}
		job.Output.SegmentDuration = 2
		job.Probe.Duration = 2
		job = mgr.Scheduler.Submit(job)
		content := []byte("cached transport stream")
		if _, err := mgr.Cache.PutSegment(job.ID, "source", 0, 2, bytes.NewReader(content), true); err != nil {
			t.Fatal(err)
		}
		if err := mgr.Scheduler.Finish(&transcode.FinishRequest{JobID: job.ID, Status: transcode.JobFinished}); err != nil {
			t.Fatal(err)
		}
		ready := job.WaitReady()
		response := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(response)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/tc/cached", nil)
		ctx.Params = gin.Params{{Key: "job", Value: job.ID}, {Key: "token", Value: job.PlayToken},
			{Key: "profile", Value: "source"}, {Key: "seg", Value: "seg-0.ts"}}
		handler(ctx)
		if response.Code != http.StatusOK || job.GetStatus() != transcode.JobFinished || job.WaitReady() != ready {
			t.Fatalf("cached read restarted its encoder: code=%d status=%s", response.Code, job.GetStatus())
		}
	}
}
