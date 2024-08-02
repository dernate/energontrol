package energontrol

import "time"

type PlantCtrlState struct {
	PlantNo   uint8
	CtrlState uint64
	Action    bool
}

type SessionRequest struct {
	SessionId  uint8
	UserId     uint64
	PrivateKey uint16
}

type WaitForSessionState struct {
	Desired uint16
	Sleep   time.Duration
	Retries uint
}
