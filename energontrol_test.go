package energontrol

import (
	"github.com/dernate/gopcxmlda"
	"github.com/joho/godotenv"
	"os"
	"testing"
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
	UserId := os.Getenv("USERID")
	PlantNo := []uint8{1, 3, 4}
	_, err = Start(Server, UserId, PlantNo...)
	if err != nil {
		t.Errorf("Error: %s", err)
	} else {
		t.Log("Test passed")
	}
}
