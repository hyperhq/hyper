package daemon

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/daemon/logger/jsonfilelog"
	"github.com/docker/docker/pkg/version"
	dockertypes "github.com/docker/engine-api/types"
	"github.com/docker/engine-api/types/container"
	"github.com/docker/engine-api/types/strslice"

	"github.com/golang/glog"
	"github.com/hyperhq/hyper/servicediscovery"
	"github.com/hyperhq/hyper/utils"
	"github.com/hyperhq/runv/hypervisor"
	"github.com/hyperhq/runv/hypervisor/pod"
	"github.com/hyperhq/runv/hypervisor/types"
)

func (daemon *Daemon) StartPod(stdin io.ReadCloser, stdout io.WriteCloser, podId, vmId, tag string) (int, string, error) {
	// we can only support 1024 Pods
	if daemon.GetRunningPodNum() >= 1024 {
		return -1, "", fmt.Errorf("Pod full, the maximum Pod is 1024!")
	}

	var ttys []*hypervisor.TtyIO = []*hypervisor.TtyIO{}

	if tag != "" {
		glog.V(1).Info("Pod Run with client terminal tag: ", tag)
		ttys = append(ttys, &hypervisor.TtyIO{
			Stdin:     stdin,
			Stdout:    stdout,
			ClientTag: tag,
			Callback:  make(chan *types.VmResponse, 1),
		})
	}

	glog.Infof("pod:%s, vm:%s", podId, vmId)
	// Do the status check for the given pod
	daemon.PodList.Lock()
	glog.V(2).Infof("lock PodList")

	p, ok := daemon.PodList.Get(podId)
	if !ok {
		glog.V(2).Infof("unlock PodList")
		daemon.PodList.Unlock()
		return -1, "", fmt.Errorf("The pod(%s) can not be found, please create it first", podId)
	}
	var lazy bool = hypervisor.HDriver.SupportLazyMode() && vmId == ""

	code, cause, err := daemon.StartPodWithLock(p, vmId, nil, lazy, types.VM_KEEP_NONE, ttys)
	if err != nil {
		glog.Error(err.Error())
		glog.V(2).Infof("unlock PodList")
		daemon.PodList.Unlock()
		return -1, "", err
	}

	glog.V(2).Infof("unlock PodList")
	daemon.PodList.Unlock()

	if len(ttys) > 0 {
		p.RLock()
		tty, ok := p.ttyList[tag]
		p.RUnlock()

		if ok {
			tty.WaitForFinish()
		}
	}

	return code, cause, nil
}

//create pod if not exist
func (daemon *Daemon) RunPod(podId, podArgs, vmId string, config interface{}, lazy, autoremove bool, keep int, streams []*hypervisor.TtyIO) (int, string, error) {
	daemon.PodList.Lock()
	glog.V(2).Infof("lock PodList")
	defer glog.V(2).Infof("unlock PodList")
	defer daemon.PodList.Unlock()
	glog.V(1).Infof("podArgs: %s", podArgs)

	p, err := daemon.GetPod(podId, podArgs, autoremove)
	if err != nil {
		return -1, "", err
	}

	return daemon.StartPodWithLock(p, vmId, config, lazy, keep, streams)
}

func (daemon *Daemon) StartPodWithLock(p *Pod, vmId string, config interface{}, lazy bool, keep int, streams []*hypervisor.TtyIO) (int, string, error) {
	if p.vm != nil {
		return -1, "", fmt.Errorf("pod %s is already running", p.id)
	}

	vmResponse, err := p.Start(daemon, vmId, lazy, keep, streams)
	if err != nil {
		return -1, "", err
	}

	return vmResponse.Code, vmResponse.Cause, nil
}

// I'd like to move the remain part of this file to another file.
type Pod struct {
	id           string
	status       *hypervisor.PodStatus
	spec         *pod.UserPod
	vm           *hypervisor.Vm
	ctnStartInfo []*hypervisor.ContainerInfo
	volumes      []*hypervisor.VolumeInfo
	ttyList      map[string]*hypervisor.TtyIO
	sync.RWMutex
}

func NewPod(rawSpec []byte, id string, data interface{}, autoremove bool) (*Pod, error) {
	var err error

	p := &Pod{
		id:      id,
		ttyList: make(map[string]*hypervisor.TtyIO),
	}

	if p.spec, err = pod.ProcessPodBytes(rawSpec); err != nil {
		glog.V(1).Infof("Process POD file error: %s", err.Error())
		return nil, err
	}

	if err = p.init(data, autoremove); err != nil {
		return nil, err
	}

	return p, nil
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

func (p *Pod) DoCreate(daemon *Daemon) error {
	jsons, err := p.tryLoadContainers(daemon)
	if err != nil {
		return err
	}

	if err = p.createNewContainers(daemon, jsons); err != nil {
		return err
	}

	if err = p.parseContainerJsons(daemon, jsons); err != nil {
		return err
	}

	if err = p.createVolumes(daemon); err != nil {
		return err
	}

	if err = p.updateContainerStatus(jsons); err != nil {
		return err
	}

	return nil
}

func (daemon *Daemon) CreatePod(podId, podArgs string, autoremove bool) (*Pod, error) {
	// we can only support 1024 Pods
	if daemon.GetRunningPodNum() >= 1024 {
		return nil, fmt.Errorf("Pod full, the maximum Pod is 1024!")
	}

	if podId == "" {
		podId = fmt.Sprintf("pod-%s", pod.RandStr(10, "alpha"))
	}

	return daemon.createPodInternal(podId, podArgs, autoremove, false)
}

func (daemon *Daemon) createPodInternal(podId, podArgs string, autoremove, withinLock bool) (*Pod, error) {
	glog.V(2).Infof("podArgs: %s", podArgs)

	pod, err := NewPod([]byte(podArgs), podId, daemon, autoremove)
	if err != nil {
		return nil, err
	}

	// Creation
	if err = pod.DoCreate(daemon); err != nil {
		return nil, err
	}

	if !withinLock {
		daemon.PodList.Lock()
		glog.V(2).Infof("lock PodList")
		defer glog.V(2).Infof("unlock PodList")
		defer daemon.PodList.Unlock()
	}

	if err = daemon.AddPod(pod, podArgs); err != nil {
		return nil, err
	}

	return pod, nil
}

func (daemon *Daemon) SetPodLabels(podId string, override bool, labels map[string]string) error {
	daemon.PodList.RLock()
	glog.V(2).Infof("lock read of PodList")
	defer daemon.PodList.RUnlock()
	defer glog.V(2).Infof("unlock read of PodList")

	var pod *Pod
	if strings.Contains(podId, "pod-") {
		var ok bool
		pod, ok = daemon.PodList.Get(podId)
		if !ok {
			return fmt.Errorf("Can not get Pod info with pod ID(%s)", podId)
		}
	} else {
		pod = daemon.PodList.GetByName(podId)
		if pod == nil {
			return fmt.Errorf("Can not get Pod info with pod name(%s)", podId)
		}
	}

	if pod.spec.Labels == nil {
		pod.spec.Labels = make(map[string]string)
	}

	for k := range labels {
		if _, ok := pod.spec.Labels[k]; ok && !override {
			return fmt.Errorf("Can't update label %s without override", k)
		}
	}

	for k, v := range labels {
		pod.spec.Labels[k] = v
	}

	spec, err := json.Marshal(pod.spec)
	if err != nil {
		return err
	}

	if err := daemon.WritePodToDB(pod.id, spec); err != nil {
		return err
	}

	return nil
}

func (p *Pod) init(data interface{}, autoremove bool) error {
	if err := p.spec.Validate(); err != nil {
		return err
	}

	if err := p.preprocess(); err != nil {
		return err
	}

	resPath := filepath.Join(DefaultResourcePath, p.id)
	if err := os.MkdirAll(resPath, os.FileMode(0755)); err != nil {
		glog.Error("cannot create resource dir ", resPath)
		return err
	}

	status := hypervisor.NewPod(p.id, p.spec)
	status.Handler.Handle = hyperHandlePodEvent
	status.Handler.Data = data
	status.Autoremove = autoremove
	status.ResourcePath = resPath

	p.status = status

	return nil
}

func (p *Pod) preprocess() error {
	if p.spec == nil {
		return fmt.Errorf("No spec available for preprocess: %s", p.id)
	}

	if err := ParseServiceDiscovery(p.id, p.spec); err != nil {
		return err
	}

	if err := p.setupServices(); err != nil {
		return err
	}

	if err := p.setupEtcHosts(); err != nil {
		return err
	}

	if err := p.setupDNS(); err != nil {
		glog.Warning("Fail to prepare DNS for %s: %v", p.id, err)
		return err
	}

	return nil
}

func (p *Pod) tryLoadContainers(daemon *Daemon) ([]*dockertypes.ContainerJSON, error) {
	var (
		containerJsons = make([]*dockertypes.ContainerJSON, len(p.spec.Containers))
		rsp            *dockertypes.ContainerJSON
		ok             bool
	)

	if ids, _ := daemon.GetPodContainersByName(p.id); ids != nil {
		containerNames := make(map[string]int)

		for idx, c := range p.spec.Containers {
			containerNames[c.Name] = idx
		}
		for _, id := range ids {
			if r, err := daemon.ContainerInspect(id, false, version.Version("1.21")); err == nil {
				rsp, ok = r.(*dockertypes.ContainerJSON)
				if !ok {
					if glog.V(1) {
						glog.Warningf("fail to load container %s for pod %s", id, p.id)
					}
					continue
				}

				n := strings.TrimLeft(rsp.Name, "/")
				if idx, ok := containerNames[n]; ok {
					glog.V(1).Infof("Found exist container %s (%s), pod: %s", n, id, p.id)
					containerJsons[idx] = rsp
				} else if glog.V(1) {
					glog.Warningf("loaded container %s (%s) is not belongs to pod %s", n, id, p.id)
				}
			}
		}
	}

	return containerJsons, nil
}

func (p *Pod) createNewContainers(daemon *Daemon, jsons []*dockertypes.ContainerJSON) error {

	var (
		ok  bool
		err error
		ccs dockertypes.ContainerCreateResponse
		rsp *dockertypes.ContainerJSON
		r   interface{}

		cleanup = func(id string) {
			if err != nil {
				glog.V(1).Infof("rollback container %s of %s", id, p.id)
				daemon.Daemon.ContainerRm(id, &dockertypes.ContainerRmConfig{})
			}
		}
	)

	for idx, c := range p.spec.Containers {
		if jsons[idx] != nil {
			glog.V(1).Infof("do not need to create container %s of pod %s[%d]", c.Name, p.id, idx)
			continue
		}

		config := &container.Config{
			Image:           c.Image,
			Cmd:             strslice.New(c.Command...),
			NetworkDisabled: true,
		}

		if len(c.Entrypoint) != 0 {
			config.Entrypoint = strslice.New(c.Entrypoint...)
		}

		ccs, err = daemon.Daemon.ContainerCreate(dockertypes.ContainerCreateConfig{
			Name:   c.Name,
			Config: config,
		})

		if err != nil {
			glog.Error(err.Error())
			return err
		}
		defer cleanup(ccs.ID)

		glog.Infof("create container %s", ccs.ID)
		if r, err = daemon.ContainerInspect(ccs.ID, false, version.Version("1.21")); err != nil {
			return err
		}

		if rsp, ok = r.(*dockertypes.ContainerJSON); !ok {
			err = fmt.Errorf("fail to unpack container json response for %s of %s", c.Name, p.id)
			return err
		}

		jsons[idx] = rsp
	}

	return nil
}

func (p *Pod) parseContainerJsons(daemon *Daemon, jsons []*dockertypes.ContainerJSON) (err error) {
	err = nil
	p.ctnStartInfo = []*hypervisor.ContainerInfo{}

	for i, c := range p.spec.Containers {
		if jsons[i] == nil {
			estr := fmt.Sprintf("container %s of pod %s does not have inspect json", c.Name, p.id)
			glog.Error(estr)
			return errors.New(estr)
		}

		var (
			info *dockertypes.ContainerJSON = jsons[i]
			ci   *hypervisor.ContainerInfo  = &hypervisor.ContainerInfo{}
		)

		if c.Name == "" {
			c.Name = strings.TrimLeft(info.Name, "/")
		}
		if c.Image == "" {
			c.Image = info.Config.Image
		}
		glog.Infof("container name %s, image %s", c.Name, c.Image)

		mountId, err := GetMountIdByContainer(daemon.Storage.Type(), info.ID)
		if err != nil {
			estr := fmt.Sprintf("Cannot find mountID for container %s : %s", info.ID, err)
			glog.Error(estr)
			return errors.New(estr)
		}

		ci.Id = mountId
		ci.Workdir = info.Config.WorkingDir
		ci.Cmd = append([]string{info.Path}, info.Args...)

		// We should ignore these two in runv, instead of clear them, but here is a work around
		p.spec.Containers[i].Entrypoint = []string{}
		p.spec.Containers[i].Command = []string{}
		glog.Infof("container info config %v, Cmd %v, Args %v", info.Config, info.Config.Cmd.Slice(), info.Args)

		env := make(map[string]string)
		for _, v := range info.Config.Env {
			env[v[:strings.Index(v, "=")]] = v[strings.Index(v, "=")+1:]
		}
		for _, e := range p.spec.Containers[i].Envs {
			env[e.Env] = e.Value
		}
		ci.Envs = env

		processImageVolumes(info, info.ID, p.spec, &p.spec.Containers[i])

		p.ctnStartInfo = append(p.ctnStartInfo, ci)
		glog.V(1).Infof("Container Info is \n%v", ci)
	}

	return nil
}

func GetMountIdByContainer(driver, cid string) (string, error) {
	idPath := path.Join(utils.HYPER_ROOT, fmt.Sprintf("image/%s/layerdb/mounts/%s/mount-id", driver, cid))
	if _, err := os.Stat(idPath); err != nil && os.IsNotExist(err) {
		return "", err
	}

	id, err := ioutil.ReadFile(idPath)
	if err != nil {
		return "", err
	}

	return string(id), nil
}

func (p *Pod) createVolumes(daemon *Daemon) error {

	var (
		vol *hypervisor.VolumeInfo
		err error
	)

	sd := daemon.Storage
	for i := range p.spec.Volumes {
		if p.spec.Volumes[i].Source == "" {
			vol, err = sd.CreateVolume(daemon, p.id, p.spec.Volumes[i].Name)
			if err != nil {
				return err
			}

			p.spec.Volumes[i].Source = vol.Filepath
			if sd.Type() != "devicemapper" {
				p.spec.Volumes[i].Driver = "vfs"
			} else {
				// type other than doesn't need to be mounted
				p.spec.Volumes[i].Driver = "raw"
			}
		}
	}
	return nil
}

func (p *Pod) updateContainerStatus(jsons []*dockertypes.ContainerJSON) error {
	p.status.Containers = []*hypervisor.Container{}
	for idx, c := range p.spec.Containers {
		if jsons[idx] == nil {
			estr := fmt.Sprintf("container %s of pod %s does not have inspect json", c.Name, p.id)
			glog.Error(estr)
			return errors.New(estr)
		}

		cmds := append([]string{jsons[idx].Path}, jsons[idx].Args...)
		p.status.AddContainer(jsons[idx].ID, "/"+c.Name, jsons[idx].Image, cmds, types.S_POD_CREATED)
	}
	return nil
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

func processImageVolumes(config *dockertypes.ContainerJSON, id string, userPod *pod.UserPod, container *pod.UserContainer) {
	if config.Config.Volumes == nil {
		return
	}

	existed := make(map[string]bool)
	for _, v := range container.Volumes {
		existed[v.Path] = true
	}

	for tgt := range config.Config.Volumes {
		if _, ok := existed[tgt]; ok {
			continue
		}

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

func (p *Pod) setupServices() error {

	err := servicediscovery.PrepareServices(p.spec, p.id)
	if err != nil {
		glog.Errorf("PrepareServices failed %s", err.Error())
	}
	return err
}

// PrepareEtcHosts sets /etc/hosts for each container
func (p *Pod) setupEtcHosts() (err error) {
	var (
		hostsVolumeName = "etchosts-volume"
		hostVolumePath  = ""
		hostsPath       = "/etc/hosts"
	)

	if p.spec == nil {
		return
	}

	for idx, c := range p.spec.Containers {
		insert := true

		for _, v := range c.Volumes {
			if v.Path == hostsPath {
				insert = false
				break
			}
		}

		for _, f := range c.Files {
			if f.Path == hostsPath {
				insert = false
				break
			}
		}

		if !insert {
			continue
		}

		if hostVolumePath == "" {
			hostVolumePath, err = prepareHosts(p.id)
			if err != nil {
				return
			}

			p.spec.Volumes = append(p.spec.Volumes, pod.UserVolume{
				Name:   hostsVolumeName,
				Source: hostVolumePath,
				Driver: "vfs",
			})
		}

		p.spec.Containers[idx].Volumes = append(c.Volumes, pod.UserVolumeReference{
			Path:     hostsPath,
			Volume:   hostsVolumeName,
			ReadOnly: false,
		})
	}

	return
}

/***
  PrepareDNS() Set the resolv.conf of host to each container, except the following cases:

  - if the pod has a `dns` field with values, the pod will follow the dns setup, and daemon
    won't insert resolv.conf file into any containers
  - if the pod has a `file` which source is uri "file:///etc/resolv.conf", this mean the user
    will handle this file by himself/herself, daemon won't touch the dns setting even if the file
    is not referenced by any containers. This could be a method to prevent the daemon from unwanted
    setting the dns configuration
  - if a container has a file config in the pod spec with `/etc/resolv.conf` as target `path`,
    then this container won't be set as the file from hosts. Then a user can specify the content
    of the file.

*/
func (p *Pod) setupDNS() (err error) {
	err = nil
	var (
		resolvconf = "/etc/resolv.conf"
		fileId     = p.id + "-resolvconf"
	)

	if p.spec == nil {
		estr := "No Spec available for insert a DNS configuration"
		glog.V(1).Info(estr)
		err = fmt.Errorf(estr)
		return
	}

	if len(p.spec.Dns) > 0 {
		glog.V(1).Info("Already has DNS config, bypass DNS insert")
		return
	}

	if stat, e := os.Stat(resolvconf); e != nil || !stat.Mode().IsRegular() {
		glog.V(1).Info("Host resolv.conf is not exist or not a regular file, do not insert DNS conf")
		return
	}

	for _, src := range p.spec.Files {
		if src.Uri == "file:///etc/resolv.conf" {
			glog.V(1).Info("Already has resolv.conf configured, bypass DNS insert")
			return
		}
	}

	p.spec.Files = append(p.spec.Files, pod.UserFile{
		Name:     fileId,
		Encoding: "raw",
		Uri:      "file://" + resolvconf,
	})

	for idx, c := range p.spec.Containers {
		insert := true

		for _, f := range c.Files {
			if f.Path == resolvconf {
				insert = false
				break
			}
		}

		if !insert {
			continue
		}

		p.spec.Containers[idx].Files = append(c.Files, pod.UserFileReference{
			Path:     resolvconf,
			Filename: fileId,
			Perm:     "0644",
		})
	}

	return
}

func (p *Pod) setupMountsAndFiles(sd Storage) (err error) {
	if len(p.ctnStartInfo) != len(p.spec.Containers) {
		estr := fmt.Sprintf("Prepare error, pod %s does not get container infos well", p.id)
		glog.Error(estr)
		err = errors.New(estr)
		return err
	}

	var (
		sharedDir = path.Join(hypervisor.BaseDir, p.vm.Id, hypervisor.ShareDirTag)
		files     = make(map[string](pod.UserFile))
	)

	for _, f := range p.spec.Files {
		files[f.Name] = f
	}

	for i, c := range p.status.Containers {
		var (
			ci *hypervisor.ContainerInfo
		)

		mountId := p.ctnStartInfo[i].Id
		glog.Infof("container ID: %s, mountId %s\n", c.Id, mountId)
		ci, err = sd.PrepareContainer(mountId, sharedDir)
		if err != nil {
			return err
		}

		err = processInjectFiles(&p.spec.Containers[i], files, sd, mountId, sd.RootPath(), sharedDir)
		if err != nil {
			return err
		}

		ci.Id = c.Id
		ci.Cmd = p.ctnStartInfo[i].Cmd
		ci.Envs = p.ctnStartInfo[i].Envs
		ci.Entrypoint = p.ctnStartInfo[i].Entrypoint
		ci.Workdir = p.ctnStartInfo[i].Workdir

		p.ctnStartInfo[i] = ci
	}

	return nil
}

func (p *Pod) mountVolumes(daemon *Daemon, sd Storage) (err error) {
	err = nil
	p.volumes = []*hypervisor.VolumeInfo{}

	var (
		sharedDir = path.Join(hypervisor.BaseDir, p.vm.Id, hypervisor.ShareDirTag)
	)

	for _, v := range p.spec.Volumes {
		var vol *hypervisor.VolumeInfo
		if v.Source == "" {
			err = fmt.Errorf("volume %s in pod %s is not created", v.Name, p.id)
			return err
		}

		vol, err = ProbeExistingVolume(&v, sharedDir)
		if err != nil {
			return err
		}

		p.volumes = append(p.volumes, vol)
	}

	return nil
}

func (p *Pod) Prepare(daemon *Daemon) (err error) {
	if err = p.setupMountsAndFiles(daemon.Storage); err != nil {
		return
	}

	if err = p.mountVolumes(daemon, daemon.Storage); err != nil {
		return
	}

	return nil
}

func stopLogger(mypod *hypervisor.PodStatus) {
	for _, c := range mypod.Containers {
		if c.Logs.Driver == nil {
			continue
		}

		c.Logs.Driver.Close()
	}
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

		if p.ctnStartInfo != nil && len(p.ctnStartInfo) > i {
			ctx.ContainerEntrypoint = p.ctnStartInfo[i].Workdir
			ctx.ContainerArgs = p.ctnStartInfo[i].Cmd
			ctx.ContainerImageID = p.ctnStartInfo[i].Image
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

func (p *Pod) AttachTtys(daemon *Daemon, streams []*hypervisor.TtyIO) (err error) {

	ttyContainers := p.ctnStartInfo
	if p.spec.Type == "service-discovery" {
		ttyContainers = p.ctnStartInfo[1:]
	}

	for idx, str := range streams {
		if idx >= len(ttyContainers) {
			break
		}

		p.Lock()
		p.ttyList[str.ClientTag] = str
		p.Unlock()

		err = p.vm.Attach(str, ttyContainers[idx].Id, nil)
		if err != nil {
			glog.Errorf("Failed to attach client %s before start pod", str.ClientTag)
			return
		}
		glog.V(1).Infof("Attach client %s before start pod", str.ClientTag)
	}

	return nil
}

func (p *Pod) Start(daemon *Daemon, vmId string, lazy bool, keep int, streams []*hypervisor.TtyIO) (*types.VmResponse, error) {

	var (
		err       error = nil
		preparing bool  = true
	)

	if p.status.Status == types.S_POD_RUNNING ||
		(p.status.Type == "kubernetes" && p.status.Status != types.S_POD_CREATED) {
		estr := fmt.Sprintf("invalid pod status for start %v", p.status.Status)
		glog.Warning(estr)
		return nil, errors.New(estr)
	}

	if err = p.GetVM(daemon, vmId, lazy, keep); err != nil {
		return nil, err
	}

	defer func() {
		if err != nil && preparing && vmId == "" {
			p.KillVM(daemon)
		}
	}()

	if err = p.Prepare(daemon); err != nil {
		return nil, err
	}

	if err = p.startLogging(daemon); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			stopLogger(p.status)
		}
	}()

	if err = p.AttachTtys(daemon, streams); err != nil {
		return nil, err
	}

	// now start, the pod handler will deal with the vm
	preparing = false

	vmResponse := p.vm.StartPod(p.status, p.spec, p.ctnStartInfo, p.volumes)
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
	err = daemon.UpdateVmByPod(p.id, p.vm.Id)
	if err != nil {
		glog.Error(err.Error())
		return nil, err
	}

	return vmResponse, nil
}

// The caller must make sure that the restart policy and the status is right to restart
func (daemon *Daemon) RestartPod(mypod *hypervisor.PodStatus) error {
	// Remove the pod
	// The pod is stopped, the vm is gone
	daemon.CleanUpContainer(mypod)
	daemon.RemovePod(mypod.Id)
	daemon.DeleteVolumeId(mypod.Id)

	podData, err := daemon.GetPodByName(mypod.Id)
	if err != nil {
		return err
	}
	var lazy bool = hypervisor.HDriver.SupportLazyMode()

	// Start the pod
	_, _, err = daemon.RunPod(mypod.Id, string(podData), "", nil, lazy, false, types.VM_KEEP_NONE, []*hypervisor.TtyIO{})
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
	mypod *hypervisor.PodStatus, vm *hypervisor.Vm) (bool, bool) {
	daemon := data.(*Daemon)

	if vmResponse.Code == types.E_POD_FINISHED {
		if vm.Keep != types.VM_KEEP_NONE {
			vm.Status = types.S_VM_IDLE
			return false, false
		}
		stopLogger(mypod)
		mypod.SetPodContainerStatus(vmResponse.Data.([]uint32))
		vm.Status = types.S_VM_IDLE
		if mypod.Autoremove == true {
			daemon.CleanPod(mypod.Id)
			return false, false
		}
	} else if vmResponse.Code == types.E_VM_SHUTDOWN || vmResponse.Code == types.E_POD_STOPPED {
		if mypod.Status == types.S_POD_RUNNING {
			stopLogger(mypod)
			mypod.Status = types.S_POD_SUCCEEDED
			mypod.SetContainerStatus(types.S_POD_SUCCEEDED)
		}
		mypod.Vm = ""
		daemon.PodStopped(mypod.Id)
		if mypod.Type == "kubernetes" {
			cleanup := false
			switch mypod.Status {
			case types.S_POD_SUCCEEDED:
				if mypod.RestartPolicy == "always" {
					daemon.RestartPod(mypod)
					break
				}
				cleanup = true
			case types.S_POD_FAILED:
				if mypod.RestartPolicy != "never" {
					daemon.RestartPod(mypod)
					break
				}
				cleanup = true
			default:
				break
			}
			if cleanup {
				daemon.CleanUpContainer(mypod)
				daemon.DeleteVolumeId(mypod.Id)
			}
		}

		/* POD_STOPPED event is sent out by stop pod but want to keep the vm.
		exit event handler but don't close the fanout chan of vm,
		other pod will run on this vm */
		if vmResponse.Code == types.E_POD_STOPPED {
			return true, false
		}
		return true, true
	}

	return false, false
}
