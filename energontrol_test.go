package energontrol

import (
	"fmt"
	"github.com/dernate/gopcxmlda"
	"github.com/joho/godotenv"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestAvailable(t *testing.T) {
	err := godotenv.Load()
	if err != nil {
		t.Fatal("Error loading .env file")
	}
	OPCIP := os.Getenv("IP")
	OPCPort := os.Getenv("PORT")

	Server := gopcxmlda.Server{
		Addr:     OPCIP,
		Port:     OPCPort,
		LocaleID: "en-us",
		Timeout:  10,
	}
	available, err := serverAvailable(Server)
	if err != nil {
		t.Errorf("Error: %s", err)
	} else {
		t.Log("Test passed", available)
	}
}

func TestSetAction(t *testing.T) {
	plantState := []PlantCtrlState{
		{PlantNo: 2, CtrlState: 0},
		{PlantNo: 4, CtrlState: 1},
		{PlantNo: 5, CtrlState: 2},
	}
	setActionToStop(&plantState, true, CtrlValues["Stop60"])
	plantState2 := []PlantCtrlState{
		{PlantNo: 2, CtrlState: 0},
		{PlantNo: 4, CtrlState: 1},
		{PlantNo: 5, CtrlState: 2},
	}
	setActionToStop(&plantState2, true, CtrlValues["Stop"])
	plantState3 := []PlantCtrlState{
		{PlantNo: 2, CtrlState: 0},
		{PlantNo: 4, CtrlState: 1},
		{PlantNo: 5, CtrlState: 2},
	}
	setActionToStop(&plantState3, false, CtrlValues["Stop60"])
	plantState4 := []PlantCtrlState{
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
	OPCIP := os.Getenv("IP")
	OPCPort := os.Getenv("PORT")

	Server := gopcxmlda.Server{
		Addr:     OPCIP,
		Port:     OPCPort,
		LocaleID: "en-us",
		Timeout:  10,
	}
	userIdStr := os.Getenv("USERID")
	UserId, err := strconv.ParseUint(userIdStr, 10, 64)
	if err != nil {
		t.Errorf("Error: %s", err)
	}
	PlantNo := []uint8{2, 4}
	started, errList := Start(Server, UserId, PlantNo...)
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
	var item []gopcxmlda.T_Item
	for _, p := range PlantNo {
		item = append(item, gopcxmlda.T_Item{
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
	CtrlState, err := Server.Read(item, &ClientRequestHandle, &ClientItemHandles, "", options)
	if err != nil {
		t.Fatal(err)
	} else {
		var bErr bool
		for _, s := range CtrlState.Body.ReadResponse.RItemList.Items {
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
	OPCIP := os.Getenv("IP")
	OPCPort := os.Getenv("PORT")

	Server := gopcxmlda.Server{
		Addr:     OPCIP,
		Port:     OPCPort,
		LocaleID: "en-us",
		Timeout:  10,
	}
	userIdStr := os.Getenv("USERID")
	UserId, err := strconv.ParseUint(userIdStr, 10, 64)
	if err != nil {
		t.Errorf("Error: %s", err)
	}
	PlantNo := []uint8{2, 4}
	stopped1, errList1 := Stop(Server, UserId, false, true, PlantNo[0])
	if len(errList1) > 0 {
		for _, err := range errList1 {
			if err != nil {
				t.Errorf("Error: %s", err)
			}
		}
	}
	stopped2, errList2 := Stop(Server, UserId, true, true, PlantNo[1])
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
	var items []gopcxmlda.T_Item
	for _, p := range PlantNo {
		items = append(items, gopcxmlda.T_Item{
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
	CtrlState, err := Server.Read(items, &ClientRequestHandle, &ClientItemHandles, "", options)
	if err != nil {
		t.Fatal(err)
	} else {
		var bErr bool
		if CtrlState.Body.ReadResponse.RItemList.Items[0].Value.Value.(uint64) != 1 {
			t.Errorf("Error: Plant %d did not stop", PlantNo[0])
			bErr = true
		}
		if CtrlState.Body.ReadResponse.RItemList.Items[1].Value.Value.(uint64) != 2 {
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
	OPCIP := os.Getenv("IP")
	OPCPort := os.Getenv("PORT")

	Server := gopcxmlda.Server{
		Addr:     OPCIP,
		Port:     OPCPort,
		LocaleID: "en-us",
		Timeout:  10,
	}
	PlantNo := []uint8{1, 3, 4}
	WaitFor := WaitForSessionState{}
	s, err := sessionState(Server, "Ctrl", WaitFor, PlantNo...)
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
