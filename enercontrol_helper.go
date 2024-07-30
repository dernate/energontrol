package energontrol

import (
	"fmt"
	"github.com/dernate/gopcxmlda"
)

func serverAvailable(Server gopcxmlda.Server) (bool, error) {
	// check if Server is connected
	var handle string
	status, err := Server.GetStatus(&handle, "")
	if err != nil {
		return false, err
	}
	return status.Body.GetStatusResponse.GetStatusResult.ServerState == "running", nil
}

func getPlantCtrlState(Server gopcxmlda.Server, PlantNo []uint8) ([]PlantCtrlState, error) {
	// check plant ctrl state
	var handle1 string
	var handle2 []string
	options := map[string]interface{}{
		"returnItemName": true,
	}
	items := []gopcxmlda.T_Item{}
	for _, plant := range PlantNo {
		items = append(items, gopcxmlda.T_Item{
			ItemName: fmt.Sprintf("Loc/Wec/Plant%d/Ctrl/Ctrl", plant),
		})
	}
	value, err := Server.Read(items, &handle1, &handle2, "", options)
	if err != nil {
		return nil, err
	} else {
		plantState := make([]PlantCtrlState, len(PlantNo))
		for i, item := range value.Body.ReadResponse.RItemList.Items {
			plantState[i].PlantNo = PlantNo[i]
			plantState[i].CtrlState = item.Value.Value.(uint64)
		}
		return plantState, nil
	}
}

func setAction(plantState *[]PlantCtrlState, action uint64) {
	//remove items from PlantNo if they are already started
	for _, state := range *plantState {
		if state.CtrlState == action {
			state.Action = false
		} else {
			state.Action = true
		}
	}
}
