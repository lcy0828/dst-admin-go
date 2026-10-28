package performance

import (
	"fmt"
	"testing"
	"time"
)

// Synthetic two-world retention corpus. No game or production database is used.
func BenchmarkRetainedReport(b *testing.B) {
	s := storeFixture(b)
	now := time.Now().UTC().Truncate(5 * time.Minute)
	for _, spec := range []struct {
		table      string
		days, step int
	}{{s.raw, 7, 30}, {s.aggregate, 90, 300}} {
		tx, err := s.db.DB().Begin()
		if err != nil {
			b.Fatal(err)
		}
		stmt, err := tx.Prepare(fmt.Sprintf(`INSERT INTO %s (id,room_id,world_id,at_unix,world_name,target_id,installation_id,instance_id,state,score_version,score_sum,valid_seconds,low_seconds,window_seconds,minimum_score,cpu_sum,cpu_count,cpu_peak,rss_bytes,samples) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, spec.table))
		if err != nil {
			b.Fatal(err)
		}
		for i := 0; i < spec.days*86400/spec.step; i++ {
			for _, world := range []string{"master", "caves"} {
				at := now.Add(-time.Duration(i*spec.step) * time.Second)
				instance := "local/default/1793188375-569056/5A1672E99FF2C5C2/simulation-speed-v1"
				_, err = stmt.Exec(recordID("room", world, instance, at.Unix()), "room", world, at.Unix(), world, "local", "default", instance, "running", ScoreVersion, 95*float64(spec.step), spec.step, 0, spec.step, 90, 50, 1, 60, 512<<20, spec.step/30)
				if err != nil {
					b.Fatal(err)
				}
			}
		}
		stmt.Close()
		if err = tx.Commit(); err != nil {
			b.Fatal(err)
		}
	}
	var pages, size int
	s.db.Raw("PRAGMA page_count").Row().Scan(&pages)
	s.db.Raw("PRAGMA page_size").Row().Scan(&size)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, err := s.Report("room", Query{From: now.Add(-90 * 24 * time.Hour), To: now, MaxPoints: 600}, 2, now)
		if err != nil || len(r.Worlds) != 2 {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(pages*size), "database-bytes/2-worlds")
}
func BenchmarkAppend(b *testing.B) {
	s := storeFixture(b)
	at := time.Now().UTC()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Append(Point{RoomID: "room", WorldID: "master", WorldName: "Master", InstanceID: "boot/session", State: "running", At: at.Add(time.Duration(i) * 30 * time.Second), Score: number(95), WindowSeconds: 30, ValidSeconds: 30, CPUPercent: number(50), RSSBytes: 512 << 20}); err != nil {
			b.Fatal(err)
		}
	}
}
