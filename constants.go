package energontrol

var CtrlValues = map[string]uint32{
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

var sessionStates = map[uint32]string{
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

const (
	RbhNoAccess                  = uint32(0)       // No Access on Rbh
	RbhAutoDeicingAllowed        = uint32(1)       // Automatic deicing allowed
	RbhAutoOffWEA                = uint32(1 << 1)  // Automatic operation of the heater suppressed
	RbhManualOnWEA               = uint32(1 << 2)  // RBH manually on (inside plant)
	RbhManualOnSCADA             = uint32(1 << 3)  // RBH manually on (SCADA)
	RbhAutoDeicingWhenStopped    = uint32(1 << 4)  // Automatic deicing when the system is stopped
	RbhAutoDeicingInOperation    = uint32(1 << 5)  // Automatic deicing in operation
	RbhHeatingPreventiveAuto     = uint32(1 << 6)  // Heater preventively turned on (by automation)
	RbhHeatingWhenStoppedSCADA   = uint32(1 << 7)  // Heater on when the system is stopped (SCADA)
	RbhHeatingInOperationSCADA   = uint32(1 << 8)  // Heater on during operation (SCADA)
	RbhNoSupplyPowerAvailable    = uint32(1 << 10) // No supply power available
	RbhFault                     = uint32(1 << 11) // Heater malfunction
	RbhDeicingAllowedInOperation = uint32(1 << 12) // Deicing allowed in operation
	RbhPreventiveHeaterAllowed   = uint32(1 << 13) // Preventive heater allowed
	RbhInstalled                 = uint32(1 << 15) // Heater installed
	RbhNotInstalled              = uint32(1 << 16) // Heater not installed
)

var RbhStatus = map[uint32]string{
	RbhNoAccess:                  "No Access on Rbh",
	RbhAutoDeicingAllowed:        "Automatic deicing allowed",
	RbhAutoOffWEA:                "Automatic operation of the heater suppressed",
	RbhManualOnWEA:               "RBH manually on (inside plant)",
	RbhManualOnSCADA:             "RBH manually on (SCADA)",
	RbhAutoDeicingWhenStopped:    "Automatic deicing when the system is stopped",
	RbhAutoDeicingInOperation:    "Automatic deicing in operation",
	RbhHeatingPreventiveAuto:     "Heater preventively turned on (by automation)",
	RbhHeatingWhenStoppedSCADA:   "Heater on when the system is stopped (SCADA)",
	RbhHeatingInOperationSCADA:   "Heater on during operation (SCADA)",
	RbhNoSupplyPowerAvailable:    "No supply power available",
	RbhFault:                     "Heater malfunction",
	RbhDeicingAllowedInOperation: "Deicing allowed in operation",
	RbhPreventiveHeaterAllowed:   "Preventive heater allowed",
	RbhInstalled:                 "Heater installed",
	RbhNotInstalled:              "Heater not installed",
}
