package energontrol

import (
	"github.com/dernate/gopcxmlda"
)

func Start(Server gopcxmlda.Server, UserId uint64, PlantNo ...uint8) ([]bool, []error) {
	var errList []error
	var started []bool
	if len(PlantNo) == 0 {
		return nil, nil
	} else {
		for range PlantNo {
			started = append(started, false)
			errList = append(errList, nil)
		}
	}
	// check if Server is connected
	if available, err := serverAvailable(Server); !available {
		for range PlantNo {
			errList = append(errList, err)
		}
		return make([]bool, len(PlantNo)), errList
	}
	// check if plants have already the desired state
	plantState, err := getPlantCtrlState(Server, PlantNo)
	if err != nil {
		for range PlantNo {
			errList = append(errList, err)
		}
		return make([]bool, len(PlantNo)), errList
	}
	// check if plants are already started. If not set an Action Bit
	setActionToStart(&plantState)
	// Filter plants based on the evaluated Action Bit
	var PlantNoToStart []uint8
	for i, state := range plantState {
		if !state.Action {
			LogInfo(state.PlantNo, "Start", "Plant already started")
			started[i] = true
		} else {
			// Process just plants, that are not already started
			PlantNoToStart = append(PlantNoToStart, PlantNo[i])
		}
	}
	// start plants
	startedFiltered, errListFiltered := controlProcedure(Server, UserId, 0, PlantNoToStart...)
	if len(errListFiltered) > 0 {
		for i, err := range errListFiltered {
			if err != nil {
				LogError(PlantNoToStart[i], "Start", err.Error())
				for j, p := range PlantNo {
					if PlantNoToStart[i] == p {
						errList[j] = err
					}
				}
			} else if startedFiltered[i] {
				for j, p := range PlantNo {
					if PlantNoToStart[i] == p {
						started[j] = true
					}
				}
			}
		}
	}
	return started, errList
}

// Stop FullStop = true stops to "Stop" (90° blade angle), while FullStop = false stops to "Stop60"
func Stop(Server gopcxmlda.Server, UserId uint64, FullStop bool, ForceExplicitCommand bool, PlantNo ...uint8) ([]bool, []error) {
	var errList []error
	var stopped []bool
	if len(PlantNo) == 0 {
		return nil, nil
	} else {
		for range PlantNo {
			stopped = append(stopped, false)
			errList = append(errList, nil)
		}
	}
	Action := "Stop"
	var CtrlValue uint64
	if FullStop {
		CtrlValue = 2
	} else {
		CtrlValue = 1
	}
	// check if Server is connected
	if available, err := serverAvailable(Server); !available {
		for range PlantNo {
			errList = append(errList, err)
		}
		return make([]bool, len(PlantNo)), errList
	}
	// check if plants have already the desired state
	plantState, err := getPlantCtrlState(Server, PlantNo)
	if err != nil {
		for range PlantNo {
			errList = append(errList, err)
		}
		return make([]bool, len(PlantNo)), errList
	}
	// check if plants are already stopped. If not set an Action Bit. Consider ForceExplicitCommand.
	setActionToStop(&plantState, ForceExplicitCommand, CtrlValue)
	// Filter plants based on the evaluated Action Bit
	var PlantNoToStop []uint8
	for i, state := range plantState {
		if !state.Action {
			LogInfo(state.PlantNo, Action, "Plant already stopped")
			stopped[i] = true
		} else {
			// Process just plants, that are not already stopped
			PlantNoToStop = append(PlantNoToStop, PlantNo[i])
		}
	}
	// stop plants
	stoppedFiltered, errListFiltered := controlProcedure(Server, UserId, CtrlValue, PlantNoToStop...)
	if len(errListFiltered) > 0 {
		for i, err := range errListFiltered {
			if err != nil {
				LogError(PlantNoToStop[i], Action, err.Error())
				for j, p := range PlantNo {
					if PlantNoToStop[i] == p {
						errList[j] = err
					}
				}
			} else if stoppedFiltered[i] {
				for j, p := range PlantNo {
					if PlantNoToStop[i] == p {
						stopped[j] = true
					}
				}
			}
		}
	}
	return stopped, errList
}

func Reset(Server gopcxmlda.Server, UserId uint64, PlantNo ...uint8) ([]bool, []error) {
	var errList []error
	var resetted []bool
	if len(PlantNo) == 0 {
		return nil, nil
	} else {
		for range PlantNo {
			resetted = append(resetted, false)
			errList = append(errList, nil)
		}
	}
	Action := "Reset"
	// check if Server is connected
	if available, err := serverAvailable(Server); !available {
		for range PlantNo {
			errList = append(errList, err)
		}
		return make([]bool, len(PlantNo)), errList
	}
	// Reset Plants
	resetted, errList = resetProcedure(Server, UserId, PlantNo...)
	if len(errList) > 0 {
		for i, err := range errList {
			if err != nil {
				LogError(PlantNo[i], Action, err.Error())
			}
		}
	}
	return resetted, errList
}
