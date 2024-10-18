package energontrol

import (
	"errors"
	"github.com/dernate/gopcxmlda"
)

func Start(Server gopcxmlda.Server, UserId uint64, PlantNo ...uint8) ([]bool, []error) {
	var errList []error
	var started []bool
	if len(PlantNo) == 0 {
		errList = append(errList, errors.New("no PlantNo provided"))
		return nil, errList
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
	Value := ControlAndRbhValue{
		SetCtrlValue: true,
		CtrlValue:    0,
	}
	for range PlantNoToStart {
		Value.CtrlAction = append(Value.CtrlAction, true)
	}
	startedFiltered, errListFiltered := controlProcedure(Server, UserId, Value, PlantNoToStart...)
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
		errList = append(errList, errors.New("no PlantNo provided"))
		return nil, errList
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
	Value := ControlAndRbhValue{
		SetCtrlValue: true,
		CtrlValue:    CtrlValue,
	}
	for range PlantNoToStop {
		Value.CtrlAction = append(Value.CtrlAction, true)
	}
	stoppedFiltered, errListFiltered := controlProcedure(Server, UserId, Value, PlantNoToStop...)
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
		errList = append(errList, errors.New("no PlantNo provided"))
		return nil, errList
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
		errList = append(errList, errors.New("no PlantNo provided"))
		return nil, errList
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
	Value := ControlAndRbhValue{
		SetRbhValue: true,
		RbhValue:    10,
	}
	for range PlantNoToRbhOn {
		Value.RbhAction = append(Value.RbhAction, true)
	}
	rbhOnFiltered, errListFiltered := controlProcedure(server, UserId, Value, PlantNoToRbhOn...)
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
		errList = append(errList, errors.New("no PlantNo provided"))
		return nil, errList
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
	Value := ControlAndRbhValue{
		SetRbhValue: true,
		RbhValue:    2,
	}
	for range PlantNoToRbhAutoOff {
		Value.RbhAction = append(Value.RbhAction, true)
	}
	rbhAutoOffFiltered, errListFiltered := controlProcedure(server, UserId, Value, PlantNoToRbhAutoOff...)
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
		errList = append(errList, errors.New("no PlantNo provided"))
		return nil, errList
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
	Value := ControlAndRbhValue{
		SetRbhValue: true,
		RbhValue:    0,
	}
	for range PlantNoToRbhStandard {
		Value.RbhAction = append(Value.RbhAction, true)
	}
	rbhStandardFiltered, errListFiltered := controlProcedure(server, UserId, Value, PlantNoToRbhStandard...)
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

// ControlAndRbh Set Ctrl and Rbh values for plants at the same time
func ControlAndRbh(Server gopcxmlda.Server, UserId uint64, Values ControlAndRbhValue, PlantNo ...uint8) ([]bool, []error) {
	var errList []error
	var controlled []bool
	if len(PlantNo) == 0 {
		errList = append(errList, errors.New("no PlantNo provided"))
		return nil, errList
	} else {
		for range PlantNo {
			controlled = append(controlled, false)
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
	var err error
	var CtrlState []PlantState
	var RbhState []PlantState
	if Values.SetCtrlValue {
		CtrlState, err = getPlantCtrlOrRbhState(Server, "Ctrl", PlantNo)
		if err != nil {
			for range PlantNo {
				errList = append(errList, err)
				controlled = append(controlled, false)
			}
			return controlled, errList
		}
		if Values.CtrlValue > 0 {
			setActionToStop(&CtrlState, false, Values.CtrlValue)
		} else {
			setActionToStart(&CtrlState)
		}
		for _, state := range CtrlState {
			if state.Action {
				Values.CtrlAction = append(Values.CtrlAction, true)
			} else {
				Values.CtrlAction = append(Values.CtrlAction, false)
			}
		}
		if allFalse(Values.CtrlAction) {
			Values.SetCtrlValue = false
		}
	}
	if Values.SetRbhValue {
		RbhState, err = getPlantCtrlOrRbhState(Server, "Rbh", PlantNo)
		if err != nil {
			for range PlantNo {
				errList = append(errList, err)
				controlled = append(controlled, false)
			}
			return controlled, errList
		}
		setActionRbh(&RbhState, Values.RbhValue)
		for _, state := range RbhState {
			if state.Action {
				Values.RbhAction = append(Values.RbhAction, true)
			} else {
				Values.RbhAction = append(Values.RbhAction, false)
			}
		}
		if allFalse(Values.RbhAction) {
			Values.SetRbhValue = false
		}
	}
	// Filter plants based on the evaluated Action Bit
	var PlantNoToControl []uint8
	if !Values.SetCtrlValue && !Values.SetRbhValue {
		for i, p := range PlantNo {
			LogInfo(p, "ControlAndRbh", "Ctrl & Rbh of Plant already controlled")
			controlled[i] = true
		}
	} else {
		for i, p := range PlantNo {
			if Values.CtrlAction != nil && Values.CtrlAction[i] {
				PlantNoToControl = append(PlantNoToControl, p)
			} else if Values.RbhAction != nil && Values.RbhAction[i] {
				PlantNoToControl = append(PlantNoToControl, p)
			} else {
				LogInfo(PlantNo[i], "ControlAndRbh", "Ctrl of Plant already controlled")
				controlled[i] = true
			}
		}
	}
	// control plants
	controlledFiltered, errListFiltered := controlProcedure(Server, UserId, Values, PlantNoToControl...)
	if len(errListFiltered) > 0 {
		for i, err := range errListFiltered {
			if err != nil {
				LogError(PlantNoToControl[i], "ControlAndRbh", err.Error())
				for j, p := range PlantNo {
					if PlantNoToControl[i] == p {
						errList[j] = err
					}
				}
			} else if controlledFiltered[i] {
				for j, p := range PlantNo {
					if PlantNoToControl[i] == p {
						controlled[j] = true
					}
				}
			}
		}
	}
	return controlled, errList
}

func Turbines(Server gopcxmlda.Server) (TurbineInfo, error) {
	// check if Server is connected
	if available, err := serverAvailable(Server); !available {
		return TurbineInfo{}, err
	}
	// Browse for all Turbines
	var ClientRequestHandle string
	options := gopcxmlda.TBrowseOptions{}
	b, err := Server.Browse("Loc/Wec", &ClientRequestHandle, "", options)
	if err != nil {
		return TurbineInfo{}, err
	}
	var T TurbineInfo
	T.PlantNo = filterPlants(b)
	err = getPlantInfo(Server, &T)
	if err != nil {
		return T, err
	}
	return T, err
}

// ParkNoMatch Read the Park Number from the Server and compare it with the provided ParkNo
func ParkNoMatch(Server gopcxmlda.Server, ParkNo uint64, checkAvailable bool) (bool, error) {
	if checkAvailable {
		// check if Server is connected
		if available, err := serverAvailable(Server); !available {
			return false, err
		}
	}
	// check if ParkNo is correct
	var handle1 string
	var handle2 []string
	options := map[string]interface{}{
		"returnItemName": true,
	}
	Item := []gopcxmlda.TItem{
		{
			ItemName: "Loc/LocNo",
		},
	}
	value, err := Server.Read(Item, &handle1, &handle2, "", options)
	if err != nil {
		return false, err
	}
	if value.Response.ItemList.Items[0].Value.Value == ParkNo {
		return true, nil
	}
	return false, nil
}
