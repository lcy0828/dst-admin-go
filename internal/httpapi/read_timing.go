package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"dont/internal/requesttiming"

	"github.com/gin-gonic/gin"
)

const slowReadLogThreshold = 500 * time.Millisecond

func finishReadTiming(c *gin.Context, recorder *requesttiming.Recorder, err error) {
	elapsed, metrics := recorder.Snapshot()
	c.Header("Server-Timing", requesttiming.Header(elapsed, metrics))
	// Keep browser timings for every read; serialize detailed logs only when
	// the request is slow or failed.
	if err == nil && elapsed < slowReadLogThreshold {
		return
	}
	stages, _ := json.Marshal(metrics)
	log.Printf("[ReadTiming] request_id=%s path=%s room_id=%q duration_ms=%.3f failed=%t canceled=%t stages=%s",
		RequestID(c), c.FullPath(), c.Param("roomId"), float64(elapsed)/float64(time.Millisecond), err != nil, errors.Is(err, context.Canceled), stages)
}
