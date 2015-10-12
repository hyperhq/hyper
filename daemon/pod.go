package daemon

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/daemon/logger/jsonfilelog"
	"github.com/hyperhq/hyper/engine"
	dockertypes "github.com/hyperhq/hyper/lib/docker/api/types"
	"github.com/hyperhq/hyper/servicediscovery"
	"github.com/hyperhq/hyper/storage"
	"github.com/hyperhq/hyper/utils"
	"github.com/hyperhq/runv/hypervisor"
	"github.com/hyperhq/runv/hypervisor/pod"
	"github.com/hyperhq/runv/hypervisor/types"
	"github.com/hyperhq/runv/lib/glog"
)

func (daemon *Daemon) CmdPodCreate(job *engine.Job) error {
	// we can only support 1024 Pods
	if daemon.GetRunningPodNum() >= 1024 {
		return fmt.Errorf("Pod full, the maximum Pod is 1024!")
	}
	podArgs := job.Args[0]
	autoRemove := false
	if job.Args[1] == "yes" || job.Args[1] == "true" {
		autoRemove = true
	}

	podId := fmt.Sprintf("pod-%s", pod.RandStr(10, "alpha"))
	daemon.PodList.Lock()
	glog.V(2).Infof("lock PodList")
	defer glog.V(2).Infof("unlock PodList")
	defer daemon.PodList.Unlock()
	err := daemon.CreatePod(podId, podArgs, autoRemove)
	if err != nil {
		return err
	}

	// Prepare the VM status to client
	v := &engine.Env{}
	v.Set("ID", podId)
	v.SetInt("Code", 0)
	v.Set("Cause", "")
	if _, err := v.WriteTo(job.Stdout); err != nil {
		return err
	}

	return nil
}

func (daemon *Daemon) CmdPodStart(job *engine.Job) error {
	// we can only support 1024 Pods
	if daemon.GetRunningPodNum() >= 1024 {
		return fmt.Errorf("Pod full, the maximum Pod is 1024!")
	}

	var (
		tag         string              = ""
		ttys        []*hypervisor.TtyIO = []*hypervisor.TtyIO{}
		ttyCallback chan *types.VmResponse
	)

	podId := job.Args[0]
	vmId := job.Args[1]
	if len(job.Args) > 2 {
		tag = job.Args[2]
	}
	if tag != "" {
		glog.V(1).Info("Pod Run with client terminal tag: ", tag)
		ttyCallback = make(chan *types.VmResponse, 1)
		ttys = append(ttys, &hypervisor.TtyIO{
			Stdin:     job.Stdin,
			Stdout:    job.Stdout,
			ClientTag: tag,
			Callback:  ttyCallback,
		})
	}

	glog.Infof("pod:%s, vm:%s", podId, vmId)
	// Do the status check for the given pod
	daemon.PodList.Lock()
	glog.V(2).Infof("lock PodList")
	if _, ok := daemon.PodList.Get(podId); !ok {
		glog.V(2).Infof("unlock PodList")
		daemon.PodList.Unlock()
		return fmt.Errorf("The pod(%s) can not be found, please create it first", podId)
	}
	var lazy bool = hypervisor.HDriver.SupportLazyMode() && vmId == ""

	code, cause, err := daemon.StartPod(podId, "", vmId, nil, lazy, false, types.VM_KEEP_NONE, ttys)
	if err != nil {
		glog.Error(err.Error())
		glog.V(2).Infof("unlock PodList")
		daemon.PodList.Unlock()
		return err
	}

	if len(ttys) > 0 {
		glog.V(2).Infof("unlock PodList")
		daemon.PodList.Unlock()
		<-ttyCallback
		return nil
	}
	defer glog.V(2).Infof("unlock PodList")
	defer daemon.PodList.Unlock()

	// Prepare the VM status to client
	v := &engine.Env{}
	v.Set("ID", vmId)
	v.SetInt("Code", code)
	v.Set("Cause", cause)
	if _, err := v.WriteTo(job.Stdout); err != nil {
		return err
	}

	return nil
}

func (daemon *Daemon) CmdPodRun(job *engine.Job) error {
	// we can only support 1024 Pods
	if daemon.GetRunningPodNum() >= 1024 {
		return fmt.Errorf("Pod full, the maximum Pod is 1024!")
	}
	var (
		autoremove  bool                = false
		tag         string              = ""
		ttys        []*hypervisor.TtyIO = []*hypervisor.TtyIO{}
		ttyCallback chan *types.VmResponse
	)
	podArgs := job.Args[0]
	if job.Args[1] == "yes" {
		autoremove = true
	}
	if len(job.Args) > 2 {
		tag = job.Args[2]
	}

	if tag != "" {
		glog.V(1).Info("Pod Run with client terminal tag: ", tag)
		ttyCallback = make(chan *types.VmResponse, 1)
		ttys = append(ttys, &hypervisor.TtyIO{
			Stdin:     job.Stdin,
			Stdout:    job.Stdout,
			ClientTag: tag,
			Callback:  ttyCallback,
		})
	}

	podId := fmt.Sprintf("pod-%s", pod.RandStr(10, "alpha"))

	glog.Info(podArgs)

	var lazy bool = hypervisor.HDriver.SupportLazyMode()

	daemon.PodList.Lock()
	glog.V(2).Infof("lock PodList")
	defer glog.V(2).Infof("unlock PodList")
	defer daemon.PodList.Unlock()
	code, cause, err := daemon.StartPod(podId, podArgs, "", nil, lazy, autoremove, types.VM_KEEP_NONE, ttys)
	if err != nil {
		glog.Error(err.Error())
		return err
	}

	if len(ttys) > 0 {
		<-ttyCallback
		return nil
	}

	// Prepare the VM status to client
	v := &engine.Env{}
	v.Set("ID", podId)
	v.SetInt("Code", code)
	v.Set("Cause", cause)
	if _, err := v.WriteTo(job.Stdout); err != nil {
		return err
	}

	return nil
}

// I'd like to move the remain part of this file to another file.
type Pod struct {
	id         string
	status     *hypervisor.PodStatus
	spec       *pod.UserPod
	vm         *hypervisor.Vm
	containers []*hypervisor.ContainerInfo
	volumes    []*hypervisor.VolumeInfo
}

func (p *Pod) GetVM(daemon *Daemon, id string, lazy bool, keep int) (err error) {
	if p == nil || p.spec == nil {
		return errors.New("Pod: unable to create VM without resource info.")
	}
	p.vm, err = daemon.GetVM(id, &p.spec.Resource, lazy, keep)
	return
}

func (p *Pod) SetVM(id string, vm *hypervisor.Vm) {
	p.status.Vm = id
	p.vm = vm
}

func (p *Pod) KillVM(daemon *Daemon) {
	if p.vm != nil {
		daemon.KillVm(p.vm.Id)
		p.vm = nil
	}
}

func (p *Pod) Status() *hypervisor.PodStatus {
	return p.status
}

func CreatePod(daemon *Daemon, dclient DockerInterface, podId, podArgs string, autoremove bool) (*Pod, error) {
	glog.V(1).Infof("podArgs: %s", podArgs)

	resPath := filepath.Join(DefaultResourcePath, podId)
	if err := os.MkdirAll(resPath, os.FileMode(0755)); err != nil {
		glog.Error("cannot create resource dir ", resPath)
		return nil, err
	}

	spec, err := ProcessPodBytes([]byte(podArgs), podId)
	if err != nil {
		glog.V(1).Infof("Process POD file error: %s", err.Error())
		return nil, err
	}

	if err = spec.Validate(); err != nil {
		return nil, err
	}

	status := hypervisor.NewPod(podId, spec)
	status.Handler.Handle = hyperHandlePodEvent
	status.Autoremove = autoremove
	status.ResourcePath = resPath

	pod := &Pod{
		id:     podId,
		status: status,
		spec:   spec,
	}

	if err = pod.InitContainers(daemon, dclient); err != nil {
		return nil, err
	}

	return pod, nil
}

func (p *Pod) InitContainers(daemon *Daemon, dclient DockerInterface) (err error) {

	type cinfo struct {
		id    string
		name  string
		image string
	}

	var (
		containers map[string]*cinfo = make(map[string]*cinfo)
		created    []string          = []string{}
	)

	// trying load existing containers from db
	if ids, _ := daemon.GetPodContainersByName(p.id); ids != nil {
		for _, id := range ids {
			if jsonResponse, err := daemon.DockerCli.GetContainerInfo(id); err == nil {
				n := strings.TrimLeft(jsonResponse.Name, "/")
				containers[n] = &cinfo{
					id:    id,
					name:  jsonResponse.Name,
					image: jsonResponse.Config.Image,
				}
				glog.V(1).Infof("Found exist container %s (%s), image: %s", n, id, jsonResponse.Config.Image)
			}
		}
	}

	defer func() {
		if err != nil {
			for _, cid := range created {
				dclient.SendCmdDelete(cid)
			}
		}
	}()

	glog.V(1).Info("Process the Containers section in POD SPEC")
	for _, c := range p.spec.Containers {

		glog.V(1).Info("trying to init container ", c.Name)

		if info, ok := containers[c.Name]; ok {
			p.status.AddContainer(info.id, info.name, info.image, []string{}, types.S_POD_CREATED)
			continue
		}

		var (
			cId []byte
			rsp *dockertypes.ContainerJSONRaw
		)

		cId, _, err = dclient.SendCmdCreate(c.Name, c.Image, []string{}, nil)
		if err != nil {
			glog.Error(err.Error())
			return
		}

		created = append(created, string(cId))

		if rsp, err = dclient.GetContainerInfo(string(cId)); err != nil {
			return
		}

		p.status.AddContainer(string(cId), rsp.Name, rsp.Config.Image, []string{}, types.S_POD_CREATED)
	}

	return nil
}

//FIXME: there was a `config` argument passed by docker/builder, but we never processed it.
func (daemon *Daemon) CreatePod(podId, podArgs string, autoremove bool) error {

	pod, err := CreatePod(daemon, daemon.DockerCli, podId, podArgs, autoremove)
	if err != nil {
		return err
	}

	return daemon.AddPod(pod, podArgs)
}

func (p *Pod) PrepareContainers(sd Storage, dclient DockerInterface) (err error) {
	err = nil
	p.containers = []*hypervisor.ContainerInfo{}

	var (
		sharedDir = path.Join(hypervisor.BaseDir, p.vm.Id, hypervisor.ShareDirTag)
	)

	files := make(map[string](pod.UserFile))
	for _, f := range p.spec.Files {
		files[f.Name] = f
	}

	for i, c := range p.status.Containers {
		var (
			info *dockertypes.ContainerJSONRaw
			ci   *hypervisor.ContainerInfo
		)
		info, err = getContinerInfo(dclient, c)

		ci, err = sd.PrepareContainer(c.Id, sharedDir)
		if err != nil {
			return err
		}
		ci.Workdir = info.Config.WorkingDir
		ci.Entrypoint = info.Config.Entrypoint.Slice()
		ci.Cmd = info.Config.Cmd.Slice()

		env := make(map[string]string)
		for _, v := range info.Config.Env {
			env[v[:strings.Index(v, "=")]] = v[strings.Index(v, "=")+1:]
		}
		for _, e := range p.spec.Containers[i].Envs {
			env[e.Env] = e.Value
		}
		ci.Envs = env

		processImageVolumes(info, c.Id, p.spec, &p.spec.Containers[i])

		err = processInjectFiles(&p.spec.Containers[i], files, sd, c.Id, sd.RootPath(), sharedDir)
		if err != nil {
			return err
		}

		p.containers = append(p.containers, ci)
		glog.V(1).Infof("Container Info is \n%v", ci)
	}

	return nil
}

func getContinerInfo(dclient DockerInterface, container *hypervisor.Container) (info *dockertypes.ContainerJSONRaw, err error) {
	info, err = dclient.GetContainerInfo(container.Id)
	if err != nil {
		glog.Error("got error when get container Info ", err.Error())
		return nil, err
	}
	if container.Name == "" {
		container.Name = info.Name
	}
	if container.Image == "" {
		container.Image = info.Config.Image
	}
	return
}

func processInjectFiles(container *pod.UserContainer, files map[string]pod.UserFile, sd Storage,
	id, rootPath, sharedDir string) error {
	for _, f := range container.Files {
		targetPath := f.Path
		if strings.HasSuffix(targetPath, "/") {
			targetPath = targetPath + f.Filename
		}
		file, ok := files[f.Filename]
		if !ok {
			continue
		}

		var src io.Reader

		if file.Uri != "" {
			urisrc, err := utils.UriReader(file.Uri)
			if err != nil {
				return err
			}
			defer urisrc.Close()
			src = urisrc
		} else {
			src = strings.NewReader(file.Contents)
		}

		switch file.Encoding {
		case "base64":
			src = base64.NewDecoder(base64.StdEncoding, src)
		default:
		}

		err := sd.InjectFile(src, id, targetPath, sharedDir,
			utils.PermInt(f.Perm), utils.UidInt(f.User), utils.UidInt(f.Group))
		if err != nil {
			glog.Error("got error when inject files ", err.Error())
			return err
		}
	}

	return nil
}

func processImageVolumes(config *dockertypes.ContainerJSONRaw, id string, userPod *pod.UserPod, container *pod.UserContainer) {
	if config.Config.Volumes == nil {
		return
	}

	for tgt := range config.Config.Volumes {
		n := id + strings.Replace(tgt, "/", "_", -1)
		v := pod.UserVolume{
			Name:   n,
			Source: "",
		}
		r := pod.UserVolumeReference{
			Volume:   n,
			Path:     tgt,
			ReadOnly: false,
		}
		userPod.Volumes = append(userPod.Volumes, v)
		container.Volumes = append(container.Volumes, r)
	}
}

func (p *Pod) PrepareServices() error {
	err := servicediscovery.PrepareServices(p.spec, p.id)
	if err != nil {
		glog.Errorf("PrepareServices failed %s", err.Error())
	}
	return err
}

func (p *Pod) PrepareVolume(daemon *Daemon, sd Storage) (err error) {
	err = nil
	p.volumes = []*hypervisor.VolumeInfo{}

	var (
		sharedDir = path.Join(hypervisor.BaseDir, p.vm.Id, hypervisor.ShareDirTag)
	)

	for _, v := range p.spec.Volumes {
		var vol *hypervisor.VolumeInfo
		if v.Source == "" {
			vol, err = sd.CreateVolume(daemon, p.id, v.Name)
			if err != nil {
				return
			}

			v.Source = vol.Filepath
			if sd.Type() != "devicemapper" {
				v.Driver = "vfs"
			} else {
				v.Driver = "raw"
			}
		}

		if v.Driver == "vfs" {
			vol.Filepath, err = storage.MountVFSVolume(v.Source, sharedDir)
			if err != nil {
				return
			}
			glog.V(1).Infof("dir %s is bound to %s", v.Source, vol.Filepath)
		}

		p.volumes = append(p.volumes, vol)
	}

	return nil
}

func (p *Pod) Prepare(daemon *Daemon) (err error) {
	if err = p.PrepareServices(); err != nil {
		return
	}

	if err = p.PrepareContainers(daemon.Storage, daemon.DockerCli); err != nil {
		return
	}

	if err = p.PrepareVolume(daemon, daemon.Storage); err != nil {
		return
	}

	return nil
}

func (p *Pod) getLogger(daemon *Daemon) (err error) {
	if p.spec.LogConfig.Type == "" {
		p.spec.LogConfig.Type = daemon.DefaultLog.Type
		p.spec.LogConfig.Config = daemon.DefaultLog.Config
	}

	if p.spec.LogConfig.Type == "none" {
		return nil
	}

	var (
		needLogger []int = []int{}
		creator    logger.Creator
	)

	for i, c := range p.status.Containers {
		if c.Logs.Driver == nil {
			needLogger = append(needLogger, i)
		}
	}

	if len(needLogger) == 0 && p.status.Status == types.S_POD_RUNNING {
		return nil
	}

	if err = logger.ValidateLogOpts(p.spec.LogConfig.Type, p.spec.LogConfig.Config); err != nil {
		return
	}
	creator, err = logger.GetLogDriver(p.spec.LogConfig.Type)
	if err != nil {
		return
	}
	glog.V(1).Infof("configuring log driver [%s] for %s", p.spec.LogConfig.Type, p.id)

	for i, c := range p.status.Containers {
		ctx := logger.Context{
			Config:             p.spec.LogConfig.Config,
			ContainerID:        c.Id,
			ContainerName:      c.Name,
			ContainerImageName: p.spec.Containers[i].Image,
			ContainerCreated:   time.Now(), //FIXME: should record creation time in PodStatus
		}

		if p.containers != nil && len(p.containers) > i {
			ctx.ContainerEntrypoint = p.containers[i].Workdir
			ctx.ContainerArgs = p.containers[i].Cmd
			ctx.ContainerImageID = p.containers[i].Image
		}

		if p.spec.LogConfig.Type == jsonfilelog.Name {
			ctx.LogPath = filepath.Join(p.status.ResourcePath, fmt.Sprintf("%s-json.log", c.Id))
			glog.V(1).Info("configure container log to ", ctx.LogPath)
		}

		if c.Logs.Driver, err = creator(ctx); err != nil {
			return
		}
		glog.V(1).Infof("configured logger for %s/%s (%s)", p.id, c.Id, c.Name)
	}

	return nil
}

func (p *Pod) startLogging(daemon *Daemon) (err error) {
	err = nil

	if err = p.getLogger(daemon); err != nil {
		return
	}

	if p.spec.LogConfig.Type == "none" {
		return nil
	}

	for _, c := range p.status.Containers {
		var stdout, stderr io.Reader

		tag := "log-" + utils.RandStr(8, "alphanum")
		if stdout, stderr, err = p.vm.GetLogOutput(c.Id, tag, nil); err != nil {
			return
		}
		c.Logs.Copier = logger.NewCopier(c.Id, map[string]io.Reader{"stdout": stdout, "stderr": stderr}, c.Logs.Driver)
		c.Logs.Copier.Run()

		if jl, ok := c.Logs.Driver.(*jsonfilelog.JSONFileLogger); ok {
			c.Logs.LogPath = jl.LogPath()
		}
	}

	return nil
}

func (p *Pod) AttachTtys(streams []*hypervisor.TtyIO) (err error) {

	ttyContainers := p.containers
	if p.spec.Type == "service-discovery" {
		ttyContainers = p.containers[1:]
	}

	for idx, str := range streams {
		if idx >= len(ttyContainers) {
			break
		}

		err = p.vm.Attach(str.Stdin, str.Stdout, str.ClientTag, ttyContainers[idx].Id, str.Callback, nil)
		if err != nil {
			glog.Errorf("Failed to attach client %s before start pod", str.ClientTag)
			return
		}
		glog.V(1).Infof("Attach client %s before start pod", str.ClientTag)
	}

	return nil
}

func (p *Pod) Start(daemon *Daemon, vmId string, lazy, autoremove bool, keep int, streams []*hypervisor.TtyIO) (*types.VmResponse, error) {

	var err error = nil

	if err = p.GetVM(daemon, vmId, lazy, keep); err != nil {
		return nil, err
	}

	defer func() {
		if err != nil && vmId == "" {
			p.KillVM(daemon)
		}
	}()

	if err = p.Prepare(daemon); err != nil {
		return nil, err
	}

	if err = p.startLogging(daemon); err != nil {
		return nil, err
	}

	if err = p.AttachTtys(streams); err != nil {
		return nil, err
	}

	vmResponse := p.vm.StartPod(p.status, p.spec, p.containers, p.volumes)
	if vmResponse.Data == nil {
		err = fmt.Errorf("VM response data is nil")
		return vmResponse, err
	}

	err = daemon.UpdateVmData(p.vm.Id, vmResponse.Data.([]byte))
	if err != nil {
		glog.Error(err.Error())
		return nil, err
	}
	// add or update the Vm info for POD
	if err := daemon.UpdateVmByPod(p.id, p.vm.Id); err != nil {
		glog.Error(err.Error())
		return nil, err
	}

	return vmResponse, nil
}

func (daemon *Daemon) StartPod(podId, podArgs, vmId string, config interface{}, lazy, autoremove bool, keep int, streams []*hypervisor.TtyIO) (int, string, error) {
	glog.V(1).Infof("podArgs: %s", podArgs)
	var (
		err error
		p   *Pod
	)

	p, err = daemon.GetPod(podId, podArgs, autoremove)
	if err != nil {
		return -1, "", err
	}

	vmResponse, err := p.Start(daemon, vmId, lazy, autoremove, keep, streams)
	if err != nil {
		return -1, "", err
	}

	return vmResponse.Code, vmResponse.Cause, nil
}

// The caller must make sure that the restart policy and the status is right to restart
func (daemon *Daemon) RestartPod(mypod *hypervisor.PodStatus) error {
	// Remove the pod
	// The pod is stopped, the vm is gone
	for _, c := range mypod.Containers {
		glog.V(1).Infof("Ready to rm container: %s", c.Id)
		if _, _, err := daemon.DockerCli.SendCmdDelete(c.Id); err != nil {
			glog.V(1).Infof("Error to rm container: %s", err.Error())
		}
	}
	daemon.RemovePod(mypod.Id)
	daemon.DeletePodContainerFromDB(mypod.Id)
	daemon.DeleteVolumeId(mypod.Id)

	podData, err := daemon.GetPodByName(mypod.Id)
	if err != nil {
		return err
	}
	var lazy bool = hypervisor.HDriver.SupportLazyMode()

	// Start the pod
	_, _, err = daemon.StartPod(mypod.Id, string(podData), "", nil, lazy, false, types.VM_KEEP_NONE, []*hypervisor.TtyIO{})
	if err != nil {
		glog.Error(err.Error())
		return err
	}

	if err := daemon.WritePodAndContainers(mypod.Id); err != nil {
		glog.Error("Found an error while saving the Containers info")
		return err
	}

	return nil
}

func hyperHandlePodEvent(vmResponse *types.VmResponse, data interface{},
	mypod *hypervisor.PodStatus, vm *hypervisor.Vm) bool {
	daemon := data.(*Daemon)

	if vmResponse.Code == types.E_POD_FINISHED {
		if vm.Keep != types.VM_KEEP_NONE {
			mypod.Vm = ""
			vm.Status = types.S_VM_IDLE
			return false
		}
		mypod.SetPodContainerStatus(vmResponse.Data.([]uint32))
		mypod.Vm = ""
		vm.Status = types.S_VM_IDLE
		if mypod.Autoremove == true {
			daemon.CleanPod(mypod.Id)
			return false
		}
	} else if vmResponse.Code == types.E_VM_SHUTDOWN {
		if mypod.Status == types.S_POD_RUNNING {
			mypod.Status = types.S_POD_SUCCEEDED
			mypod.SetContainerStatus(types.S_POD_SUCCEEDED)
		}
		mypod.Vm = ""
		daemon.RemoveVm(vm.Id)
		if mypod.Type == "kubernetes" {
			switch mypod.Status {
			case types.S_POD_SUCCEEDED:
				if mypod.RestartPolicy == "always" {
					daemon.RestartPod(mypod)
					break
				}
				daemon.DeletePodFromDB(mypod.Id)
				for _, c := range mypod.Containers {
					glog.V(1).Infof("Ready to rm container: %s", c.Id)
					if _, _, err := daemon.DockerCli.SendCmdDelete(c.Id); err != nil {
						glog.V(1).Infof("Error to rm container: %s", err.Error())
					}
				}
				daemon.DeletePodContainerFromDB(mypod.Id)
				daemon.DeleteVolumeId(mypod.Id)
				break
			case types.S_POD_FAILED:
				if mypod.RestartPolicy != "never" {
					daemon.RestartPod(mypod)
					break
				}
				daemon.DeletePodFromDB(mypod.Id)
				for _, c := range mypod.Containers {
					glog.V(1).Infof("Ready to rm container: %s", c.Id)
					if _, _, err := daemon.DockerCli.SendCmdDelete(c.Id); err != nil {
						glog.V(1).Infof("Error to rm container: %s", err.Error())
					}
				}
				daemon.DeletePodContainerFromDB(mypod.Id)
				daemon.DeleteVolumeId(mypod.Id)
				break
			default:
				break
			}
		}
		return true
	}

	return false
}
