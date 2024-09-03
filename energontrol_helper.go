package energontrol

import (
	"fmt"
	"github.com/dernate/gopcxmlda"
	"math/rand"
	"regexp"
	"strconv"
	"time"
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

func getPlantCtrlOrRbhState(Server gopcxmlda.Server, CtrlOrRbh string, PlantNo []uint8) ([]PlantState, error) {
	if CtrlOrRbh != "Ctrl" && CtrlOrRbh != "Rbh" {
		return nil, fmt.Errorf("CtrlOrRbh must be either Ctrl or Rbh")
	}
	// check plant ctrl state
	var handle1 string
	var handle2 []string
	options := map[string]interface{}{
		"returnItemName": true,
	}
	var items []gopcxmlda.T_Item
	for _, plant := range PlantNo {
		items = append(items, gopcxmlda.T_Item{
			ItemName: fmt.Sprintf("Loc/Wec/Plant%d/Ctrl/%s", plant, CtrlOrRbh),
		})
	}
	value, err := Server.Read(items, &handle1, &handle2, "", options)
	if err != nil {
		return nil, err
	} else {
		plantState := make([]PlantState, len(PlantNo))
		for i, item := range value.Body.ReadResponse.RItemList.Items {
			plantState[i].PlantNo = PlantNo[i]
			plantState[i].CtrlState = item.Value.Value.(uint64)
		}
		return plantState, nil
	}
}

func setActionToStart(plantState *[]PlantState) {
	for i, state := range *plantState {
		// If CtrlState is 0, the plant is already started.
		// If CtrlState is 129 or above, we can't start the plant.
		if state.CtrlState == 0 || state.CtrlState > 128 {
			(*plantState)[i].Action = false
			LogIfStateChangePermitted(state, state.PlantNo, 0) // 0=="Start". desiredState is just for the Log Message if even needed
		} else {
			(*plantState)[i].Action = true
		}
	}
}

func setActionToStop(plantState *[]PlantState, ForceExplicitCommand bool, Action uint64) {
	for i, state := range *plantState {
		// If CtrlState is 129 or 130, the plant is already stopped, but we can't force a change.
		// If CtrlState is 255, no one can change the state.
		// If ForceExplicitCommand is true, we can force a change, e.g. from 60° Stop to a 90° Stop or vice versa.
		if (ForceExplicitCommand && state.CtrlState < 129 && state.CtrlState == Action) ||
			(!ForceExplicitCommand && state.CtrlState > 0) {
			(*plantState)[i].Action = false
			if ForceExplicitCommand {
				LogIfStateChangePermitted(state, state.PlantNo, Action)
			} else {
				LogIfStateChangePermitted(state, state.PlantNo, 2) // 2=="Stop". desiredState is just for the Log Message if even needed
			}
		} else {
			(*plantState)[i].Action = true
		}
	}
}

func setActionRbh(plantState *[]PlantState, Action uint64) {
	for i, state := range *plantState {
		if rbhStatusRight(state.CtrlState, Action) {
			(*plantState)[i].Action = false
		} else {
			(*plantState)[i].Action = true
		}
	}
}

func rbhStatusRight(actual uint64, desired uint64) bool {
	RbhBitMaskThatIndicatesRbhIsRunning := RbhManualOnWEA | RbhManualOnSCADA | RbhAutoDeicingWhenStopped | RbhAutoDeicingInOperation | RbhHeatingPreventiveAuto | RbhHeatingWhenStoppedSCADA | RbhHeatingInOperationSCADA
	RbhBitMaskThatIndicateFailure := RbhNotInstalled | RbhNoSupplyPowerAvailable | RbhFault
	switch desired {
	case 0:
		// We can only set 0, 2, and 2+8=10. So, check if 2 is set, if not,
		// then the blade heater is also not running because we can't change that.
		return actual&RbhAutoOffWEA == 0
	case 2:
		// With desired==2 (suppress automatic), neither bit 2^1 nor bit 2^8 should be present.
		// Therefore, ((actual & 8) XOR (actual & 2)) && !(actual & 8)
		/* (actual & 8) | (actual & 2) | A XOR B
		   0     |     0     |    0
		   0     |     1     |    1
		   1     |     0     |    1    // Shouldn't technically occur, but is caught by "&& !(actual & 8)"
		   1     |     1     |    0
		*/
		return (actual&RbhManualOnSCADA != 0) != ((actual&RbhAutoOffWEA) != 0) && (actual&RbhManualOnSCADA) == 0
		//return ((bool)(ist & 8) ^ (bool)(ist & 2)) && !((bool)(ist & 8));
	case 10:
		// Check if any of the bits 2^2 to 2^8 are set and that no interfering bits are set.
		return (actual&RbhBitMaskThatIndicatesRbhIsRunning) != 0 && (actual&RbhBitMaskThatIndicateFailure) == 0
		//return (St & 508) && !(St & 68608);
	default:
		return false
	}
}

func controlProcedure(Server gopcxmlda.Server, UserId uint64, Values ControlAndRbhValue, PlantNo ...uint8) ([]bool, []error) {
	if len(PlantNo) == 0 {
		return nil, nil
	}
	SessionType := "Ctrl"
	Action := "" // Action contains specific Ctrl and/or Rbh action descriptions. Used for Logging.
	if Values.SetCtrlValue {
		for _action, _CtrlValue := range CtrlValues {
			if _CtrlValue == Values.CtrlValue {
				Action = "'Ctrl: " + _action + "'"
				break
			}
		}
	}
	if Values.SetRbhValue {
		for _action, _RbhValue := range RbhValues {
			if _RbhValue == Values.RbhValue {
				if Action != "" {
					Action += " and "
				}
				Action += "'Rbh: " + _action + "'"
				break
			}
		}
	}
	var success []bool
	var errList []error
	for range PlantNo {
		errList = append(errList, nil)
		success = append(success, false)
	}
	// Get session state
	WaitFor := WaitForState{
		Desired: 0,
		Sleep:   100 * time.Millisecond,
		Retries: 10,
	}
	SesState, err := sessionState(Server, SessionType, WaitFor, PlantNo...)
	if err != nil {
		for i := range errList {
			errList[i] = err
			success[i] = false
		}
		return success, errList
	}
	if len(SesState) != len(PlantNo) {
		for i := range errList {
			errList[i] = fmt.Errorf("Session state item count does not match PlantNo")
			success[i] = false
		}
		return success, errList
	}
	var SessionRequestValues []SessionRequest
	for range PlantNo {
		SessionRequestValues = append(SessionRequestValues, SessionRequest{})
	}
	for i, plant := range PlantNo {
		if SesState[i] != 0 {
			errMsg := fmt.Sprintf("Can't start session, %s", getSessionStateText(SesState[i]))
			LogWarn(plant, Action, errMsg)
			errList[i] = fmt.Errorf(errMsg)
			success[i] = false
			continue
		}
		// do session request
		SessionRequestValues[i] = generateSessionRequest(UserId)
		err = requestSession(Server, SessionRequestValues[i], plant, SessionType)
		if err != nil {
			errList[i] = err
			success[i] = false
		}
	}
	// Get new Session State
	WaitFor.Desired = 1
	SesState, err = sessionState(Server, SessionType, WaitFor, PlantNo...)
	if err != nil {
		for i := range errList {
			errList[i] = err
			success[i] = false
		}
		return success, errList
	}
	if len(SesState) != len(PlantNo) {
		for i := range errList {
			errList[i] = fmt.Errorf("Session state item count does not match PlantNo")
			success[i] = false
		}
		return success, errList
	}
	var PublicKeys []uint64
	for range PlantNo {
		PublicKeys = append(PublicKeys, 0)
	}
	for i, plant := range PlantNo {
		if SesState[i] != 1 {
			errMsg := fmt.Sprintf("Session error for Plant %d, %s", plant, getSessionStateText(SesState[i]))
			LogWarn(plant, Action, errMsg)
			errList[i] = fmt.Errorf(errMsg)
			success[i] = false
			continue
		}
		var PublicKey uint64
		PublicKey, err = getPublicKey(Server, plant, SessionType)
		if err != nil {
			PublicKeys[i] = 0
			errList[i] = err
			success[i] = false
			continue
		}
		PublicKeys[i] = PublicKey
		if Values.SetCtrlValue && Values.CtrlAction[i] {
			err = writeControlValue(Server, plant, Values.CtrlValue, SessionRequestValues[i].PrivateKey, PublicKey, "Ctrl")
			if err != nil {
				errList[i] = err
				success[i] = false
				continue
			}
		}
		if Values.SetRbhValue && Values.RbhAction[i] {
			err = writeControlValue(Server, plant, Values.RbhValue, SessionRequestValues[i].PrivateKey, PublicKey, "Rbh")
			if err != nil {
				errList[i] = err
				success[i] = false
				continue
			}
		}
	}

	// Get new Session State
	WaitFor.Desired = 2
	SesState, err = sessionState(Server, SessionType, WaitFor, PlantNo...)
	if err != nil {
		for i := range errList {
			errList[i] = err
			success[i] = false
		}
		return success, errList
	}
	if len(SesState) != len(PlantNo) {
		for i := range errList {
			errList[i] = fmt.Errorf("Session state item count does not match PlantNo")
			success[i] = false
		}
		return success, errList
	}
	for i, plant := range PlantNo {
		if SesState[i] != 2 {
			errMsg := fmt.Sprintf("Session error for Plant %d, %s", plant, getSessionStateText(SesState[i]))
			LogWarn(plant, Action, errMsg)
			errList[i] = fmt.Errorf(errMsg)
			success[i] = false
			continue
		}
		err = submitValue(Server, plant, SessionRequestValues[i].PrivateKey, PublicKeys[i], SessionType)
		if err != nil {
			errList[i] = err
			success[i] = false
			continue
		}
	}
	// Get new Session State
	WaitFor.Desired = 4
	SesState, err = sessionState(Server, SessionType, WaitFor, PlantNo...)
	if err != nil {
		for i := range errList {
			errList[i] = err
			success[i] = false
		}
		return success, errList
	}
	if len(SesState) != len(PlantNo) {
		for i := range errList {
			errList[i] = fmt.Errorf("Session state item count does not match PlantNo")
			success[i] = false
		}
		return success, errList
	}
	for i, plant := range PlantNo {
		if SesState[i] != 4 {
			errMsg := fmt.Sprintf("Session error for Plant %d, %s", plant, getSessionStateText(SesState[i]))
			LogWarn(plant, Action, errMsg)
			errList[i] = fmt.Errorf(errMsg)
			success[i] = false
			continue
		}
		success[i] = true
	}
	return success, errList
}

// Get the session state of a plant
func sessionState(Server gopcxmlda.Server, CtrlOrReset string, WaitFor WaitForState, PlantNo ...uint8) ([]uint16, error) {
	if CtrlOrReset != "Ctrl" && CtrlOrReset != "Reset" {
		return nil, fmt.Errorf("CtrlOrReset must be either Ctrl or Reset")
	}
	// read sessionState
	var stateItems []gopcxmlda.T_Item
	for _, plant := range PlantNo {
		stateItems = append(stateItems, gopcxmlda.T_Item{
			ItemName: fmt.Sprintf("Loc/Wec/Plant%d/%s/SessionState", plant, CtrlOrReset),
		})
	}
	var handle1 string
	var handle2 []string
	options := map[string]interface{}{
		"returnItemName": true,
	}
	var value gopcxmlda.T_Read
	var err error
	var retSessionState []uint16
	for range WaitFor.Retries + 1 {
		value, err = Server.Read(stateItems, &handle1, &handle2, "", options)
		if err != nil {
			return nil, err
		}
		if WaitFor.Retries > 0 {
			bOk := false
			for _, item := range value.Body.ReadResponse.RItemList.Items {
				if WaitFor.Desired != item.Value.Value.(uint16) {
					bOk = false
					break
				} else {
					bOk = true
				}
			}
			if !bOk {
				time.Sleep(WaitFor.Sleep)
				continue
			} else {
				break
			}
		}
	}
	for _, item := range value.Body.ReadResponse.RItemList.Items {
		retSessionState = append(retSessionState, item.Value.Value.(uint16))
	}
	return retSessionState, nil
}

func getSessionStateText(state uint16) string {
	if stateText, exists := sessionStates[state]; exists {
		return fmt.Sprintf("Session is '%s'", stateText)
	} else {
		return "Unknown session state"
	}
}

func getRbhStateText(state uint64) []string {
	var st []string
	if state == 0 {
		return []string{RbhStatus[RbhNoAccess]}
	}
	// Überprüfen der Bitmasken und Hinzufügen der entsprechenden Nachrichten
	for mask, message := range RbhStatus {
		if state&mask != 0 {
			st = append(st, message)
		}
	}
	return st
}

func generateSessionRequest(UserId uint64) SessionRequest {
	var SR SessionRequest
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	SR.SessionId = uint8(r.Intn(20))
	SR.PrivateKey = uint16(r.Intn(32000))
	SR.UserId = UserId
	return SR
}

// requestSession Request a session
func requestSession(Server gopcxmlda.Server, SR SessionRequest, PlantNo uint8, CtrlOrReset string) error {
	if CtrlOrReset != "Ctrl" && CtrlOrReset != "Reset" {
		return fmt.Errorf("CtrlOrReset must be either Ctrl or Reset")
	}
	items := []gopcxmlda.T_Item{
		{
			ItemName: fmt.Sprintf("Loc/Wec/Plant%d/%s/SessionRequest", PlantNo, CtrlOrReset),
			Value: gopcxmlda.T_Value{
				Value: []uint64{uint64(SR.SessionId), SR.UserId, uint64(SR.PrivateKey)},
			},
		},
	}
	var ClientRequestHandle string
	var ClientItemHandles []string
	options := map[string]interface{}{
		"ReturnErrorText": true,
		"ReturnItemName":  true,
		"ReturnItemPath":  true,
	}
	_, err := Server.Write(items, &ClientRequestHandle, &ClientItemHandles, "", options)
	if err != nil {
		return err
	} else {
		return nil
	}
}

func getPublicKey(Server gopcxmlda.Server, PlantNo uint8, CtrlOrReset string) (uint64, error) {
	if CtrlOrReset != "Ctrl" && CtrlOrReset != "Reset" {
		return 0, fmt.Errorf("CtrlOrReset must be either Ctrl or Reset")
	}
	var handle1 string
	var handle2 []string
	options := map[string]interface{}{
		"returnItemName": true,
	}
	items := []gopcxmlda.T_Item{
		{
			ItemName: fmt.Sprintf("Loc/Wec/Plant%d/%s/SessionPubKey", PlantNo, CtrlOrReset),
		},
	}
	value, err := Server.Read(items, &handle1, &handle2, "", options)
	if err != nil {
		return 0, err
	} else if value.Body.ReadResponse.RItemList.Items[0].Value.Value.(uint64) == 0 {
		return 0, fmt.Errorf("public key is 0")
	} else {
		return value.Body.ReadResponse.RItemList.Items[0].Value.Value.(uint64), nil
	}
}

func writeControlValue(Server gopcxmlda.Server, PlantNo uint8, CtrlValue uint64, PrivateKey uint16, PublicKey uint64, CtrlOrRbh string) error {
	if CtrlOrRbh != "Ctrl" && CtrlOrRbh != "Rbh" {
		return fmt.Errorf("CtrlOrRbh must be either Ctrl or Rbh")
	}
	items := []gopcxmlda.T_Item{
		{
			ItemName: fmt.Sprintf("Loc/Wec/Plant%d/Ctrl/Set%s", PlantNo, CtrlOrRbh),
			Value: gopcxmlda.T_Value{
				Value: []uint64{CtrlValue, uint64(PrivateKey), PublicKey},
			},
		},
	}
	var ClientRequestHandle string
	var ClientItemHandles []string
	options := map[string]interface{}{
		"ReturnErrorText": true,
		"ReturnItemName":  true,
		"ReturnItemPath":  true,
	}
	_, err := Server.Write(items, &ClientRequestHandle, &ClientItemHandles, "", options)
	if err != nil {
		return err
	} else {
		return nil
	}
}

func submitValue(Server gopcxmlda.Server, PlantNo uint8, PrivateKey uint16, PublicKey uint64, CtrlOrReset string) error {
	if CtrlOrReset != "Ctrl" && CtrlOrReset != "Reset" {
		return fmt.Errorf("CtrlOrReset must be either Ctrl or Reset")
	}
	items := []gopcxmlda.T_Item{
		{
			ItemName: fmt.Sprintf("Loc/Wec/Plant%d/%s/SessionSubmit", PlantNo, CtrlOrReset),
			Value: gopcxmlda.T_Value{
				Value: []uint64{uint64(PrivateKey), PublicKey},
			},
		},
	}
	var ClientRequestHandle string
	var ClientItemHandles []string
	options := map[string]interface{}{
		"ReturnErrorText": true,
		"ReturnItemName":  true,
		"ReturnItemPath":  true,
	}
	_, err := Server.Write(items, &ClientRequestHandle, &ClientItemHandles, "", options)
	if err != nil {
		return err
	} else {
		return nil
	}
}

func writeResetValue(Server gopcxmlda.Server, PlantNo uint8, PrivateKey uint16, PublicKey uint64) error {
	items := []gopcxmlda.T_Item{
		{
			ItemName: fmt.Sprintf("Loc/Wec/Plant%d/Reset/SetReset", PlantNo),
			Value: gopcxmlda.T_Value{
				Value: []uint64{uint64(PlantNo), uint64(PrivateKey), PublicKey},
			},
		},
	}
	var ClientRequestHandle string
	var ClientItemHandles []string
	options := map[string]interface{}{
		"ReturnErrorText": true,
		"ReturnItemName":  true,
		"ReturnItemPath":  true,
	}
	_, err := Server.Write(items, &ClientRequestHandle, &ClientItemHandles, "", options)
	if err != nil {
		return err
	} else {
		return nil
	}
}

func resetProcedure(Server gopcxmlda.Server, UserId uint64, PlantNo ...uint8) ([]bool, []error) {
	var success []bool
	var errList []error
	SessionType := "Reset"
	Action := "Reset"
	if len(PlantNo) == 0 {
		return nil, nil
	}
	// Get session state
	SesState, err := sessionState(Server, SessionType, WaitForState{}, PlantNo...)
	if err != nil {
		for range PlantNo {
			errList = append(errList, err)
		}
		return nil, errList
	}
	for i, _sessionState := range SesState {
		if _sessionState != 0 {
			errMsg := fmt.Sprintf("Can't start session, %s", getSessionStateText(_sessionState))
			LogWarn(PlantNo[i], Action, errMsg)
			errList = append(errList, fmt.Errorf(errMsg))
			success = append(success, false)
			continue
		}
		// do session request
		SessionRequestValues := generateSessionRequest(UserId)
		err := requestSession(Server, SessionRequestValues, PlantNo[i], SessionType)
		if err != nil {
			errList = append(errList, err)
			success = append(success, false)
			continue
		}
		// Get new Session State
		WaitFor := WaitForState{
			Desired: 1,
			Sleep:   100 * time.Millisecond,
			Retries: 10,
		}

		SesState, err = sessionState(Server, SessionType, WaitFor, PlantNo[i])
		if err != nil {
			errList = append(errList, err)
			success = append(success, false)
			continue
		}
		if SesState[0] != 1 {
			errMsg := fmt.Sprintf("Session error for Plant %d, %s", PlantNo[i], getSessionStateText(SesState[0]))
			LogWarn(PlantNo[i], Action, errMsg)
			errList = append(errList, fmt.Errorf(errMsg))
			success = append(success, false)
			continue
		}
		PublicKey, err := getPublicKey(Server, PlantNo[i], SessionType)
		if err != nil {
			errList = append(errList, err)
			success = append(success, false)
			continue
		}
		err = writeResetValue(Server, PlantNo[i], SessionRequestValues.PrivateKey, PublicKey)
		if err != nil {
			errList = append(errList, err)
			success = append(success, false)
			continue
		}
		// Get new Session State
		WaitFor = WaitForState{
			Desired: 2,
			Sleep:   100 * time.Millisecond,
			Retries: 10,
		}
		SesState, err = sessionState(Server, SessionType, WaitFor, PlantNo[i])
		if err != nil {
			errList = append(errList, err)
			success = append(success, false)
			continue
		}
		if SesState[0] != 2 {
			errMsg := fmt.Sprintf("Session error for Plant %d, %s", PlantNo[i], getSessionStateText(SesState[0]))
			LogWarn(PlantNo[i], Action, errMsg)
			errList = append(errList, fmt.Errorf(errMsg))
			success = append(success, false)
			continue
		}
		err = submitValue(Server, PlantNo[i], SessionRequestValues.PrivateKey, PublicKey, SessionType)
		if err != nil {
			errList = append(errList, err)
			success = append(success, false)
			continue
		}
		// Get new Session State
		WaitFor = WaitForState{
			Desired: 4,
			Sleep:   100 * time.Millisecond,
			Retries: 10,
		}
		SesState, err = sessionState(Server, SessionType, WaitFor, PlantNo[i])
		if err != nil {
			errList = append(errList, err)
			success = append(success, false)
			continue
		}
		if SesState[0] != 4 {
			errMsg := fmt.Sprintf("Session error for Plant %d, %s", PlantNo[i], getSessionStateText(SesState[0]))
			LogWarn(PlantNo[i], Action, errMsg)
			errList = append(errList, fmt.Errorf(errMsg))
			success = append(success, false)
			continue
		} else {
			errList = append(errList, nil)
			success = append(success, true)
		}
	}
	return success, errList
}

// allFalse checks if all values in a slice are false
func allFalse(b []bool) bool {
	for _, value := range b {
		if value {
			return false
		}
	}
	return true
}

func filterPlants(b gopcxmlda.T_Browse) []uint8 {
	var plants []uint8
	re := regexp.MustCompile(`^Loc/Wec/Plant(\d+)$`)
	for _, item := range b.Body.BrowseResponse.Elements {
		if matches := re.FindStringSubmatch(item.ItemName); matches != nil {
			if num, err := strconv.Atoi(matches[1]); err == nil {
				if num >= 0 && num <= 255 {
					plants = append(plants, uint8(num))
				}
			}
		}
	}
	return plants
}

func getPlantInfo(Server gopcxmlda.Server, T *TurbineInfo) error {
	if T.Ctrl == nil {
		T.Ctrl = make(map[uint8]bool)
	}
	if T.Para == nil {
		T.Para = make(map[uint8]bool)
	}
	if T.Rbh == nil {
		T.Rbh = make(map[uint8]bool)
	}
	if T.Reset == nil {
		T.Reset = make(map[uint8]bool)
	}
	if T.IceDet == nil {
		T.IceDet = make(map[uint8]bool)
	}
	var ClientRequestHandle string
	var ClientItemHandles []string
	_parkNo, err := Server.Read([]gopcxmlda.T_Item{
		{
			ItemName: "Loc/LocNo",
		},
	}, &ClientRequestHandle, &ClientItemHandles, "", map[string]interface{}{
		"returnItemName": true,
	})
	if err != nil {
		return err
	}
	if len(_parkNo.Body.ReadResponse.RItemList.Items) == 0 {
		return fmt.Errorf("ParkNo not found")
	}
	T.ParkNo = _parkNo.Body.ReadResponse.RItemList.Items[0].Value.Value.(uint64)
	for _, plant := range T.PlantNo {
		optionsBranch := gopcxmlda.T_BrowseOptions{
			BrowseFilter: "branch",
		}
		b, err := Server.Browse(fmt.Sprintf("Loc/Wec/Plant%d", plant), &ClientRequestHandle, "", optionsBranch)
		if err != nil {
			return err
		}
		for _, item := range b.Body.BrowseResponse.Elements {
			if item.Name == "Ctrl" && item.HasChildren {
				b2, err := Server.Browse(fmt.Sprintf("Loc/Wec/Plant%d/Ctrl", plant), &ClientRequestHandle, "", gopcxmlda.T_BrowseOptions{
					ElementNameFilter: "Set*",
				})
				if err != nil {
					return err
				}
				for _, item2 := range b2.Body.BrowseResponse.Elements {
					if item2.Name == "SetCtrl" {
						T.Ctrl[plant] = true
					}
					if item2.Name == "SetRbh" {
						T.Rbh[plant] = true
					}
					if item2.Name == "SetIceDet" {
						T.IceDet[plant] = true
					}
				}
			}
			if item.Name == "Reset" && item.HasChildren {
				b2, err := Server.Browse(fmt.Sprintf("Loc/Wec/Plant%d/Reset", plant), &ClientRequestHandle, "", gopcxmlda.T_BrowseOptions{
					ElementNameFilter: "SetReset",
				})
				if err != nil {
					return err
				}
				for _, item2 := range b2.Body.BrowseResponse.Elements {
					if item2.Name == "SetReset" {
						T.Reset[plant] = true
					}
				}
			}
			if item.Name == "Para" && item.HasChildren {
				T.Para[plant] = true
			}
		}
	}
	return nil
}
