// +build integration

package tests

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openshift/geard/containers"
	"github.com/openshift/geard/docker"
	"github.com/openshift/geard/systemd"
	chk "launchpad.net/gocheck"
)

const (
	TimeoutContainerStateChange = time.Second * 15
	TimeoutDockerStateChange    = time.Second * 5
	TimeoutDockerWait           = time.Second * 2

	IntervalContainerCheck = time.Second / 20
	IntervalHttpCheck      = time.Second / 10

	TestImage = "pmorie/sti-html-app"
	EnvImage  = "ccoleman/envtest"
)

//Hookup gocheck with go test
func Test(t *testing.T) {
	chk.TestingT(t)
}

var _ = chk.Suite(&IntegrationTestSuite{})

type IntegrationTestSuite struct {
	dockerClient  *docker.DockerClient
	daemonURI     string
	containerIds  []containers.Identifier
	repositoryIds []string
	sdconn        systemd.Systemd
}

func (s *IntegrationTestSuite) assertFilePresent(c *chk.C, path string, perm os.FileMode, readableByNobodyUser bool) {
	info, err := os.Stat(path)
	c.Assert(err, chk.IsNil)
	if (info.Mode() & os.ModeSymlink) != 0 {
		linkedFile, err := os.Readlink(path)
		c.Assert(err, chk.IsNil)
		s.assertFilePresent(c, linkedFile, perm, readableByNobodyUser)
	} else {
		if info.Mode().Perm() != perm {
			c.Errorf("File %s has permission \"%s\" but expected \"%s\"", path, info.Mode().String(), perm.String())
		}
	}

	if readableByNobodyUser {
		for i := path; i != "/"; i = filepath.Dir(i) {
			info, err = os.Stat(i)
			c.Assert(err, chk.IsNil)
			c.Assert(info.Mode().Perm()&0005, chk.Not(chk.Equals), 0)
		}
	}
}

func (s *IntegrationTestSuite) assertFileAbsent(c *chk.C, path string) {
	c.Logf("assertFileAbsent(%v,%v,%v)", path)
	_, err := os.Stat(path)
	c.Assert(err, chk.Not(chk.IsNil))
}

func (s *IntegrationTestSuite) getContainerPid(id containers.Identifier) int {
	container, err := s.dockerClient.InspectContainer(id.ContainerFor())
	if err != nil {
		return 0
	}
	return container.State.Pid
}

const (
	CONTAINER_CREATED ContainerState = iota
	CONTAINER_STARTED
	CONTAINER_RESTARTED
	CONTAINER_STOPPED
)

type ContainerState int

func (c ContainerState) String() string {
	switch c {
	case CONTAINER_CREATED:
		return "created"
	case CONTAINER_STARTED:
		return "started"
	case CONTAINER_RESTARTED:
		return "restarted"
	case CONTAINER_STOPPED:
		return "stopped"
	default:
		return "unknown"
	}
}

func (s *IntegrationTestSuite) unitState(id containers.Identifier) (string, string) {
	props, err := s.sdconn.GetUnitProperties(id.UnitNameFor())
	if props == nil || err != nil {
		return "", ""
	}
	return props["ActiveState"].(string), props["SubState"].(string)
}

func (s *IntegrationTestSuite) unitTimes(id containers.Identifier) (inactiveStart time.Time, inactiveEnd time.Time, activeStart time.Time, activeEnd time.Time) {
	props, err := s.sdconn.GetUnitProperties(id.UnitNameFor())
	if props == nil || err != nil {
		return
	}
	inactiveStart = time.Unix(int64(props["InactiveEnterTimestampMonotonic"].(uint64)), 0)
	inactiveEnd = time.Unix(int64(props["InactiveExitTimestampMonotonic"].(uint64)), 0)
	activeStart = time.Unix(int64(props["ActiveEnterTimestampMonotonic"].(uint64)), 0)
	activeEnd = time.Unix(int64(props["ActiveExitTimestampMonotonic"].(uint64)), 0)
	return
}

func until(duration, every time.Duration, f func() bool) bool {
	timeout := time.After(duration)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if f() {
				return true
			}
		case <-timeout:
			return false
		}
	}
}

func isContainerAvailable(client *docker.DockerClient, id string) (bool, error) {
	container, err := client.InspectContainer(id)
	if err == docker.ErrNoSuchContainer {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	if container.State.Running && container.State.Pid != 0 {
		return true, nil
	}
	return false, nil
}

func (s *IntegrationTestSuite) assertContainerStarts(c *chk.C, id containers.Identifier) {
	active, _ := s.unitState(id)
	switch active {
	case "active":
		return
	case "activating":
		break
	default:
		c.Errorf("Container %s failed to start - %s", id, active)
		c.FailNow()
		return
	}

	isRunning := func() bool {
		active, sub := s.unitState(id)
		if active == "active" {
			return true
		}
		if active == "activating" {
			return false
		}
		c.Errorf("Unit %s start failed with state %s", id, sub)
		c.FailNow()
		return false
	}

	if !until(TimeoutContainerStateChange, time.Second/20, isRunning) {
		c.Errorf("Timeout during start of %s, never got to 'active' state", id)
		c.FailNow()
	}

	// Docker does not immediately return container status - possibly due to races inside of the
	// daemon
	failed := false
	isContainerUp := func() bool {
		done, err := isContainerAvailable(s.dockerClient, id.ContainerFor())
		if err != nil {
			failed = true
			c.Error("Docker couldn't return container info", err)
			c.FailNow()
		}
		return done
	}

	if !until(TimeoutDockerWait, IntervalHttpCheck, isContainerUp) {
		if !failed {
			c.Errorf("Docker never reported the container running %s", id)
		}
		c.FailNow()
	}
}

func (s *IntegrationTestSuite) assertContainerStartsAndExits(c *chk.C, start time.Time, id containers.Identifier) {
	hasStarted := func() bool {
		_, inactiveEnd, activeStart, _ := s.unitTimes(id)
		if inactiveEnd.Before(start) || activeStart.Before(start) {
			return false
		}
		return true
	}
	if !until(TimeoutContainerStateChange, IntervalContainerCheck, hasStarted) {
		c.Errorf("The service did not start in the allotted time")
		c.FailNow()
	}

	hasCompleted := func() bool {
		switch active, _ := s.unitState(id); active {
		case "active", "activating", "deactivating":
			return false
		}
		return true
	}
	if !until(TimeoutContainerStateChange, IntervalContainerCheck, hasCompleted) {
		c.Errorf("The service did not finish in the allotted time")
		c.FailNow()
	}
}

func (s *IntegrationTestSuite) assertContainerStops(c *chk.C, id containers.Identifier, allowFail bool) {
	active, _ := s.unitState(id)
	switch active {
	case "active", "activating":
		c.Errorf("Container %s stop not properly queued, service is still active - %s", id, active)
		c.FailNow()
		return
	}

	isStopped := func() bool {
		active, sub := s.unitState(id)
		if active == "inactive" {
			return true
		}
		if allowFail && active == "failed" {
			return true
		}
		if active == "deactivating" {
			return false
		}
		c.Errorf("Unit %s stop failed (%s) with state %s", id, active, sub)
		c.FailNow()
		return false
	}

	if !until(TimeoutContainerStateChange, IntervalContainerCheck, isStopped) {
		c.Errorf("Timeout during start of %s, never got to 'inactive' state", id)
		c.FailNow()
	}

	_, err := s.dockerClient.InspectContainer(id.ContainerFor())
	if err == nil {
		c.Errorf("Container %s is still active in docker, should be stopped and removed", id.ContainerFor())
		c.FailNow()
	}
}

func (s *IntegrationTestSuite) assertContainerRestarts(c *chk.C, id containers.Identifier) {
	isStarted := func() bool {
		active, sub := s.unitState(id)
		if active == "active" {
			return true
		}
		if active == "deactivating" || active == "activating" {
			return false
		}
		c.Errorf("Unit %s restart failed (%s) in unexpected state %s", id, active, sub)
		c.FailNow()
		return false
	}

	if !until(TimeoutContainerStateChange, IntervalContainerCheck, isStarted) {
		active, sub := s.unitState(id)
		c.Errorf("Timeout during restart of %s, never got back to 'active' state (%s/%s)", id, active, sub)
		c.FailNow()
	}

	// Docker does not immediately return container status - possibly due to races inside of the
	// daemon
	failed := false
	isContainerUp := func() bool {
		done, err := isContainerAvailable(s.dockerClient, id.ContainerFor())
		if err != nil {
			failed = true
			c.Error("Docker couldn't return container info", err)
			c.FailNow()
		}
		return done
	}

	if !until(TimeoutDockerWait, IntervalHttpCheck, isContainerUp) {
		if !failed {
			c.Errorf("Docker never reported the container running %s", id)
		}
		c.FailNow()
	}
}

func (s *IntegrationTestSuite) SetUpSuite(c *chk.C) {
	var err error

	travis := os.Getenv("TRAVIS")
	if travis != "" {
		c.Skip("-skip run on Travis")
	}

	s.daemonURI = os.Getenv("GEARD_URI")
	if s.daemonURI == "" {
		s.daemonURI = "localhost:43273"
	}

	dockerURI := os.Getenv("DOCKER_URI")
	if dockerURI == "" {
		dockerURI = "unix:///var/run/docker.sock"
	}
	s.dockerClient, err = docker.GetConnection(dockerURI)
	c.Assert(err, chk.IsNil)

	containers, err := s.dockerClient.ListContainers()
	c.Assert(err, chk.IsNil)
	for _, cinfo := range containers {
		container, err := s.dockerClient.InspectContainer(cinfo.ID)
		c.Assert(err, chk.IsNil)
		if strings.HasPrefix(container.Name, "IntTest") {
			s.dockerClient.ForceCleanContainer(cinfo.ID)
		}
	}

	_, err = s.dockerClient.GetImage(TestImage)
	c.Assert(err, chk.IsNil)

	s.sdconn, err = systemd.NewConnection()
	c.Assert(err, chk.IsNil)
	err = s.sdconn.Subscribe()
	c.Assert(err, chk.IsNil)
	defer s.sdconn.Unsubscribe()
}

func (s *IntegrationTestSuite) SetupTest(c *chk.C) {
}

func (s *IntegrationTestSuite) TearDownTest(c *chk.C) {
}

func (s *IntegrationTestSuite) TestSimpleInstallAndStartImage(c *chk.C) {
	id, err := containers.NewIdentifier("IntTest000")
	c.Assert(err, chk.IsNil)
	s.containerIds = append(s.containerIds, id)

	hostContainerId := fmt.Sprintf("%v/%v", s.daemonURI, id)

	cmd := exec.Command("/usr/bin/gear", "install", TestImage, hostContainerId)
	data, err := cmd.CombinedOutput()
	c.Log(string(data))
	c.Assert(err, chk.IsNil)
	active, _ := s.unitState(id)
	c.Assert(active, chk.Equals, "inactive")

	s.assertFilePresent(c, id.UnitPathFor(), 0664, true)
	paths, err := filepath.Glob(id.VersionedUnitPathFor("*"))
	c.Assert(err, chk.IsNil)
	for _, p := range paths {
		s.assertFilePresent(c, p, 0664, true)
	}
	s.assertFileAbsent(c, filepath.Join(id.RunPathFor(), "container-init.sh"))

	ports, err := containers.GetExistingPorts(id)
	c.Assert(err, chk.IsNil)
	c.Assert(len(ports), chk.Equals, 0)

	cmd = exec.Command("/usr/bin/gear", "status", hostContainerId)
	data, err = cmd.CombinedOutput()
	c.Assert(err, chk.IsNil)
	c.Log(string(data))
	c.Assert(strings.Contains(string(data), "Loaded: loaded (/var/lib/containers/units/In/ctr-IntTest000.service; enabled)"), chk.Equals, true)
}

var hasEnvFile = flag.Bool("env-file", true, "Test env-file feature")

func (s *IntegrationTestSuite) TestSimpleInstallWithEnv(c *chk.C) {
	if !*hasEnvFile {
		c.Skip("-env-file not specified")
	}

	id, err := containers.NewIdentifier("IntTest008")
	c.Assert(err, chk.IsNil)
	s.containerIds = append(s.containerIds, id)

	hostContainerId := fmt.Sprintf("%v/%v", s.daemonURI, id)

	start := time.Now()
	cmd := exec.Command("/usr/bin/gear", "install", EnvImage, hostContainerId, "--env-file=deployment/fixtures/simple.env", "--start")
	data, err := cmd.CombinedOutput()
	c.Log(cmd.Args)
	c.Log(string(data))
	c.Assert(err, chk.IsNil)
	s.assertContainerStartsAndExits(c, start, id)

	cmd = exec.Command("/usr/bin/gear", "status", hostContainerId)
	data, err = cmd.CombinedOutput()
	c.Assert(err, chk.IsNil)
	c.Log(string(data))
	c.Assert(strings.Contains(string(data), "TEST=\"value\""), chk.Equals, true)
	c.Assert(strings.Contains(string(data), "QUOTED=\"\\\"foo\\\"\""), chk.Equals, true)
	c.Assert(strings.Contains(string(data), "IGNORED"), chk.Equals, false)
}

func (s *IntegrationTestSuite) TestIsolateInstallAndStartImage(c *chk.C) {
	id, err := containers.NewIdentifier("IntTest001")
	c.Assert(err, chk.IsNil)
	s.containerIds = append(s.containerIds, id)

	hostContainerId := fmt.Sprintf("%v/%v", s.daemonURI, id)

	cmd := exec.Command("/usr/bin/gear", "install", TestImage, hostContainerId, "--start", "--ports=8080:0", "--isolate")
	data, err := cmd.CombinedOutput()
	c.Log(string(data))
	c.Assert(err, chk.IsNil)
	s.assertContainerStarts(c, id)

	s.assertFilePresent(c, id.UnitPathFor(), 0664, true)
	paths, err := filepath.Glob(id.VersionedUnitPathFor("*"))
	c.Assert(err, chk.IsNil)
	for _, p := range paths {
		s.assertFilePresent(c, p, 0664, true)
	}
	s.assertFilePresent(c, filepath.Join(id.RunPathFor(), "container-init.sh"), 0700, false)

	ports, err := containers.GetExistingPorts(id)
	c.Assert(err, chk.IsNil)
	c.Assert(len(ports), chk.Equals, 1)

	httpAlive := func() bool {
		resp, err := http.Get(fmt.Sprintf("http://0.0.0.0:%v", ports[0].External))
		if err == nil {
			c.Assert(resp.StatusCode, chk.Equals, 200)
			return true
		}
		return false
	}
	if !until(TimeoutContainerStateChange, IntervalHttpCheck, httpAlive) {
		c.Errorf("Unable to retrieve a 200 status code from port %d", ports[0].External)
		c.FailNow()
	}
}

func (s *IntegrationTestSuite) TestIsolateInstallImage(c *chk.C) {
	id, err := containers.NewIdentifier("IntTest002")
	c.Assert(err, chk.IsNil)
	s.containerIds = append(s.containerIds, id)

	hostContainerId := fmt.Sprintf("%v/%v", s.daemonURI, id)

	cmd := exec.Command("/usr/bin/gear", "install", TestImage, hostContainerId)
	data, err := cmd.CombinedOutput()
	c.Log(string(data))
	c.Assert(err, chk.IsNil)
	active, _ := s.unitState(id)
	c.Assert(active, chk.Equals, "inactive")

	s.assertFilePresent(c, id.UnitPathFor(), 0664, true)
	paths, err := filepath.Glob(id.VersionedUnitPathFor("*"))
	c.Assert(err, chk.IsNil)
	for _, p := range paths {
		s.assertFilePresent(c, p, 0664, true)
	}
}

func (s *IntegrationTestSuite) TestStartStopContainer(c *chk.C) {
	id, err := containers.NewIdentifier("IntTest003")
	c.Assert(err, chk.IsNil)
	s.containerIds = append(s.containerIds, id)

	hostContainerId := fmt.Sprintf("%v/%v", s.daemonURI, id)

	cmd := exec.Command("/usr/bin/gear", "install", TestImage, hostContainerId, "--ports=8080:34957", "--isolate")
	data, err := cmd.CombinedOutput()
	c.Log(string(data))
	c.Assert(err, chk.IsNil)
	s.assertFilePresent(c, id.UnitPathFor(), 0664, true)

	cmd = exec.Command("/usr/bin/gear", "start", hostContainerId)
	data, err = cmd.CombinedOutput()
	c.Log(string(data))
	c.Assert(err, chk.IsNil)
	s.assertContainerStarts(c, id)
	s.assertFilePresent(c, filepath.Join(id.RunPathFor(), "container-init.sh"), 0700, false)

	ports, err := containers.GetExistingPorts(id)
	c.Assert(err, chk.IsNil)
	c.Assert(len(ports), chk.Equals, 1)

	httpAlive := func() bool {
		resp, err := http.Get(fmt.Sprintf("http://0.0.0.0:%v", ports[0].External))
		if err == nil {
			c.Assert(resp.StatusCode, chk.Equals, 200)
			return true
		}
		return false
	}
	if !until(TimeoutContainerStateChange, IntervalHttpCheck, httpAlive) {
		c.Errorf("Unable to retrieve a 200 status code from port %d", ports[0].External)
		c.FailNow()
	}

	cmd = exec.Command("/usr/bin/gear", "stop", hostContainerId)
	data, err = cmd.CombinedOutput()
	c.Log(string(data))
	c.Assert(err, chk.IsNil)
	s.assertContainerStops(c, id, true)
}

func (s *IntegrationTestSuite) TestRestartContainer(c *chk.C) {
	id, err := containers.NewIdentifier("IntTest004")
	c.Assert(err, chk.IsNil)
	s.containerIds = append(s.containerIds, id)

	hostContainerId := fmt.Sprintf("%v/%v", s.daemonURI, id)

	cmd := exec.Command("/usr/bin/gear", "install", TestImage, hostContainerId, "--ports=8080:0", "--start", "--isolate")
	data, err := cmd.CombinedOutput()
	c.Log(string(data))
	c.Assert(err, chk.IsNil)
	s.assertFilePresent(c, id.UnitPathFor(), 0664, true)
	s.assertContainerStarts(c, id)
	s.assertFilePresent(c, filepath.Join(id.RunPathFor(), "container-init.sh"), 0700, false)
	oldPid := s.getContainerPid(id)

	cmd = exec.Command("/usr/bin/gear", "restart", hostContainerId)
	data, err = cmd.CombinedOutput()
	c.Log(string(data))
	c.Assert(err, chk.IsNil)
	s.assertContainerRestarts(c, id)

	newPid := s.getContainerPid(id)
	c.Assert(oldPid, chk.Not(chk.Equals), newPid)
}

func (s *IntegrationTestSuite) TestStatus(c *chk.C) {
	id, err := containers.NewIdentifier("IntTest005")
	c.Assert(err, chk.IsNil)
	s.containerIds = append(s.containerIds, id)

	hostContainerId := fmt.Sprintf("%v/%v", s.daemonURI, id)

	cmd := exec.Command("/usr/bin/gear", "install", TestImage, hostContainerId)
	data, err := cmd.CombinedOutput()
	c.Log(string(data))
	c.Assert(err, chk.IsNil)
	s.assertFilePresent(c, id.UnitPathFor(), 0664, true)
	active, _ := s.unitState(id)
	if active == "failed" {
		c.Logf("Container %s has previous recorded 'failed' state, convert to 'inactive'", id)
		active = "inactive"
	}
	c.Assert(active, chk.Equals, "inactive")

	cmd = exec.Command("/usr/bin/gear", "status", hostContainerId)
	data, err = cmd.CombinedOutput()
	c.Assert(err, chk.IsNil)
	c.Log(string(data))
	c.Assert(strings.Contains(string(data), "Loaded: loaded (/var/lib/containers/units/In/ctr-IntTest005.service; enabled)"), chk.Equals, true)

	cmd = exec.Command("/usr/bin/gear", "start", hostContainerId)
	_, err = cmd.CombinedOutput()
	c.Assert(err, chk.IsNil)
	s.assertContainerStarts(c, id)

	cmd = exec.Command("/usr/bin/gear", "status", hostContainerId)
	data, err = cmd.CombinedOutput()
	c.Log(string(data))
	c.Assert(err, chk.IsNil)
	c.Assert(strings.Contains(string(data), "Loaded: loaded (/var/lib/containers/units/In/ctr-IntTest005.service; enabled)"), chk.Equals, true)
	c.Assert(strings.Contains(string(data), "Active: active (running)"), chk.Equals, true)

	cmd = exec.Command("/usr/bin/gear", "stop", hostContainerId)
	_, err = cmd.CombinedOutput()
	c.Assert(err, chk.IsNil)
	s.assertContainerStops(c, id, true)

	cmd = exec.Command("/usr/bin/gear", "status", hostContainerId)
	data, err = cmd.CombinedOutput()
	c.Assert(err, chk.IsNil)
	c.Log(string(data))
	c.Assert(strings.Contains(string(data), "Loaded: loaded (/var/lib/containers/units/In/ctr-IntTest005.service; enabled)"), chk.Equals, true)
}

func (s *IntegrationTestSuite) TestLongContainerName(c *chk.C) {
	id, err := containers.NewIdentifier("IntTest006xxxxxxxxxxxxxx")
	c.Assert(err, chk.IsNil)
	s.containerIds = append(s.containerIds, id)

	hostContainerId := fmt.Sprintf("%v/%v", s.daemonURI, id)

	cmd := exec.Command("/usr/bin/gear", "install", TestImage, hostContainerId, "--start", "--ports=8080:0", "--isolate")
	data, err := cmd.CombinedOutput()
	c.Log(string(data))
	c.Assert(err, chk.IsNil)
	s.assertContainerStarts(c, id)

	s.assertFilePresent(c, id.UnitPathFor(), 0664, true)
	s.assertFilePresent(c, filepath.Join(id.RunPathFor(), "container-init.sh"), 0700, false)

	ports, err := containers.GetExistingPorts(id)
	c.Assert(err, chk.IsNil)
	c.Assert(len(ports), chk.Equals, 1)

	httpAlive := func() bool {
		resp, err := http.Get(fmt.Sprintf("http://0.0.0.0:%v", ports[0].External))
		if err == nil {
			c.Assert(resp.StatusCode, chk.Equals, 200)
			return true
		}
		return false
	}
	if !until(TimeoutContainerStateChange, IntervalHttpCheck, httpAlive) {
		c.Errorf("Unable to retrieve a 200 status code from port %d", ports[0].External)
		c.FailNow()
	}
}

func (s *IntegrationTestSuite) TestContainerNetLinks(c *chk.C) {
	id, err := containers.NewIdentifier("IntTest007")
	c.Assert(err, chk.IsNil)
	s.containerIds = append(s.containerIds, id)

	hostContainerId := fmt.Sprintf("%v/%v", s.daemonURI, id)

	cmd := exec.Command("/usr/bin/gear", "install", TestImage, hostContainerId, "--ports=8080:4004", "--isolate")
	data, err := cmd.CombinedOutput()
	c.Log(string(data))
	c.Assert(err, chk.IsNil)
	s.assertFilePresent(c, id.UnitPathFor(), 0664, true)

	cmd = exec.Command("/usr/bin/gear", "link", "-n", "127.0.0.1:8081:74.125.239.114:80", hostContainerId)
	data, err = cmd.CombinedOutput()
	c.Log(string(data))
	c.Assert(err, chk.IsNil)

	cmd = exec.Command("/usr/bin/gear", "start", hostContainerId)
	data, err = cmd.CombinedOutput()
	s.assertContainerStarts(c, id)
	s.assertFilePresent(c, filepath.Join(id.RunPathFor(), "container-init.sh"), 0700, false)

	cmd = exec.Command("/usr/bin/switchns", "--container="+id.ContainerFor(), "--", "/sbin/iptables", "-t", "nat", "-L")
	data, err = cmd.CombinedOutput()
	c.Log(string(data))
	c.Assert(strings.Contains(string(data), "tcp dpt:tproxy to:74.125.239.114"), chk.Equals, true)
}

func (s *IntegrationTestSuite) TearDownSuite(c *chk.C) {
	for _, id := range s.containerIds {
		hostContainerId := fmt.Sprintf("%v/%v", s.daemonURI, id)

		cmd := exec.Command("/usr/bin/gear", "delete", hostContainerId)
		data, err := cmd.CombinedOutput()
		c.Log(string(data))
		if err != nil {
			c.Logf("Container %v did not cleanup properly", id)
		}
	}
}
