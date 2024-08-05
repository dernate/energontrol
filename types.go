package energontrol

import "time"

type PlantState struct {
	PlantNo   uint8
	CtrlState uint64
	Action    bool
}

type SessionRequest struct {
	SessionId  uint8
	UserId     uint64
	PrivateKey uint16
}

type WaitForState struct {
	Desired uint16
	Sleep   time.Duration
	Retries uint
}

type ControlAndRbhValue struct {
	SetCtrlValue bool
	CtrlValue    uint64
	CtrlAction   []bool
	SetRbhValue  bool
	RbhValue     uint64
	RbhAction    []bool
}
