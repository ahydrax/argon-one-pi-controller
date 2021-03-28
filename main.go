package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"github.com/corrupt/go-smbus"
	"github.com/stianeikeland/go-rpio/v4"
	"github.com/takama/daemon"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	name            = "Argon One Pi Controller"
	description     = "Watches shutdown button and temperature"
	shutdownPin     = 4
	smbusFanAddress byte = 0x1a
)

var dependencies = []string{"multi-user.target"}

var stdlog, errlog *log.Logger

func init() {
	stdlog = log.New(os.Stdout, "", log.Ldate|log.Ltime)
	errlog = log.New(os.Stderr, "", log.Ldate|log.Ltime)
}

type Service struct {
	daemon.Daemon
}

func (service *Service) Manage() (string, error) {

	usage := "Usage: argononepicontroller install | remove | start | stop | status"

	if len(os.Args) > 1 {
		command := os.Args[1]
		switch command {
		case "install":
			return service.Install()
		case "remove":
			return service.Remove()
		case "start":
			return service.Start()
		case "stop":
			return service.Stop()
		case "status":
			return service.Status()
		default:
			return usage, nil
		}
	}

	err := rpio.Open()
	if err != nil {
		return "failed opening gpio", err
	}

	smbus, err := smbus.New(0, smbusFanAddress)
	if err != nil {
		return "failed opening smbus", err
	}

	osInterrupt := make(chan os.Signal, 1)
	signal.Notify(osInterrupt, os.Interrupt, os.Kill, syscall.SIGABRT, syscall.SIGTERM)

	var osSignals = make(chan os.Signal, 1)
	signal.Notify(osSignals)

	var tempChannel = make(chan float64, 1)
	var errChannel = make(chan error, 1)

	appCtx, cancel := context.WithCancel(context.Background())

	go monitorTemperature(appCtx, tempChannel, errChannel)
	go handleTemperature(appCtx, tempChannel, smbus, errChannel)
	go watchShutdownButton(appCtx, errChannel)

	select {
	case killSignal := <-osInterrupt:
		stdlog.Println("Got signal: ", killSignal)
		cancel()
		return "Process finished", nil

	case err := <-errChannel:
		cancel()
		return "failed", err
	}
}

func main() {
	srv, err := daemon.New(name, description, daemon.SystemDaemon, dependencies...)

	if err != nil {
		errlog.Println("Error: ", err)
		os.Exit(1)
	}

	service := &Service{srv}
	status, err := service.Manage()
	if err != nil {
		errlog.Println(status, "\nError: ", err)
		os.Exit(1)
	}
	stdlog.Println(status)
}

func monitorTemperature(ctx context.Context, tempChannel chan float64, errCh chan error) {
	for {
		select {
		case <-ctx.Done():
			stdlog.Println("temperature-watch: cancellation requested")
			return
		default:
			temp, err := getCurrentTemperature()
			if err != nil {
				errCh <- err
				return
			}

			tempChannel <- temp
			time.Sleep(5 * time.Second)
		}
	}
}

func getCurrentTemperature() (float64, error) {

	getTempCommand := exec.Command("vcgencmd", "measure_temp")
	var commandStdout bytes.Buffer

	getTempCommand.Stdout = &commandStdout

	err := getTempCommand.Run()
	if err != nil {
		return 0, err
	}

	commandResult := string(commandStdout.Bytes())

	commandResult = strings.Replace(commandResult, "temp=", "", 1)
	commandResult = strings.Replace(commandResult, "'C", "", 1)

	parsedFloat, err := strconv.ParseFloat(commandResult, 64)
	if err != nil {
		return 0, err
	}

	return parsedFloat, nil
}

func handleTemperature(ctx context.Context, channel chan float64, bus *smbus.SMBus, errCh chan error) {
	for {
		select {
		case <-ctx.Done():
			stdlog.Println("temperature-handler: cancellation requested")
			return

		case temp := <-channel:
			if temp > 50 {
				fanSpeedBytes := make([]byte,4)
				binary.LittleEndian.PutUint32(fanSpeedBytes, 100)
				bus.Write_block_data(smbusFanAddress, fanSpeedBytes)
			}
		}
	}
}

func watchShutdownButton(ctx context.Context, errCh chan error) {

	var rebootCommand = exec.Command("reboot")
	var shutdownCommand = exec.Command("shutdown", "now")

	err := rpio.Open()
	if err != nil {
		errCh <- err
		return
	}

	var shutdownPin = rpio.Pin(shutdownPin)
	rpio.PinMode(shutdownPin, rpio.Input)
	rpio.PullMode(shutdownPin, rpio.PullDown)

	for {
		select {
		case <-ctx.Done():
			stdlog.Println("button-watch: cancellation requested")
			return

		default:
			var sleepTime = time.Millisecond * 100
			var tick = 0.1
			var pulseTime = tick
			shutdownPin.Detect(rpio.RiseEdge)
			time.Sleep(sleepTime)
			for shutdownPin.Read() == rpio.High {
				time.Sleep(sleepTime)
				pulseTime += tick
			}

			if pulseTime >= 2 || pulseTime <= 3 {
				err := rebootCommand.Run()
				if err != nil {
					errCh <- err
				}
			}

			if pulseTime >= 4 {
				err := shutdownCommand.Run()
				if err != nil {
					errCh <- err
				}
			}
		}
	}
}
