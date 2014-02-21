package jobs

import (
	"fmt"
	"github.com/smarterclayton/geard/gears"
	"github.com/smarterclayton/geard/systemd"
	"github.com/smarterclayton/geard/utils"
	"github.com/smarterclayton/go-systemd/dbus"
	"io"
	"log"
	"reflect"
	"time"
)

type BuildImageJobRequest struct {
	JobResponse
	JobRequest
	Source    string
	BaseImage string
	Tag       string
	Data      *ExtendedBuildImageData
}

type ExtendedBuildImageData struct {
	RuntimeImage string
	Clean        bool
	Verbose      bool
}

const buildImage = "pmorie/sti-builder"

func (j *BuildImageJobRequest) Execute() {
	w := j.SuccessWithWrite(JobResponseAccepted, true)

	fmt.Fprintf(w, "Processing build-image request:\n")
	// TODO: download source, add bind-mount

	unitName := j.RequestId.UnitNameForBuild()
	unitDescription := fmt.Sprintf("Builder for %s", j.Tag)

	stdout, err := gears.ProcessLogsForUnit(unitName)
	if err != nil {
		stdout = utils.EmptyReader
		log.Printf("job_build_image: Unable to fetch build logs: %s, %+v", err.Error(), err)
	}
	defer stdout.Close()

	conn, errc := systemd.NewSystemdConnection()
	if errc != nil {
		log.Print("job_build_image:", errc)
		fmt.Fprintf(w, "Unable to watch start status", errc)
		return
	}

	if err := conn.Subscribe(); err != nil {
		log.Print("job_build_image:", err)
		fmt.Fprintf(w, "Unable to watch start status", errc)
		return
	}
	defer conn.Unsubscribe()

	// make subscription global for efficiency
	changes, errch := conn.SubscribeUnitsCustom(1*time.Second, 2,
		func(s1 *dbus.UnitStatus, s2 *dbus.UnitStatus) bool {
			return true
		},
		func(unit string) bool {
			return unit != unitName
		})

	fmt.Fprintf(w, "Running sti build unit: %s\n", unitName)

	startCmd := []string{
		"/usr/bin/docker", "run",
		"-rm",
		"-v", "/run/docker.sock:/run/docker.sock",
		"-t", buildImage,
		"sti", "build", j.Source, j.BaseImage, j.Tag,
		"--url", "unix:///run/docker.sock",
	}

	if j.Data.RuntimeImage != "" {
		startCmd = append(startCmd, "--runtime-image")
		startCmd = append(startCmd, j.Data.RuntimeImage)
	}

	if j.Data.Clean {
		startCmd = append(startCmd, "--clean")
	}

	if j.Data.Verbose {
		startCmd = append(startCmd, "-l")
		startCmd = append(startCmd, "DEBUG")
	}

	status, err := systemd.SystemdConnection().StartTransientUnit(
		unitName,
		"fail",
		dbus.PropExecStart(startCmd, true),
		dbus.PropDescription(unitDescription),
		dbus.PropRemainAfterExit(true),
		dbus.PropSlice("gear.slice"),
	)

	if err != nil {
		errType := reflect.TypeOf(err)
		fmt.Fprintf(w, "Unable to start build container for this image due to (%s): %s\n", errType, err.Error())
		return
	} else if status != "done" {
		fmt.Fprintf(w, "Build did not complete successfully: %s\n", status)
	} else {
		fmt.Fprintf(w, "Sti build is running\n")
	}

	go io.Copy(w, stdout)

wait:
	for {
		select {
		case c := <-changes:
			if changed, ok := c[unitName]; ok {
				if changed.SubState != "running" {
					fmt.Fprintf(w, "Build completed\n", changed.SubState)
					break wait
				}
			}
		case err := <-errch:
			fmt.Fprintf(w, "Error %+v\n", err)
		case <-time.After(25 * time.Second):
			log.Print("job_build_image:", "timeout")
			break wait
		}
	}

	stdout.Close()
}
