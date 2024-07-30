package energontrol

import (
	"github.com/dernate/gopcxmlda"
)

func Start(Server gopcxmlda.Server, UserId string, PlantNo ...uint8) ([]bool, error) {
	// check if Server is connected
	if available, err := serverAvailable(Server); !available {
		return make([]bool, len(PlantNo)), err
	}
	// check if plants have already the desired state
	plantState, err := getPlantCtrlState(Server, PlantNo)
	if err != nil {
		return make([]bool, len(PlantNo)), err
	}
	//remove items from PlantNo if they are already started
	setAction(&plantState, CtrlValues["Start"])
	for _, state := range plantState {
		if state.Action {
			// start plant
			// ToDo
		} else {
			LogInfo(state.PlantNo, "Start", "Plant already started")
		}
	}
	return nil, nil
}
