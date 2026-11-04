package notifications

import (
	"time"

	"dont/internal/jobs"
	"github.com/jinzhu/gorm"
)

// A room need not have been viewed before restoring it. Track its actual world
// IDs even when no previous day has been sampled, without starting any world.
func (s *Service) TrackRestoreWorlds(job jobs.Job, worlds []string) {
	if job.Status != jobs.StatusRunning || (job.Kind != "backup.restore" && job.Kind != "backup-set.restore") || job.RoomID == "" {
		return
	}
	s.project(func(p *Service) {
		for index, world := range worlds {
			if index >= 32 {
				break
			}
			var previous worldNoticeRecord
			err := p.store.db.Table(p.store.channelsTable+"_world").Where("id = ?", worldNoticeKey(job.RoomID, world)).First(&previous).Error
			if err != nil && !gorm.IsRecordNotFoundError(err) {
				p.projectionError(err)
				return
			}
			before := previous.Day
			if time.Since(previous.ObservedAt) > 2*time.Minute || previous.ObservedAt.After(time.Now()) {
				before = nil
			}
			at := job.CreatedAt
			if job.StartedAt != nil {
				at = *job.StartedAt
			}
			if at.IsZero() {
				at = time.Now().UTC()
			}
			record := changeNoticeRecord{ID: job.ID + ":" + worldNoticeKey(job.RoomID, world), RunID: job.ID, RoomID: job.RoomID, WorldID: world, Action: "restore", BeforeDay: before, Status: "waiting", CreatedAt: at}
			if err := p.store.db.Table(p.store.channelsTable+"_change").Where("id = ?", record.ID).FirstOrCreate(&record).Error; err != nil {
				p.projectionError(err)
				return
			}
		}
	})
}
