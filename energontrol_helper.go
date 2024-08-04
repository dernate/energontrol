package energontrol

import (
	"fmt"
	"github.com/dernate/gopcxmlda"
	"math/rand"
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
			plantState[i].CtrlState = item.Value.Value.(uint32)
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

func setActionToStop(plantState *[]PlantState, ForceExplicitCommand bool, Action uint32) {
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

func setActionRbh(plantState *[]PlantState, Action uint32) {
	for i, state := range *plantState {
		if rbhStatusRight(state.CtrlState, Action) {
			(*plantState)[i].Action = false
		} else {
			(*plantState)[i].Action = true
		}
	}
}

func rbhStatusRight(actual uint32, desired uint32) bool {
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

func controlProcedure(Server gopcxmlda.Server, UserId uint64, CtrlOrRbhValue uint32, CtrlOrRbh string, PlantNo ...uint8) ([]bool, []error) {
	var success []bool
	var errList []error
	SessionType := "Ctrl"
	var ControlType string
	var Action string // Text for the Ctrl/Rbh Value
	if CtrlOrRbh != "Ctrl" && CtrlOrRbh != "Rbh" {
		return nil, []error{fmt.Errorf("CtrlOrRbh must be either Ctrl or Rbh")}
	}
	if CtrlOrRbh == "Ctrl" {
		ControlType = "Ctrl"
		for _action, _CtrlValue := range CtrlValues {
			if _CtrlValue == CtrlOrRbhValue {
				Action = _action
				break
			}
		}
	} else {
		ControlType = "Rbh"
		for _action, _RbhValue := range RbhValues {
			if _RbhValue == uint64(CtrlOrRbhValue) {
				Action = _action
				break
			}
		}
	}
	if Action == "" {
		for range PlantNo {
			errList = append(errList, fmt.Errorf("CtrlValue (%d) cannot be set because it is invalid", CtrlOrRbhValue))
		}
		return nil, errList
	}
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
		err = writeControlValue(Server, PlantNo[i], CtrlOrRbhValue, SessionRequestValues.PrivateKey, PublicKey, ControlType)
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
			if WaitFor.Retries > 0 {

			}
			errList = append(errList, nil)
			success = append(success, true)
		}
	}
	return success, errList
}

// Get the session state of a plant
func sessionState(Server gopcxmlda.Server, CtrlOrReset string, WaitFor WaitForState, PlantNo ...uint8) ([]uint32, error) {
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
	for range WaitFor.Retries + 1 {
		value, err = Server.Read(stateItems, &handle1, &handle2, "", options)
		if err != nil {
			return nil, err
		}
		if len(PlantNo) == 1 && WaitFor.Retries > 0 {
			if WaitFor.Desired == value.Body.ReadResponse.RItemList.Items[0].Value.Value.(uint32) {
				break
			} else {
				time.Sleep(WaitFor.Sleep)
			}
		} else {
			break
		}
	}
	var bSessionState []uint32
	for _, item := range value.Body.ReadResponse.RItemList.Items {
		bSessionState = append(bSessionState, item.Value.Value.(uint32))
	}
	return bSessionState, nil
}

func getSessionStateText(state uint32) string {
	if stateText, exists := sessionStates[state]; exists {
		return fmt.Sprintf("Session is '%s'", stateText)
	} else {
		return "Unknown session state"
	}
}

func getRbhStateText(state uint32) []string {
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

func writeControlValue(Server gopcxmlda.Server, PlantNo uint8, CtrlValue uint32, PrivateKey uint16, PublicKey uint64, CtrlOrRbh string) error {
	if CtrlOrRbh != "Ctrl" && CtrlOrRbh != "Rbh" {
		return fmt.Errorf("CtrlOrRbh must be either Ctrl or Rbh")
	}
	items := []gopcxmlda.T_Item{
		{
			ItemName: fmt.Sprintf("Loc/Wec/Plant%d/Ctrl/Set%s", PlantNo, CtrlOrRbh),
			Value: gopcxmlda.T_Value{
				Value: []uint64{uint64(CtrlValue), uint64(PrivateKey), PublicKey},
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
