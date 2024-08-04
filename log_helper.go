package energontrol

import (
	"fmt"
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

func LogWarn(plantNo uint8, action string, msg string) {
	t := time.Now()
	log.WithFields(log.Fields{
		"PlantNo": plantNo,
		"Action":  action,
		"Time":    t.Format(time.RFC3339),
	}).Warn(msg)
}

func LogError(plantNo uint8, action string, msg string) {
	t := time.Now()
	log.WithFields(log.Fields{
		"PlantNo": plantNo,
		"Action":  action,
		"Time":    t.Format(time.RFC3339),
	}).Error(msg)
}

func LogIfStateChangePermitted(state PlantState, PlantNo uint8, desiredState uint32) {
	if state.CtrlState > 128 {
		var MSG string
		var action string
		for _msg, _CtrlState := range CtrlValues {
			if _CtrlState == state.CtrlState {
				MSG = _msg
			}
			if _CtrlState == desiredState {
				action = _msg
			}
		}
		LogWarn(PlantNo, action,
			fmt.Sprintf("%s is not allowed, because plant is in state %d (%s)", action, state.CtrlState, MSG))
	}
}
