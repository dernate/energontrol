# Control Enercon Wind Turbines

This package uses the gopcxmlda package to interact via OPC XML DA with an Enercon SCADA PC and exposes functions to control Enercon Wind Turbines.

## Functions
The following Functions are implemented:

The following Functions are not yet implemented:
- [ ] Start
- [ ] Stop
- [ ] Reset
- [ ] Turbines

### Start(Server, PlantNo..)
Start one or more turbines.

Example: ...

### Stop(Server, Stoptype, PlantNo..)
Stop one or more turbines. Stoptype can be uint8(60) for 60° stop or uint8(90) for 90° Stop.

Example: ...

### Reset(Server, PlantNo..)
Reset one or more turbines.

Example: ...

### RbhOn(Server, PlantNo..)
Set the Rotor Blade Heating to "Manual On".

Example: ...

### RbhAutoOff(Server, PlantNo..)
Set the Rotor Blade Heating to "Auto Off" (Supress Automatic -> Off).

Example: ...

### RbhStandard(Server, PlantNo..)
Set the Rotor Blade Heating to "Standard".

Example: ...

### Turbines(Server)
Get a list of turbines and which controls are available for each turbine.

Example: ...

## Data Structure "Server"
Contains information about the OPC-XML-DA host.

type Server struct {
	Addr     string        // Address of the server
	Port     string        // Port number of the server
	LocaleID string        // Locale ID of the server
	timeout  time.Duration // Timeout duration for the connection
}
