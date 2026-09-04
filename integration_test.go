//go:build opc_integration

// These tests send real control commands to real wind turbines. They are behind
// a build tag and an explicit environment guard, because in v1 they ran on a
// plain `go test ./...` — a CI job, or the "run all tests" button of an IDE, was
// enough to start, stop and reset turbines with hard-coded plant numbers.
//
// To run them:
//
//	export OPC_URL=http://scada.example:8080/DA
//	export USERID=1234
//	export PARKNO=5678                       # the park these tests may command
//	export ENERGONTROL_TEST_PLANTS=2,4       # the plants these tests may command
//	export ENERGONTROL_ALLOW_LIVE_CONTROL=yes-i-know
//	go test -tags opc_integration -run TestLive -v ./...
//
// Every test verifies the park number before it sends anything, so a set of
// credentials pointed at the wrong park stops the run instead of commanding
// somebody else's turbines.

package energontrol

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dernate/gopcxmlda"
	"github.com/joho/godotenv"
)

type liveEnv struct {
	client *Client
	server *gopcxmlda.Server
	userID uint64
	parkNo uint64
	plants []uint8
}

// requireLive builds the live client, or skips the test.
func requireLive(t *testing.T, commanding bool) liveEnv {
	t.Helper()
	_ = godotenv.Load()

	if commanding && os.Getenv("ENERGONTROL_ALLOW_LIVE_CONTROL") != "yes-i-know" {
		t.Skip("live control commands require ENERGONTROL_ALLOW_LIVE_CONTROL=yes-i-know")
	}
	rawURL := os.Getenv("OPC_URL")
	if rawURL == "" {
		t.Skip("OPC_URL is not set")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("OPC_URL: %v", err)
	}
	server := &gopcxmlda.Server{Url: parsed, LocaleID: "en-us", Timeout: 10 * time.Second}

	env := liveEnv{
		client: New(server, WithLogger(testLogger(t)), WithMaxStateAge(60*time.Second)),
		server: server,
	}
	if v := os.Getenv("USERID"); v != "" {
		if env.userID, err = strconv.ParseUint(v, 10, 64); err != nil {
			t.Fatalf("USERID: %v", err)
		}
	}
	if v := os.Getenv("PARKNO"); v != "" {
		if env.parkNo, err = strconv.ParseUint(v, 10, 64); err != nil {
			t.Fatalf("PARKNO: %v", err)
		}
	} else if commanding {
		t.Skip("PARKNO must name the park these tests may command")
	}
	for _, field := range strings.Split(os.Getenv("ENERGONTROL_TEST_PLANTS"), ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		n, err := strconv.ParseUint(field, 10, 8)
		if err != nil {
			t.Fatalf("ENERGONTROL_TEST_PLANTS: %v", err)
		}
		env.plants = append(env.plants, uint8(n))
	}
	if commanding {
		if len(env.plants) == 0 {
			t.Skip("ENERGONTROL_TEST_PLANTS must name the plants these tests may command")
		}
		// Never command a park other than the one the operator named.
		match, err := env.client.ParkNoMatch(context.Background(), env.parkNo, true)
		if err != nil {
			t.Fatalf("verifying the park number: %v", err)
		}
		if !match {
			t.Fatalf("the server does not serve park %d — refusing to send commands", env.parkNo)
		}
	}
	return env
}

func TestLiveServerAvailable(t *testing.T) {
	env := requireLive(t, false)
	if err := env.client.ServerAvailable(context.Background()); err != nil {
		t.Fatalf("ServerAvailable: %v", err)
	}
}

func TestLiveTurbines(t *testing.T) {
	env := requireLive(t, false)
	info, err := env.client.Turbines(context.Background())
	if err != nil {
		t.Fatalf("Turbines: %v", err)
	}
	t.Logf("park %d, %d plants", info.ParkNo, len(info.PlantNo))
	for _, p := range info.PlantNo {
		t.Logf("plant %3d: ctrl=%t rbh=%t reset=%t para=%t icedet=%t",
			p, info.Ctrl[p], info.Rbh[p], info.Reset[p], info.Para[p], info.IceDet[p])
	}
}

func TestLivePlantState(t *testing.T) {
	env := requireLive(t, false)
	if len(env.plants) == 0 {
		t.Skip("ENERGONTROL_TEST_PLANTS is not set")
	}
	ctx := context.Background()
	states, err := env.client.PlantCtrlState(ctx, env.plants...)
	if err != nil {
		t.Fatalf("PlantCtrlState: %v", err)
	}
	for _, s := range states {
		t.Logf("plant %d: %s", s.PlantNo, s.Ctrl)
	}
	rbh, err := env.client.PlantRbhState(ctx, env.plants...)
	if err != nil {
		t.Fatalf("PlantRbhState: %v", err)
	}
	for _, s := range rbh {
		t.Logf("plant %d heating: %s", s.PlantNo, RbhStatusString(s.Status))
	}
}

func TestLiveStopThenStart(t *testing.T) {
	env := requireLive(t, true)
	ctx := context.Background()

	before, err := env.client.PlantCtrlState(ctx, env.plants...)
	if err != nil {
		t.Fatalf("reading the state before the test: %v", err)
	}
	t.Cleanup(func() {
		// Put every plant back the way it was found.
		for _, s := range before {
			if s.Ctrl != CtrlStart {
				continue
			}
			if _, err := env.client.Start(context.Background(), env.userID, s.PlantNo); err != nil {
				t.Errorf("restoring plant %d: %v", s.PlantNo, err)
			}
		}
	})

	stopped, err := env.client.Stop(ctx, env.userID, true, true, env.plants...)
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for _, r := range stopped {
		t.Logf("stop: %s", r)
		if !r.InRequestedState() {
			t.Errorf("plant %d did not stop: %v", r.PlantNo, r.Err)
		}
	}
	assertCtrlState(t, env, CtrlStop90)

	started, err := env.client.Start(ctx, env.userID, env.plants...)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, r := range started {
		t.Logf("start: %s", r)
	}
}

func TestLiveRbhCycle(t *testing.T) {
	env := requireLive(t, true)
	ctx := context.Background()

	before, err := env.client.PlantRbhState(ctx, env.plants...)
	if err != nil {
		t.Fatalf("reading the heating state: %v", err)
	}
	t.Cleanup(func() {
		if _, err := env.client.RbhStandard(context.Background(), env.userID, env.plants...); err != nil {
			t.Errorf("restoring the heating to standard: %v", err)
		}
	})
	for _, s := range before {
		t.Logf("plant %d heating before: %s", s.PlantNo, RbhStatusString(s.Status))
	}
	res, err := env.client.RbhOn(ctx, env.userID, env.plants...)
	if err != nil {
		t.Fatalf("RbhOn: %v", err)
	}
	for _, r := range res {
		t.Logf("rbh on: %s", r)
	}
}

func TestLiveReset(t *testing.T) {
	env := requireLive(t, true)
	res, err := env.client.Reset(context.Background(), env.userID, env.plants...)
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	for _, r := range res {
		t.Logf("reset: %s", r)
	}
}

// assertCtrlState waits for the SCADA to reflect a commanded state.
func assertCtrlState(t *testing.T, env liveEnv, want CtrlValue) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		states, err := env.client.PlantCtrlState(context.Background(), env.plants...)
		if err != nil {
			t.Fatalf("PlantCtrlState: %v", err)
		}
		all := true
		for _, s := range states {
			if s.Ctrl != want {
				all = false
				t.Logf("plant %d is %s, waiting for %s", s.PlantNo, s.Ctrl, want)
			}
		}
		if all {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("plants did not reach %s within the timeout", want)
			return
		}
		time.Sleep(2 * time.Second)
	}
}
