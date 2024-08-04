package energontrol

import "time"

type PlantState struct {
	PlantNo   uint8
	CtrlState uint32
	Action    bool
}

type SessionRequest struct {
	SessionId  uint8
	UserId     uint64
	PrivateKey uint16
}

type WaitForState struct {
	Desired uint32
	Sleep   time.Duration
	Retries uint
}
