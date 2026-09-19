package admincommand

import (
	"fmt"
	"hash/fnv"
	"time"
)

func checkDelay(id string, at time.Time, interval time.Duration, retry int64) time.Duration {
	h := fnv.New32a()
	_, _ = fmt.Fprintf(h, "%s:%d", id, at.Unix()/int64(interval.Seconds()))
	delay := time.Duration(float64(interval) * (0.8 + float64(h.Sum32()%4001)/10000))
	if minimum := time.Duration(retry) * time.Second; minimum > delay {
		delay = minimum
	}
	return delay
}
