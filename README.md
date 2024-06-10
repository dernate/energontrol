# Control Enercon Wind Turbines via HTTP API

This package uses the gopcxmlda package to interact via OPC XML DA with an Enercon SCADA PC and exposes an HTTP API to control Enercon Wind Turbines.

It uses Go as programming language, that's why ener**G**ontrol.

One can say this is a translator or gateway for an easier access, as the OPC XML DA protocol, used by Enercon to control Wind Turbines, is very outdated and has it's own issues.

## Endpoints
The following HTTP endpoints are implemented:
### /start
POST: Start one or more turbines.

Example: (ToDo)

### /stop
POST: Stop one or more turbines.

Example: (ToDo)

### /reset
POST: Reset one or more turbines.

Example: (ToDo)

### /turbines
GET: Get a list of turbines and which controls are available for each turbine.

Example:

## Configuration
The hostname/IP of the Enercon SCADA PC is required at start and can be passed via command line parameter.

Example:
./energontrol \<hostname\>
