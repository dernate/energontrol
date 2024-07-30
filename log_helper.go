package energontrol

import (
	log "github.com/sirupsen/logrus"
	"time"
)

func LogLevel(level uint32) {
	log.SetLevel(log.Level(level))
}

func LogInfo(plantNo uint8, action string, msg string) {
	t := time.Now()
	log.WithFields(log.Fields{
		"PlantNo": plantNo,
		"Action":  action,
		"Time":    t.Format(time.RFC3339),
	}).Info(msg)
}
