package energontrol

var CtrlValues = map[string]uint64{
	"Start":              0,
	"Stop60":             1,
	"Stop90":             2,
	"Stop60Enercon":      129,
	"StopEnercon":        130,
	"CommunicationError": 255,
}

var RbhValues = map[string]uint64{
	"Standard":       0,
	"AutoOff":        2,
	"ManualOn":       10,
	"PresetDuration": 128,
}

var sessionStates = map[uint16]string{
	0:   "Session free",
	1:   "Session reserved",
	2:   "Parameter input",
	3:   "Waiting time in loop mode",
	4:   "Waiting time session end",
	5:   "Session blocked (global reservation)",
	108: "Occupied",
	109: "Access denied",
	121: "Value error",
	174: "Incorrect user ID",
	175: "Insufficient rights",
}
