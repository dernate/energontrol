package energontrol

import (
	"context"
	"fmt"
	"github.com/dernate/gopcxmlda"
	"github.com/joho/godotenv"
	"net/url"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"
)

func TestAvailable(t *testing.T) {
	err := godotenv.Load()
	if err != nil {
		t.Fatal("Error loading .env file")
	}
	OpcUrl := os.Getenv("OPC_URL")
	_url, err := url.Parse(OpcUrl)
	if err != nil {
		t.Errorf("Error: %s", err)
	}

	Server := gopcxmlda.Server{
		Url:      _url,
		LocaleID: "en-us",
		Timeout:  10,
	}
	available, err := serverAvailable(context.Background(), Server)
	if err != nil {
		t.Errorf("Error: %s", err)
	} else {
		t.Log("Test passed", available)
	}
}

func TestSetAction(t *testing.T) {
	plantState := []PlantState{
		{PlantNo: 2, CtrlState: 0},
		{PlantNo: 4, CtrlState: 1},
		{PlantNo: 5, CtrlState: 2},
	}
	setActionToStop(&plantState, true, CtrlValues["Stop60"])
	plantState2 := []PlantState{
		{PlantNo: 2, CtrlState: 0},
		{PlantNo: 4, CtrlState: 1},
		{PlantNo: 5, CtrlState: 2},
	}
	setActionToStop(&plantState2, true, CtrlValues["Stop"])
	plantState3 := []PlantState{
		{PlantNo: 2, CtrlState: 0},
		{PlantNo: 4, CtrlState: 1},
		{PlantNo: 5, CtrlState: 2},
	}
	setActionToStop(&plantState3, false, CtrlValues["Stop60"])
	plantState4 := []PlantState{
		{PlantNo: 2, CtrlState: 0},
		{PlantNo: 4, CtrlState: 1},
		{PlantNo: 5, CtrlState: 1},
	}
	setActionToStart(&plantState4)
	if !(plantState[0].Action == true && plantState[1].Action == false && plantState[2].Action == true) {
		t.Errorf("Error at Stop ForceExplicitCommand: %t, Action: %s", true, "Stop60")
	}
	if !(plantState2[0].Action == true && plantState2[1].Action == true && plantState2[2].Action == false) {
		t.Errorf("Error at Stop ForceExplicitCommand: %t, Action: %s", true, "Stop")
	}
	if !(plantState3[0].Action == true && plantState3[1].Action == false && plantState3[2].Action == false) {
		t.Errorf("Error at Stop ForceExplicitCommand: %t, Action: %s", false, "Stop60")
	}
	if !(plantState4[0].Action == false && plantState4[1].Action == true && plantState4[2].Action == true) {
		t.Errorf("Error at Start")
	} else {
		t.Log("Test passed")
	}
}

func TestStart(t *testing.T) {
	err := godotenv.Load()
	if err != nil {
		t.Fatal("Error loading .env file")
	}
	OpcUrl := os.Getenv("OPC_URL")
	_url, err := url.Parse(OpcUrl)
	if err != nil {
		t.Errorf("Error: %s", err)
	}

	Server := gopcxmlda.Server{
		Url:      _url,
		LocaleID: "en-us",
		Timeout:  10,
	}
	userIdStr := os.Getenv("USERID")
	UserId, err := strconv.ParseUint(userIdStr, 10, 64)
	if err != nil {
		t.Errorf("Error: %s", err)
	}
	PlantNo := []uint8{2, 4}
	started, errList := Start(context.Background(), Server, UserId, PlantNo...)
	if len(errList) > 0 {
		for _, err := range errList {
			if err != nil {
				t.Errorf("Error: %s", err)
			}
		}
	}
	if len(started) == 0 {
		t.Errorf("Return Value of \"started\" empty")
	} else {
		// check if returned values indicate started plants
		for i, s := range started {
			if !s {
				t.Errorf("Error: Plant %d did not start", PlantNo[i])
			}
		}
	}

	// check if plant started
	time.Sleep(10 * time.Second)
	var item []gopcxmlda.TItem
	for _, p := range PlantNo {
		item = append(item, gopcxmlda.TItem{
			ItemName: fmt.Sprintf("Loc/Wec/Plant%d/Ctrl/Ctrl", p),
		})
	}
	var ClientRequestHandle string
	var ClientItemHandles []string
	options := map[string]interface{}{
		"ReturnItemTime": true,
		"returnItemPath": true,
		"returnItemName": true,
	}
	CtrlState, err := Server.Read(context.Background(), item, &ClientRequestHandle, &ClientItemHandles, "", options)
	if err != nil {
		t.Fatal(err)
	} else {
		var bErr bool
		for _, s := range CtrlState.Response.ItemList.Items {
			if s.Value.Value.(uint64) != 0 {
				t.Errorf("Error: Plant %d did not start", s.Value.Value)
				bErr = true
			}
		}
		if !bErr {
			t.Log("Test passed")
		}
	}
}

func TestStop(t *testing.T) {
	err := godotenv.Load()
	if err != nil {
		t.Fatal("Error loading .env file")
	}
	OpcUrl := os.Getenv("OPC_URL")
	_url, err := url.Parse(OpcUrl)
	if err != nil {
		t.Errorf("Error: %s", err)
	}

	Server := gopcxmlda.Server{
		Url:      _url,
		LocaleID: "en-us",
		Timeout:  10,
	}
	userIdStr := os.Getenv("USERID")
	UserId, err := strconv.ParseUint(userIdStr, 10, 64)
	if err != nil {
		t.Errorf("Error: %s", err)
	}
	PlantNo := []uint8{2, 4}
	stopped1, errList1 := Stop(context.Background(), Server, UserId, false, true, PlantNo[0])
	if len(errList1) > 0 {
		for _, err := range errList1 {
			if err != nil {
				t.Errorf("Error: %s", err)
			}
		}
	}
	stopped2, errList2 := Stop(context.Background(), Server, UserId, true, true, PlantNo[1])
	if len(errList2) > 0 {
		for _, err := range errList2 {
			if err != nil {
				t.Errorf("Error: %s", err)
			}
		}
	}
	stopped := append(stopped1, stopped2...)
	if len(stopped) == 0 {
		t.Errorf("Return Value of \"stopped\" empty")
	} else {
		// check if returned values indicate stopped plants
		for i, s := range stopped {
			if !s {
				t.Errorf("Error: Plant %d did not stop", PlantNo[i])
			}
		}
	}

	// check if plant stopped
	time.Sleep(10 * time.Second)
	var items []gopcxmlda.TItem
	for _, p := range PlantNo {
		items = append(items, gopcxmlda.TItem{
			ItemName: fmt.Sprintf("Loc/Wec/Plant%d/Ctrl/Ctrl", p),
		})
	}
	var ClientRequestHandle string
	var ClientItemHandles []string
	options := map[string]interface{}{
		"ReturnItemTime": true,
		"returnItemPath": true,
		"returnItemName": true,
	}
	CtrlState, err := Server.Read(context.Background(), items, &ClientRequestHandle, &ClientItemHandles, "", options)
	if err != nil {
		t.Fatal(err)
	} else {
		var bErr bool
		if CtrlState.Response.ItemList.Items[0].Value.Value.(uint64) != 1 {
			t.Errorf("Error: Plant %d did not stop", PlantNo[0])
			bErr = true
		}
		if CtrlState.Response.ItemList.Items[1].Value.Value.(uint64) != 2 {
			t.Errorf("Error: Plant %d did not stop", PlantNo[1])
			bErr = true
		}
		if !bErr {
			t.Log("Test passed")
		}
	}
}

func TestSessionState(t *testing.T) {
	err := godotenv.Load()
	if err != nil {
		t.Fatal("Error loading .env file")
	}
	OpcUrl := os.Getenv("OPC_URL")
	_url, err := url.Parse(OpcUrl)
	if err != nil {
		t.Errorf("Error: %s", err)
	}

	Server := gopcxmlda.Server{
		Url:      _url,
		LocaleID: "en-us",
		Timeout:  10,
	}
	PlantNo := []uint8{1, 3, 4}
	WaitFor := WaitForState{}
	s, err := sessionState(context.Background(), Server, "Ctrl", WaitFor, PlantNo...)
	if err != nil {
		t.Errorf("Error: %s", err)
	} else {
		t.Log(s)
		t.Log("Test passed")
	}
}

func TestGetSessionStateText(t *testing.T) {
	states := []uint16{0, 1, 2, 4, 5, 175, 234}
	expected := []string{
		"Session is Session free",
		"Session is Session reserved",
		"Session is Parameter input",
		"Session is Waiting time session end",
		"Session is Session blocked (global reservation)",
		"Session is Insufficient rights",
		"Unknown session state",
	}
	for i, state := range states {
		s := getSessionStateText(state)
		if s != expected[i] {
			t.Errorf("Error: %s ; %s", s, expected[i])
		}
	}
	t.Log("Test passed")
}

func TestReset(t *testing.T) {
	err := godotenv.Load()
	if err != nil {
		t.Fatal("Error loading .env file")
	}
	OpcUrl := os.Getenv("OPC_URL")
	_url, err := url.Parse(OpcUrl)
	if err != nil {
		t.Errorf("Error: %s", err)
	}

	Server := gopcxmlda.Server{
		Url:      _url,
		LocaleID: "en-us",
		Timeout:  10,
	}
	PlantNo := []uint8{2}
	userIdStr := os.Getenv("USERID")
	UserId, err := strconv.ParseUint(userIdStr, 10, 64)
	if err != nil {
		t.Errorf("Error: %s", err)
	}
	resetted, errList := Reset(context.Background(), Server, UserId, PlantNo...)
	if len(errList) > 0 {
		for _, err := range errList {
			if err != nil {
				t.Errorf("Error: %s", err)
			}
		}
	}
	if len(resetted) == 0 {
		t.Errorf("Return Value of \"resetted\" empty")
	} else {
		// check if returned values indicate resetted plants
		var bErr bool
		for i, r := range resetted {
			if !r {
				t.Errorf("Error: Plant %d did not reset", PlantNo[i])
				bErr = true
			}
		}
		if !bErr {
			t.Log("Test passed")
		} else {
			t.Error("Test failed")
		}
	}
}

func TestGetRbhStateText(t *testing.T) {
	states := []uint64{RbhNoAccess, RbhAutoDeicingAllowed + RbhInstalled, RbhAutoOffWEA + RbhInstalled, RbhAutoOffWEA + RbhManualOnSCADA + RbhInstalled}
	expected := [][]string{
		{RbhStatus[RbhNoAccess]},
		{RbhStatus[RbhAutoDeicingAllowed], RbhStatus[RbhInstalled]},
		{RbhStatus[RbhAutoOffWEA], RbhStatus[RbhInstalled]},
		{RbhStatus[RbhAutoOffWEA], RbhStatus[RbhManualOnSCADA], RbhStatus[RbhInstalled]},
	}
	var bErr bool
	for i, state := range states {
		s := getRbhStateText(state)
		if len(s) != len(expected[i]) {
			t.Errorf("Error: %s ; %s", s, expected[i])
			bErr = true
		} else {
			sort.Strings(s)
			sort.Strings(expected[i])
			// check if the same values are in the slices
			for j := range s {
				if s[j] != expected[i][j] {
					t.Errorf("Error: %s ; %s", s, expected[i])
					bErr = true
					break
				}
			}
		}
	}
	if !bErr {
		t.Log("Test passed")
	}
}

func TestRbhStatusRight(t *testing.T) {
	actual := []uint64{
		RbhInstalled + RbhAutoDeicingAllowed,
		RbhInstalled + RbhAutoOffWEA,
		RbhInstalled + RbhAutoOffWEA + RbhManualOnSCADA,
		RbhInstalled + RbhAutoDeicingAllowed + RbhHeatingInOperationSCADA,
	}
	desired := []uint64{0, 2, 10}
	ret1 := rbhStatusRight(actual[0], desired[0])
	ret2 := rbhStatusRight(actual[0], desired[1])
	ret3 := rbhStatusRight(actual[0], desired[2])
	if !ret1 || ret2 || ret3 {
		t.Errorf("Error: %t ; %t ; %t", ret1, ret2, ret3)
	}
	ret1 = rbhStatusRight(actual[1], desired[0])
	ret2 = rbhStatusRight(actual[1], desired[1])
	ret3 = rbhStatusRight(actual[1], desired[2])
	if ret1 || !ret2 || ret3 {
		t.Errorf("Error: %t ; %t ; %t", ret1, ret2, ret3)
	}
	ret1 = rbhStatusRight(actual[2], desired[0])
	ret2 = rbhStatusRight(actual[2], desired[1])
	ret3 = rbhStatusRight(actual[2], desired[2])
	if ret1 || ret2 || !ret3 {
		t.Errorf("Error: %t ; %t ; %t", ret1, ret2, ret3)
	}
	ret1 = rbhStatusRight(actual[3], desired[0])
	ret2 = rbhStatusRight(actual[3], desired[1])
	ret3 = rbhStatusRight(actual[3], desired[2])
	if !ret1 || ret2 || !ret3 {
		t.Errorf("Error: %t ; %t ; %t", ret1, ret2, ret3)
	}
}

func TestRbhOn(t *testing.T) {
	err := godotenv.Load()
	if err != nil {
		t.Fatal("Error loading .env file")
	}
	OpcUrl := os.Getenv("OPC_URL")
	_url, err := url.Parse(OpcUrl)
	if err != nil {
		t.Errorf("Error: %s", err)
	}

	Server := gopcxmlda.Server{
		Url:      _url,
		LocaleID: "en-us",
		Timeout:  10,
	}
	userIdStr := os.Getenv("USERID")
	UserId, err := strconv.ParseUint(userIdStr, 10, 64)
	if err != nil {
		t.Errorf("Error: %s", err)
	}
	PlantNo := []uint8{4}
	rbhOn, errList := RbhOn(context.Background(), Server, UserId, PlantNo...)
	if len(errList) > 0 {
		for _, err := range errList {
			if err != nil {
				t.Errorf("Error: %s", err)
			}
		}
	}
	if len(rbhOn) == 0 {
		t.Errorf("Return Value of \"rbhOn\" empty")
	} else {
		// check if returned values indicate rbhOn plants
		var bErr bool
		for i, r := range rbhOn {
			if !r {
				t.Errorf("Error: Rbh of Plant %d did not start", PlantNo[i])
				bErr = true
			}
		}
		if !bErr {
			t.Log("Test passed")
		}
	}
}

func TestRbhAutoOff(t *testing.T) {
	err := godotenv.Load()
	if err != nil {
		t.Fatal("Error loading .env file")
	}
	OpcUrl := os.Getenv("OPC_URL")
	_url, err := url.Parse(OpcUrl)
	if err != nil {
		t.Errorf("Error: %s", err)
	}

	Server := gopcxmlda.Server{
		Url:      _url,
		LocaleID: "en-us",
		Timeout:  10,
	}
	userIdStr := os.Getenv("USERID")
	UserId, err := strconv.ParseUint(userIdStr, 10, 64)
	if err != nil {
		t.Errorf("Error: %s", err)
	}
	PlantNo := []uint8{4}
	rbhAutoOff, errList := RbhAutoOff(context.Background(), Server, UserId, PlantNo...)
	if len(errList) > 0 {
		for _, err := range errList {
			if err != nil {
				t.Errorf("Error: %s", err)
			}
		}
	}
	if len(rbhAutoOff) == 0 {
		t.Errorf("Return Value of \"rbhAutoOff\" empty")
	} else {
		// check if returned values indicate rbhAutoOff plants
		var bErr bool
		for i, r := range rbhAutoOff {
			if !r {
				t.Errorf("Error: Can't set Rbh of Plant %d to AutoOff", PlantNo[i])
				bErr = true
			}
		}
		if !bErr {
			t.Log("Test passed")
		}
	}
}

func TestRbhStandard(t *testing.T) {
	err := godotenv.Load()
	if err != nil {
		t.Fatal("Error loading .env file")
	}
	OpcUrl := os.Getenv("OPC_URL")
	_url, err := url.Parse(OpcUrl)
	if err != nil {
		t.Errorf("Error: %s", err)
	}

	Server := gopcxmlda.Server{
		Url:      _url,
		LocaleID: "en-us",
		Timeout:  10,
	}
	userIdStr := os.Getenv("USERID")
	UserId, err := strconv.ParseUint(userIdStr, 10, 64)
	if err != nil {
		t.Errorf("Error: %s", err)
	}
	PlantNo := []uint8{4}
	rbhStandard, errList := RbhStandard(context.Background(), Server, UserId, PlantNo...)
	if len(errList) > 0 {
		for _, err := range errList {
			if err != nil {
				t.Errorf("Error: %s", err)
			}
		}
	}
	if len(rbhStandard) == 0 {
		t.Errorf("Return Value of \"rbhStandard\" empty")
	} else {
		// check if returned values indicate rbhStandard plants
		var bErr bool
		for i, r := range rbhStandard {
			if !r {
				t.Errorf("Error: Can't set Rbh of Plant %d to Standard", PlantNo[i])
				bErr = true
			}
		}
		if !bErr {
			t.Log("Test passed")
		}
	}
}

func TestControlAndRbh1(t *testing.T) {
	err := godotenv.Load()
	if err != nil {
		t.Fatal("Error loading .env file")
	}
	OpcUrl := os.Getenv("OPC_URL")
	_url, err := url.Parse(OpcUrl)
	if err != nil {
		t.Errorf("Error: %s", err)
	}

	Server := gopcxmlda.Server{
		Url:      _url,
		LocaleID: "en-us",
		Timeout:  10,
	}
	userIdStr := os.Getenv("USERID")
	UserId, err := strconv.ParseUint(userIdStr, 10, 64)
	if err != nil {
		t.Errorf("Error: %s", err)
	}
	PlantNo := []uint8{2, 5}
	Values := ControlAndRbhValue{
		SetCtrlValue: true,
		CtrlValue:    1,
		SetRbhValue:  true,
		RbhValue:     10,
	}
	retControlAndRbh, errList := ControlAndRbh(context.Background(), Server, UserId, Values, PlantNo...)
	var bErr bool
	if len(errList) > 0 {
		for _, err := range errList {
			if err != nil {
				bErr = true
				t.Errorf("Error: %s", err)
			}
		}
	}
	fmt.Println(retControlAndRbh)
	if !bErr {
		t.Log("Test passed")
	}
}

func TestControlAndRbh2(t *testing.T) {
	err := godotenv.Load()
	if err != nil {
		t.Fatal("Error loading .env file")
	}
	OpcUrl := os.Getenv("OPC_URL")
	_url, err := url.Parse(OpcUrl)
	if err != nil {
		t.Errorf("Error: %s", err)
	}

	Server := gopcxmlda.Server{
		Url:      _url,
		LocaleID: "en-us",
		Timeout:  10,
	}
	userIdStr := os.Getenv("USERID")
	UserId, err := strconv.ParseUint(userIdStr, 10, 64)
	if err != nil {
		t.Errorf("Error: %s", err)
	}
	PlantNo := []uint8{2, 5}
	Values := ControlAndRbhValue{
		SetCtrlValue: true,
		CtrlValue:    0,
		SetRbhValue:  true,
		RbhValue:     0,
	}
	retControlAndRbh, errList := ControlAndRbh(context.Background(), Server, UserId, Values, PlantNo...)
	var bErr bool
	if len(errList) > 0 {
		for _, err := range errList {
			if err != nil {
				bErr = true
				t.Errorf("Error: %s", err)
			}
		}
	}
	fmt.Println(retControlAndRbh)
	if !bErr {
		t.Log("Test passed")
	}
}

func TestTurbines(t *testing.T) {
	err := godotenv.Load()
	if err != nil {
		t.Fatal("Error loading .env file")
	}
	OpcUrl := os.Getenv("OPC_URL")
	_url, err := url.Parse(OpcUrl)
	if err != nil {
		t.Errorf("Error: %s", err)
	}

	Server := gopcxmlda.Server{
		Url:      _url,
		LocaleID: "en-us",
		Timeout:  10,
	}
	turbines, err := Turbines(context.Background(), Server)
	if err != nil {
		t.Errorf("Error: %s", err)
	} else {
		t.Log(turbines)
		t.Log("Test passed")
	}
}

func TestParkNoMatch(t *testing.T) {
	err := godotenv.Load()
	if err != nil {
		t.Fatal("Error loading .env file")
	}
	ParkNoStr := os.Getenv("PARKNO")
	ParkNo, err := strconv.ParseUint(ParkNoStr, 10, 64)
	if err != nil {
		t.Errorf("Error: %s", err)
	}

	OpcUrl := os.Getenv("OPC_URL")
	_url, err := url.Parse(OpcUrl)
	if err != nil {
		t.Errorf("Error: %s", err)
	}

	Server := gopcxmlda.Server{
		Url:      _url,
		LocaleID: "en-us",
		Timeout:  10,
	}
	match, err := ParkNoMatch(context.Background(), Server, ParkNo, true)
	if err != nil {
		t.Errorf("Error: %s", err)
	}
	if match {
		t.Log("Test passed, ParkNo match")
	} else {
		t.Error("Test failed, ParkNo does not match")
	}
}
