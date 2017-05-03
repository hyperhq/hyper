package integration

import (
	"io"
	"testing"

	"github.com/hyperhq/hyperd/lib/promise"
	"github.com/hyperhq/hyperd/types"
	. "gopkg.in/check.v1"
)

const (
	server = "127.0.0.1:22318"
)

// Hook up gocheck into the "go test" runner.
func Test(t *testing.T) { TestingT(t) }

type TestSuite struct {
	client *HyperClient
}

var _ = Suite(&TestSuite{})

func (s *TestSuite) SetUpSuite(c *C) {
	cl, err := NewHyperClient(server)
	c.Assert(err, IsNil)
	if err != nil {
		c.Skip("hyperd is down")
	}
	s.client = cl
}

func (s *TestSuite) TestGetPodList(c *C) {
	podList, err := s.client.GetPodList()
	c.Assert(err, IsNil)
	c.Logf("Got PodList %v", podList)
}

func (s *TestSuite) TestGetVMList(c *C) {
	vmList, err := s.client.GetVMList()
	c.Assert(err, IsNil)
	c.Logf("Got VMList %v", vmList)
}

func (s *TestSuite) TestGetContainerList(c *C) {
	containerList, err := s.client.GetContainerList()
	c.Assert(err, IsNil)
	c.Logf("Got ContainerList %v", containerList)
}

func (s *TestSuite) TestGetImageList(c *C) {
	imageList, err := s.client.GetImageList()
	c.Assert(err, IsNil)
	c.Logf("Got ImageList %v", imageList)
}

func (s *TestSuite) TestGetContainerInfo(c *C) {
	containerList, err := s.client.GetContainerList()
	c.Assert(err, IsNil)
	c.Logf("Got ContainerList %v", containerList)

	if len(containerList) == 0 {
		return
	}

	info, err := s.client.GetContainerInfo(containerList[0].ContainerID)
	c.Assert(err, IsNil)
	c.Logf("Got ContainerInfo %v", info)
}

func (s *TestSuite) TestGetContainerLogs(c *C) {
	containerList, err := s.client.GetContainerList()
	c.Assert(err, IsNil)
	c.Logf("Got ContainerList %v", containerList)

	if len(containerList) == 0 {
		return
	}

	logs, err := s.client.GetContainerLogs(containerList[0].ContainerID)
	c.Assert(err, IsNil)
	c.Logf("Got ContainerLogs %v", logs)
}

func (s *TestSuite) TestPostAttach(c *C) {
	err := s.client.PullImage("busybox", "latest", nil)
	c.Assert(err, IsNil)

	spec := types.UserPod{
		Containers: []*types.UserContainer{
			{
				Image: "busybox",
			},
		},
	}

	pod, err := s.client.CreatePod(&spec)
	c.Assert(err, IsNil)
	c.Logf("Pod created: %s", pod)
	defer s.client.RemovePod(pod)

	err = s.client.StartPod(pod)
	c.Assert(err, IsNil)

	podInfo, err := s.client.GetPodInfo(pod)
	c.Assert(err, IsNil)

	err = s.client.PostAttach(podInfo.Status.ContainerStatus[0].ContainerID, false)
	c.Assert(err, IsNil)
}

func (s *TestSuite) TestGetPodInfo(c *C) {
	podList, err := s.client.GetPodList()
	c.Assert(err, IsNil)
	c.Logf("Got PodList %v", podList)

	if len(podList) == 0 {
		return
	}

	info, err := s.client.GetPodInfo(podList[0].PodID)
	c.Assert(err, IsNil)
	c.Logf("Got PodInfo %v", info)
}

func (s *TestSuite) TestCreateAndStartPod(c *C) {
	err := s.client.PullImage("busybox", "latest", nil)
	c.Assert(err, IsNil)

	spec := types.UserPod{
		Id: "busybox",
		Containers: []*types.UserContainer{
			{
				Image: "busybox",
			},
		},
	}

	pod, err := s.client.CreatePod(&spec)
	c.Assert(err, IsNil)
	c.Logf("Pod created: %s", pod)

	podList, err := s.client.GetPodList()
	c.Assert(err, IsNil)
	c.Logf("Got PodList %v", podList)

	var found = false
	for _, p := range podList {
		if p.PodID == pod {
			found = true
			break
		}
	}
	if !found {
		c.Errorf("Can't found pod %s", pod)
	}

	err = s.client.StartPod(pod)
	c.Assert(err, IsNil)

	podInfo, err := s.client.GetPodInfo(pod)
	c.Assert(err, IsNil)
	c.Assert(podInfo.Status.Phase, Equals, "Running")

	err = s.client.RemovePod(pod)
	c.Assert(err, IsNil)
}

func (s *TestSuite) TestCreateContainer(c *C) {
	err := s.client.PullImage("busybox", "latest", nil)
	c.Assert(err, IsNil)

	spec := types.UserPod{}
	pod, err := s.client.CreatePod(&spec)
	c.Assert(err, IsNil)
	c.Logf("Pod created: %s", pod)

	container, err := s.client.CreateContainer(pod, &types.UserContainer{
		Image: "busybox",
	})
	c.Assert(err, IsNil)
	c.Logf("Container created: %s", container)

	info, err := s.client.GetContainerInfo(container)
	c.Assert(err, IsNil)
	c.Assert(info.PodID, Equals, pod)

	err = s.client.RemovePod(pod)
	c.Assert(err, IsNil)
}

func (s *TestSuite) TestRenameContainer(c *C) {
	err := s.client.PullImage("busybox", "latest", nil)
	c.Assert(err, IsNil)

	spec := types.UserPod{}
	pod, err := s.client.CreatePod(&spec)
	c.Assert(err, IsNil)
	c.Logf("Pod created: %s", pod)

	container, err := s.client.CreateContainer(pod, &types.UserContainer{
		Image: "busybox",
	})
	c.Assert(err, IsNil)
	c.Logf("Container created: %s", container)

	info, err := s.client.GetContainerInfo(container)
	c.Assert(err, IsNil)

	oldName := info.Container.Name[1:]
	newName := "busybox0123456789"
	err = s.client.RenameContainer(oldName, newName)
	c.Assert(err, IsNil)

	info, err = s.client.GetContainerInfo(container)
	c.Assert(err, IsNil)
	c.Assert(info.Container.Name[1:], Equals, newName)

	err = s.client.RemovePod(pod)
	c.Assert(err, IsNil)
}

func (s *TestSuite) TestPullImage(c *C) {
	err := s.client.PullImage("alpine", "latest", nil)
	c.Assert(err, IsNil)

	list, err := s.client.GetImageList()
	c.Assert(err, IsNil)
	found := false
	for _, img := range list {
		for _, repo := range img.RepoTags {
			if repo == "alpine:latest" {
				found = true
				break
			}
		}
	}
	c.Assert(found, Equals, true)

	err = s.client.RemoveImage("alpine")
	c.Assert(err, IsNil)
	list, err = s.client.GetImageList()
	c.Assert(err, IsNil)

	found = false
	for _, img := range list {
		for _, repo := range img.RepoTags {
			if repo == "alpine:latest" {
				found = true
				break
			}
		}
	}
	c.Assert(found, Equals, false)
}

func (s *TestSuite) TestAddListDeleteService(c *C) {
	spec := types.UserPod{
		Containers: []*types.UserContainer{
			{
				Image:   "busybox",
				Command: []string{"sleep", "10000"},
			},
			{
				Image:   "busybox",
				Command: []string{"sleep", "10000"},
			},
		},
		Services: []*types.UserService{
			{
				ServiceIP:   "10.10.0.24",
				ServicePort: 2834,
				Protocol:    "TCP",
				Hosts: []*types.UserServiceBackend{
					{
						HostIP:   "127.0.0.1",
						HostPort: 2345,
					},
				},
			},
		},
	}

	c.Log("begin ===> create pod")

	pod, err := s.client.CreatePod(&spec)
	c.Assert(err, IsNil)

	// clear the test pod
	defer func() {
		err = s.client.RemovePod(pod)
		c.Assert(err, IsNil)
	}()

	c.Log("    2 ===> create pod")

	err = s.client.StartPod(pod)
	c.Assert(err, IsNil)

	updateService := []*types.UserService{
		{
			ServiceIP:   "10.10.0.100",
			ServicePort: 80,
			Protocol:    "TCP",
			Hosts: []*types.UserServiceBackend{
				{
					HostIP:   "192.168.23.2",
					HostPort: 8080,
				},
			},
		},
	}

	c.Log("    3 ===> update service")
	err = s.client.UpdateService(pod, updateService)
	c.Assert(err, IsNil)

	c.Log("    4 ===> list service")
	svcList, err := s.client.ListService(pod)
	c.Assert(err, IsNil)
	c.Assert(len(svcList), Equals, 1)
	c.Assert(svcList[0].ServiceIP, Equals, "10.10.0.100")

	addService := []*types.UserService{
		{
			ServiceIP:   "10.10.0.22",
			ServicePort: 80,
			Protocol:    "TCP",
			Hosts: []*types.UserServiceBackend{
				{
					HostIP:   "192.168.23.2",
					HostPort: 8080,
				},
			},
		},
	}

	c.Log("    5 ===> add service")
	err = s.client.AddService(pod, addService)
	c.Assert(err, IsNil)
	c.Log("    6 ===> list service")
	svcList, err = s.client.ListService(pod)
	c.Assert(err, IsNil)
	c.Assert(len(svcList), Equals, 2)

	c.Log("    7 ===> delete service")
	err = s.client.DeleteService(pod, addService)
	c.Assert(err, IsNil)
	c.Log("    8 ===> list service")
	svcList, err = s.client.ListService(pod)
	c.Assert(len(svcList), Equals, 1)
	c.Log("last  ===> done")
}

func (s *TestSuite) TestStartAndStopPod(c *C) {
	spec := types.UserPod{
		Id: "busybox",
		Containers: []*types.UserContainer{
			{
				Image: "busybox",
			},
		},
	}

	pod, err := s.client.CreatePod(&spec)
	c.Assert(err, IsNil)
	c.Logf("Pod created: %s", pod)

	err = s.client.StartPod(pod)
	c.Assert(err, IsNil)

	podInfo, err := s.client.GetPodInfo(pod)
	c.Assert(err, IsNil)
	c.Assert(podInfo.Status.Phase, Equals, "Running")

	_, _, err = s.client.StopPod(pod)
	c.Assert(err, IsNil)

	podInfo, err = s.client.GetPodInfo(pod)
	c.Assert(err, IsNil)

	err = s.client.RemovePod(pod)
	c.Assert(err, IsNil)

	c.Assert(podInfo.Status.Phase, Equals, "Failed")
}

func (s *TestSuite) TestSetPodLabels(c *C) {
	spec := types.UserPod{
		Id: "busybox",
		Containers: []*types.UserContainer{
			{
				Image: "busybox",
			},
		},
	}

	podID, err := s.client.CreatePod(&spec)
	c.Assert(err, IsNil)
	c.Logf("Pod created: %s", podID)

	err = s.client.SetPodLabels(podID, true, map[string]string{"foo": "bar"})
	c.Assert(err, IsNil)

	info, err := s.client.GetPodInfo(podID)
	c.Assert(err, IsNil)

	if value, ok := info.Spec.Labels["foo"]; !ok || value != "bar" {
		c.Errorf("Expect labels: %v, but got: %v", map[string]string{"foo": "bar"}, info.Spec.Labels)
	}

	err = s.client.RemovePod(podID)
	c.Assert(err, IsNil)
}

func (s *TestSuite) TestPauseAndUnpausePod(c *C) {
	spec := types.UserPod{
		Id: "busybox",
		Containers: []*types.UserContainer{
			{
				Image: "busybox",
			},
		},
	}

	pod, err := s.client.CreatePod(&spec)
	c.Assert(err, IsNil)
	c.Logf("Pod created: %s", pod)

	err = s.client.StartPod(pod)
	c.Assert(err, IsNil)

	podInfo, err := s.client.GetPodInfo(pod)
	c.Assert(err, IsNil)
	c.Assert(podInfo.Status.Phase, Equals, "Running")

	err = s.client.PausePod(pod)
	c.Assert(err, IsNil)

	podInfo, err = s.client.GetPodInfo(pod)
	c.Assert(err, IsNil)
	c.Assert(podInfo.Status.Phase, Equals, "Paused")

	err = s.client.UnpausePod(pod)
	c.Assert(err, IsNil)

	podInfo, err = s.client.GetPodInfo(pod)
	c.Assert(err, IsNil)
	c.Assert(podInfo.Status.Phase, Equals, "Running")

	err = s.client.RemovePod(pod)
	c.Assert(err, IsNil)
}

func (s *TestSuite) TestGetPodStats(c *C) {
	info, err := s.client.Info()
	c.Assert(err, IsNil)

	// pod stats only working for libvirt.
	if info.ExecutionDriver != "libvirt" {
		c.Skip("Pod stats test is skipped because execdriver is not libvirt")
	}

	spec := types.UserPod{
		Id: "busybox",
		Containers: []*types.UserContainer{
			{
				Image: "busybox",
			},
		},
	}
	podID, err := s.client.CreatePod(&spec)
	c.Assert(err, IsNil)

	defer func() {
		err = s.client.RemovePod(podID)
		c.Assert(err, IsNil)
	}()

	err = s.client.StartPod(podID)
	c.Assert(err, IsNil)

	stats, err := s.client.GetPodStats(podID)
	c.Assert(err, IsNil)
	c.Logf("Got Pod Stats %+v", stats)
	c.Assert(stats.Cpu, NotNil)
	c.Assert(stats.Timestamp, NotNil)
}

func (s *TestSuite) TestPing(c *C) {
	resp, err := s.client.Ping()
	c.Assert(err, IsNil)
	c.Logf("Got HyperdStats %v", resp)
}

func (s *TestSuite) TestSendContainerSignal(c *C) {
	sigKill := int64(9)
	spec := types.UserPod{
		Id: "busybox",
	}

	pod, err := s.client.CreatePod(&spec)
	c.Assert(err, IsNil)
	c.Logf("Pod created: %s", pod)

	defer func() {
		err = s.client.RemovePod(pod)
		c.Assert(err, IsNil)
	}()

	container, err := s.client.CreateContainer(pod, &types.UserContainer{Image: "busybox"})
	c.Assert(err, IsNil)
	c.Logf("Container created: %s", container)

	err = s.client.StartPod(pod)
	c.Assert(err, IsNil)

	containerInfo, err := s.client.GetContainerInfo(container)
	c.Assert(err, IsNil)
	c.Assert(containerInfo.Status.Phase, Equals, "running")

	ec := make(chan int32, 1)
	go func() {
		exitCode, err := s.client.Wait(container, "", false)
		if err != nil {
			c.Logf("wait container failed: %v", err)
			ec <- 1024
			return
		}
		ec <- exitCode
	}()

	err = s.client.ContainerSignal(pod, container, sigKill)
	c.Assert(err, IsNil)

	exitCode := <-ec
	c.Assert(exitCode, Equals, int32(137))

	containerInfo, err = s.client.GetContainerInfo(container)
	c.Assert(err, IsNil)
	c.Assert(containerInfo.Status.Phase, Equals, "failed")
}

func (s *TestSuite) TestSendExecSignal(c *C) {
	sigKill := int64(9)
	cName := "test-exec-signal"
	spec := types.UserPod{
		Id: "busybox",
		Containers: []*types.UserContainer{
			{
				Name:  cName,
				Image: "busybox",
			},
		},
	}
	podID, err := s.client.CreatePod(&spec)
	c.Assert(err, IsNil)

	defer func() {
		err = s.client.RemovePod(podID)
		c.Assert(err, IsNil)
	}()

	err = s.client.StartPod(podID)
	c.Assert(err, IsNil)

	execId, err := s.client.ContainerExecCreate(cName, []string{"sh", "-c", "top"}, false)
	c.Assert(err, IsNil)

	outReader, outWriter := io.Pipe()
	errC := promise.Go(func() error {
		return s.client.ContainerExecStart(cName, execId, nil, outWriter, nil, false)
	})

	// make sure process has been started.
	readC := make(chan struct{})
	go func() {
		buf := make([]byte, 32)
		for {
			n, err := outReader.Read(buf)
			if err == nil && n > 0 {
				readC <- struct{}{}
				break
			} else if err != nil && err != io.EOF {
				errC <- err
				break
			}
		}
	}()

	select {
	case err = <-errC:
		c.Assert(err, IsNil)
	case <-readC:
	}

	err = s.client.ContainerExecSignal(cName, execId, sigKill)
	c.Assert(err, IsNil)

	exitCode, err := s.client.Wait(cName, execId, false)
	c.Assert(err, IsNil)
	c.Assert(exitCode, Equals, int32(0))
}

func (s *TestSuite) TestTTYResize(c *C) {
	cName := "test-tty-resize"
	spec := types.UserPod{
		Id: "busybox",
		Containers: []*types.UserContainer{
			{
				Name:  cName,
				Image: "busybox",
				Tty:   true,
			},
		},
	}
	podID, err := s.client.CreatePod(&spec)
	c.Assert(err, IsNil)

	defer func() {
		err = s.client.RemovePod(podID)
		c.Assert(err, IsNil)
	}()

	err = s.client.StartPod(podID)
	c.Assert(err, IsNil)

	err = s.client.TTYResize(cName, "", 400, 600)
	c.Assert(err, IsNil)

	//TODO: add a user process test when ListProcess is ready.
}
