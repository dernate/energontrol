# Control Enercon Wind Turbines

This package uses the gopcxmlda package to interact via OPC XML DA with an Enercon SCADA PC and exposes functions to control Enercon Wind Turbines.

## Functions
The following Functions are implemented:
- [ ] Start
- [ ] Stop
- [ ] Reset
- [ ] RbhOn
- [ ] RbhAutoOff
- [ ] RbhStandard

The following Functions are not yet implemented:
- [ ] ControlAndRbh
- [ ] Turbines

### Basic Procedure
Basic usage is as follows:

```go
package main
import (
    "github.com/dernate/energontrol"
)

func main() {
	s := Server{
		"http://your.opc-xml-da.server", 
		8080, 
		"en-US", 
		10,
	}
}
```

### Start(Server, UserId, PlantNo...)
Start one or more turbines.

Example:
```go
UserId := 1234
PlantNo := []uint8{2, 4}
started, errList := Start(Server, UserId, PlantNo...)
```

### Stop(Server, UserId, FullStop, ForceExplicitCommand, PlantNo...)
Stop one or more turbines. FullStop can be false for 60° stop or true for 90° Stop.
If ForceExplicitCommand is false, then any stop status that is already present is accepted. 
(For example: Requested status Stop60, but the plant is already at Stop90, then it is not stopped at Stop60, but Stop90 is accepted)
If ForceExplicitCommand is true, then the plant is stopped at the requested status, even if the plant is in a similar status.

Example:
```go
UserId := 1234
PlantNo := []uint8{2, 4}
stopped, errList := Stop(Server, UserId, true, true, PlantNo...)
```

### Reset(Server, UserId, PlantNo..)
Reset one or more turbines.

Example:
```go
UserId := 1234
PlantNo := []uint8{2, 4}
resetted, errList := Reset(Server, UserId, PlantNo...)
```

### RbhOn(Server, UserId, PlantNo...)
Set the Rotor Blade Heating to "Manual On".

Example:
```go
UserId := 1234
PlantNo := []uint8{2, 4}
rbhOn, errList := RbhOn(Server, UserId, PlantNo...)
```

### RbhAutoOff(Server, UserId, PlantNo...)
Set the Rotor Blade Heating to "Auto Off" (Supress Automatic -> Off).

Example:
```go
UserId := 1234
PlantNo := []uint8{2, 4}
rbhAutoOff, errList := RbhAutoOff(Server, UserId, PlantNo...)
```

### RbhStandard(Server, UserId, PlantNo...)
Set the Rotor Blade Heating to "Standard". If automatic heating is allowed, the automatic takes control.

Example:
```go
UserId := 1234
PlantNo := []uint8{2, 4}
rbhStandard, errList := RbhStandard(Server, UserId, PlantNo...)
```

### ControlAndRbh(Server, UserId, CtrlValue, RbhValue, PlantNo)
Set Ctrl and Rbh value for one plant at once.

Example: ...

### Turbines(Server)
Get a list of turbines and which controls are available for each turbine.

Example: ...
