# Control Enercon Wind Turbines

This package uses the gopcxmlda package to interact via OPC XML DA with an Enercon SCADA PC and exposes functions to control Enercon Wind Turbines.

## Functions
The following Functions are implemented:
### Start(Server, PlantNo..)
Start one or more turbines.

Example: (ToDo)

### Stop(Server, Stoptype, PlantNo..)
Stop one or more turbines. Stoptype can be uint8(60) for 60° stop or uint8(90) for 90° Stop.

Example: (ToDo)

### Reset(Server, PlantNo..)
Reset one or more turbines.

Example: (ToDo)

### turbines(Server)
Get a list of turbines and which controls are available for each turbine.

Example:

## Data Structure "Server"
Containsi information about the OPC-XML-DA host.

type Server struct {
	Addr     string        // Address of the server
	Port     string        // Port number of the server
	LocaleID string        // Locale ID of the server
	timeout  time.Duration // Timeout duration for the connection
}
