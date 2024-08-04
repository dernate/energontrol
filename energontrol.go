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
	plantState, err := getPlantCtrlOrRbhState(Server, "Ctrl", PlantNo)
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
	startedFiltered, errListFiltered := controlProcedure(Server, UserId, 0, "Ctrl", PlantNoToStart...)
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
	var CtrlValue uint32
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
	plantState, err := getPlantCtrlOrRbhState(Server, "Ctrl", PlantNo)
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
	stoppedFiltered, errListFiltered := controlProcedure(Server, UserId, CtrlValue, "Ctrl", PlantNoToStop...)
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

func RbhOn(server gopcxmlda.Server, UserId uint64, PlantNo ...uint8) ([]bool, []error) {
	var errList []error
	var rbhOn []bool
	if len(PlantNo) == 0 {
		return nil, nil
	} else {
		for range PlantNo {
			rbhOn = append(rbhOn, false)
			errList = append(errList, nil)
		}
	}
	// check if Server is connected
	if available, err := serverAvailable(server); !available {
		for range PlantNo {
			errList = append(errList, err)
		}
		return make([]bool, len(PlantNo)), errList
	}
	// check if plants have already the desired state
	plantState, err := getPlantCtrlOrRbhState(server, "Rbh", PlantNo)
	if err != nil {
		for range PlantNo {
			errList = append(errList, err)
		}
		return make([]bool, len(PlantNo)), errList
	}
	// check if Rbh is already On. If not set an Action Bit
	setActionRbh(&plantState, 10)
	// Filter plants based on the evaluated Action Bit
	var PlantNoToRbhOn []uint8
	for i, state := range plantState {
		if !state.Action {
			LogInfo(state.PlantNo, "RbhOn", "Plant Rbh already On")
			rbhOn[i] = true
		} else {
			// Process just plants, that are not already started
			PlantNoToRbhOn = append(PlantNoToRbhOn, PlantNo[i])
		}
	}
	// start Rbh for plants
	rbhOnFiltered, errListFiltered := controlProcedure(server, UserId, 10, "Rbh", PlantNoToRbhOn...)
	if len(errListFiltered) > 0 {
		for i, err := range errListFiltered {
			if err != nil {
				LogError(PlantNoToRbhOn[i], "RbhOn", err.Error())
				for j, p := range PlantNo {
					if PlantNoToRbhOn[i] == p {
						errList[j] = err
					}
				}
			} else if rbhOnFiltered[i] {
				for j, p := range PlantNo {
					if PlantNoToRbhOn[i] == p {
						rbhOn[j] = true
					}
				}
			}
		}
	}
	return rbhOn, errList
}

func RbhAutoOff(server gopcxmlda.Server, UserId uint64, PlantNo ...uint8) ([]bool, []error) {
	var errList []error
	var rbhAutoOff []bool
	if len(PlantNo) == 0 {
		return nil, nil
	} else {
		for range PlantNo {
			rbhAutoOff = append(rbhAutoOff, false)
			errList = append(errList, nil)
		}
	}
	// check if Server is connected
	if available, err := serverAvailable(server); !available {
		for range PlantNo {
			errList = append(errList, err)
		}
		return make([]bool, len(PlantNo)), errList
	}
	// check if plants have already the desired state
	plantState, err := getPlantCtrlOrRbhState(server, "Rbh", PlantNo)
	if err != nil {
		for range PlantNo {
			errList = append(errList, err)
		}
		return make([]bool, len(PlantNo)), errList
	}
	// check if Rbh is already AutoOff. If not set an Action Bit
	setActionRbh(&plantState, 2)
	// Filter plants based on the evaluated Action Bit
	var PlantNoToRbhAutoOff []uint8
	for i, state := range plantState {
		if !state.Action {
			LogInfo(state.PlantNo, "RbhAutoOff", "Plant Rbh already AutoOff")
			rbhAutoOff[i] = true
		} else {
			// Process just plants, that are not already started
			PlantNoToRbhAutoOff = append(PlantNoToRbhAutoOff, PlantNo[i])
		}
	}
	// start Rbh for plants
	rbhAutoOffFiltered, errListFiltered := controlProcedure(server, UserId, 2, "Rbh", PlantNoToRbhAutoOff...)
	if len(errListFiltered) > 0 {
		for i, err := range errListFiltered {
			if err != nil {
				LogError(PlantNoToRbhAutoOff[i], "RbhAutoOff", err.Error())
				for j, p := range PlantNo {
					if PlantNoToRbhAutoOff[i] == p {
						errList[j] = err
					}
				}
			} else if rbhAutoOffFiltered[i] {
				for j, p := range PlantNo {
					if PlantNoToRbhAutoOff[i] == p {
						rbhAutoOff[j] = true
					}
				}
			}
		}
	}
	return rbhAutoOff, errList
}

func RbhStandard(server gopcxmlda.Server, UserId uint64, PlantNo ...uint8) ([]bool, []error) {
	var errList []error
	var rbhStandard []bool
	if len(PlantNo) == 0 {
		return nil, nil
	} else {
		for range PlantNo {
			rbhStandard = append(rbhStandard, false)
			errList = append(errList, nil)
		}
	}
	// check if Server is connected
	if available, err := serverAvailable(server); !available {
		for range PlantNo {
			errList = append(errList, err)
		}
		return make([]bool, len(PlantNo)), errList
	}
	// check if plants have already the desired state
	plantState, err := getPlantCtrlOrRbhState(server, "Rbh", PlantNo)
	if err != nil {
		for range PlantNo {
			errList = append(errList, err)
		}
		return make([]bool, len(PlantNo)), errList
	}
	// check if Rbh is already Standard. If not set an Action Bit
	setActionRbh(&plantState, 0)
	// Filter plants based on the evaluated Action Bit
	var PlantNoToRbhStandard []uint8
	for i, state := range plantState {
		if !state.Action {
			LogInfo(state.PlantNo, "RbhStandard", "Plant Rbh already Standard")
			rbhStandard[i] = true
		} else {
			// Process just plants, that are not already started
			PlantNoToRbhStandard = append(PlantNoToRbhStandard, PlantNo[i])
		}
	}
	// start Rbh for plants
	rbhStandardFiltered, errListFiltered := controlProcedure(server, UserId, 0, "Rbh", PlantNoToRbhStandard...)
	if len(errListFiltered) > 0 {
		for i, err := range errListFiltered {
			if err != nil {
				LogError(PlantNoToRbhStandard[i], "RbhStandard", err.Error())
				for j, p := range PlantNo {
					if PlantNoToRbhStandard[i] == p {
						errList[j] = err
					}
				}
			} else if rbhStandardFiltered[i] {
				for j, p := range PlantNo {
					if PlantNoToRbhStandard[i] == p {
						rbhStandard[j] = true
					}
				}
			}
		}
	}
	return rbhStandard, errList
}
