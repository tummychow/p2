package pods

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/square/p2/pkg/auth"
	"github.com/square/p2/pkg/digest"
	"github.com/square/p2/pkg/hoist"
	"github.com/square/p2/pkg/launch"
	"github.com/square/p2/pkg/logging"
	"github.com/square/p2/pkg/opencontainer"
	"github.com/square/p2/pkg/runit"
	"github.com/square/p2/pkg/uri"
	"github.com/square/p2/pkg/user"
	"github.com/square/p2/pkg/util"
	"github.com/square/p2/pkg/util/param"
	"github.com/square/p2/pkg/util/size"

	"github.com/square/p2/Godeps/_workspace/src/github.com/Sirupsen/logrus"
)

var (
	Log logging.Logger

	// ExperimentalOpencontainer permits the use of the experimental "opencontainer"
	// launcahble type.
	ExperimentalOpencontainer = param.Bool("experimental_opencontainer", false)
)

const DEFAULT_PATH = "/data/pods"

var DefaultP2Exec = "/usr/local/bin/p2-exec"

func init() {
	Log = logging.NewLogger(logrus.Fields{})
}

func PodPath(root, manifestId string) string {
	return filepath.Join(root, manifestId)
}

type Pod struct {
	Id             string
	path           string
	logger         logging.Logger
	SV             runit.SV
	ServiceBuilder *runit.ServiceBuilder
	P2Exec         string
	DefaultTimeout time.Duration // this is the default timeout for stopping and restarting services in this pod
}

func NewPod(id string, path string) *Pod {
	return &Pod{
		Id:             id,
		path:           path,
		logger:         Log.SubLogger(logrus.Fields{"pod": id}),
		SV:             runit.DefaultSV,
		ServiceBuilder: runit.DefaultBuilder,
		P2Exec:         DefaultP2Exec,
		DefaultTimeout: 60 * time.Second,
	}
}

func ExistingPod(path string) (*Pod, error) {
	temp := Pod{path: path}
	manifest, err := temp.CurrentManifest()
	if err == NoCurrentManifest {
		return nil, util.Errorf("No current manifest set, this is not an extant pod directory")
	} else if err != nil {
		return nil, err
	}
	return NewPod(manifest.ID(), path), nil
}

func PodFromManifestId(manifestId string) *Pod {
	return NewPod(manifestId, PodPath(DEFAULT_PATH, manifestId))
}

var NoCurrentManifest error = fmt.Errorf("No current manifest for this pod")

func (pod *Pod) Path() string {
	return pod.path
}

func (pod *Pod) CurrentManifest() (Manifest, error) {
	currentManPath := pod.currentPodManifestPath()
	if _, err := os.Stat(currentManPath); os.IsNotExist(err) {
		return nil, NoCurrentManifest
	}
	return ManifestFromPath(currentManPath)
}

func (pod *Pod) Halt(manifest Manifest) (bool, error) {
	launchables, err := pod.Launchables(manifest)
	if err != nil {
		return false, err
	}

	success := true
	for _, launchable := range launchables {
		err = launchable.Halt(runit.DefaultBuilder, runit.DefaultSV) // TODO: make these configurable
		switch err.(type) {
		case nil:
			// noop
		case launch.DisableError:
			// do not set success to false on a disable error
			pod.logLaunchableWarning(launchable.ID(), err, "Could not disable launchable")
		default:
			// this case intentionally includes launch.StopError
			pod.logLaunchableError(launchable.ID(), err, "Could not halt launchable")
			success = false
		}
	}
	if success {
		pod.logInfo("Successfully halted")
	} else {
		pod.logInfo("Attempted halt, but one or more services did not stop successfully")
	}
	return success, nil
}

// Launch will attempt to start every launchable listed in the pod manifest. Errors encountered
// during the launch process will be logged, but will not stop attempts to launch other launchables
// in the same pod. If any services fail to start, the first return bool will be false. If an error
// occurs when writing the current manifest to the pod directory, an error will be returned.
func (pod *Pod) Launch(manifest Manifest) (bool, error) {
	launchables, err := pod.Launchables(manifest)
	if err != nil {
		return false, err
	}

	oldManifestTemp, err := pod.WriteCurrentManifest(manifest)
	defer os.RemoveAll(oldManifestTemp)

	if err != nil {
		return false, err
	}

	var successes []bool
	for _, launchable := range launchables {
		err := launchable.MakeCurrent()
		if err != nil {
			// being unable to flip a symlink is a catastrophic error
			return false, err
		}

		out, err := launchable.PostActivate()
		if err != nil {
			// if a launchable's post-activate fails, we probably can't
			// launch it, but this does not break the entire pod
			pod.logLaunchableError(launchable.ID(), err, out)
			successes = append(successes, false)
		} else {
			if out != "" {
				pod.logger.WithField("output", out).Infoln("Successfully post-activated")
			}
			successes = append(successes, true)
		}
	}

	err = pod.buildRunitServices(launchables, manifest.GetRestartPolicy())

	success := true
	for i, launchable := range launchables {
		if !successes[i] {
			continue
		}
		err = launchable.Launch(pod.ServiceBuilder, pod.SV) // TODO: make these configurable
		switch err.(type) {
		case nil:
			// noop
		case launch.EnableError:
			// do not set success to false on an enable error
			pod.logLaunchableWarning(launchable.ID(), err, "Could not enable launchable")
		default:
			// this case intentionally includes launch.StartError
			pod.logLaunchableError(launchable.ID(), err, "Could not launch launchable")
			success = false
		}
	}

	if success {
		pod.logInfo("Successfully launched")
	} else {
		pod.logInfo("Launched pod but one or more services failed to start")
	}

	return success, nil
}

func (pod *Pod) Prune(max size.ByteCount, manifest Manifest) {
	launchables, err := pod.Launchables(manifest)
	if err != nil {
		return
	}
	for _, l := range launchables {
		err := l.Prune(max)
		if err != nil {
			pod.logLaunchableError(l.ID(), err, "Could not prune directory")
			// Don't return here. We want to prune other launchables if possible.
		}
	}
}

func (pod *Pod) Services(manifest Manifest) ([]runit.Service, error) {
	allServices := []runit.Service{}
	launchables, err := pod.Launchables(manifest)
	if err != nil {
		return nil, err
	}
	for _, l := range launchables {
		es, err := l.Executables(pod.ServiceBuilder)
		if err != nil {
			return nil, err
		}
		if es != nil {
			for _, e := range es {
				allServices = append(allServices, e.Service)
			}
		}
	}
	return allServices, nil
}

// Write servicebuilder *.yaml file and run servicebuilder, which will register runit services for this
// pod.
func (pod *Pod) buildRunitServices(launchables []launch.Launchable, restartPolicy runit.RestartPolicy) error {
	// if the service is new, building the runit services also starts them
	sbTemplate := make(map[string]runit.ServiceTemplate)
	for _, launchable := range launchables {
		executables, err := launchable.Executables(pod.ServiceBuilder)
		if err != nil {
			pod.logLaunchableError(launchable.ID(), err, "Unable to list executables")
			continue
		}
		for _, executable := range executables {
			if _, ok := sbTemplate[executable.Service.Name]; ok {
				return util.Errorf("Duplicate executable %q for launchable %q", executable.Service.Name, launchable.ID())
			}
			sbTemplate[executable.Service.Name] = runit.ServiceTemplate{
				Run: executable.Exec,
			}
		}
	}
	err := pod.ServiceBuilder.Activate(pod.Id, sbTemplate, restartPolicy)
	if err != nil {
		return err
	}

	// as with the original servicebuilder, prune after creating
	// new services
	return pod.ServiceBuilder.Prune()
}

func (pod *Pod) WriteCurrentManifest(manifest Manifest) (string, error) {
	// write the old manifest to a temporary location in case a launch fails.
	tmpDir, err := ioutil.TempDir("", "manifests")
	if err != nil {
		return "", util.Errorf("could not create a tempdir to write old manifest: %s", err)
	}
	lastManifest := filepath.Join(tmpDir, "last_manifest.yaml")

	if _, err := os.Stat(pod.currentPodManifestPath()); err == nil {
		err = uri.URICopy(pod.currentPodManifestPath(), lastManifest)
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
	}

	f, err := os.OpenFile(pod.currentPodManifestPath(), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		pod.logError(err, "Unable to open current manifest file")
		err = pod.revertCurrentManifest(lastManifest)
		if err != nil {
			pod.logError(err, "Couldn't replace old manifest as current")
		}
		return "", err
	}
	defer f.Close()

	err = manifest.Write(f)
	if err != nil {
		pod.logError(err, "Unable to write current manifest file")
		err = pod.revertCurrentManifest(lastManifest)
		if err != nil {
			pod.logError(err, "Couldn't replace old manifest as current")
		}
		return "", err
	}

	uid, gid, err := user.IDs(manifest.RunAsUser())
	if err != nil {
		pod.logError(err, "Unable to find pod UID/GID")
		// the write was still successful so we are not going to revert
		return "", err
	}
	err = f.Chown(uid, gid)
	if err != nil {
		pod.logError(err, "Unable to chown current manifest")
		return "", err
	}

	return lastManifest, nil
}

func (pod *Pod) revertCurrentManifest(lastPath string) error {
	if _, err := os.Stat(lastPath); err == nil {
		return os.Rename(lastPath, pod.currentPodManifestPath())
	} else {
		return err
	}
}

func (pod *Pod) currentPodManifestPath() string {
	return filepath.Join(pod.path, "current_manifest.yaml")
}

func (pod *Pod) ConfigDir() string {
	return filepath.Join(pod.path, "config")
}

func (pod *Pod) EnvDir() string {
	return filepath.Join(pod.path, "env")
}

func (pod *Pod) Uninstall() error {
	currentManifest, err := pod.CurrentManifest()
	if err != nil {
		return err
	}
	launchables, err := pod.Launchables(currentManifest)
	if err != nil {
		return err
	}

	// halt launchables
	for _, launchable := range launchables {
		err = launchable.Halt(runit.DefaultBuilder, runit.DefaultSV) // TODO: make these configurable
		if err != nil {
			// log and continue
		}
	}

	// remove services for this pod, then prune the old
	// service dirs away
	err = os.Remove(filepath.Join(pod.ServiceBuilder.ConfigRoot, pod.Id+".yaml"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	err = pod.ServiceBuilder.Prune()
	if err != nil {
		return err
	}

	// remove pod home dir
	return os.RemoveAll(pod.path)
}

// Install will ensure that executables for all required services are present on the host
// machine and are set up to run. In the case of Hoist artifacts (which is the only format
// supported currently, this will set up runit services.).
func (pod *Pod) Install(manifest Manifest) error {
	podHome := pod.path
	uid, gid, err := user.IDs(manifest.RunAsUser())
	if err != nil {
		return util.Errorf("Could not determine pod UID/GID for %s: %s", manifest.RunAsUser(), err)
	}

	err = util.MkdirChownAll(podHome, uid, gid, 0755)
	if err != nil {
		return util.Errorf("Could not create pod home: %s", err)
	}

	launchables, err := pod.Launchables(manifest)
	if err != nil {
		return err
	}

	for _, launchable := range launchables {
		err := launchable.Install()
		if err != nil {
			pod.logLaunchableError(launchable.ID(), err, "Unable to install launchable")
			return err
		}
	}

	// we may need to write config files to a unique directory per pod version, depending on restart semantics. Need
	// to think about this more.
	err = pod.setupConfig(manifest, launchables)
	if err != nil {
		pod.logError(err, "Could not setup config")
		return util.Errorf("Could not setup config: %s", err)
	}

	pod.logInfo("Successfully installed")

	return nil
}

func (pod *Pod) Verify(manifest Manifest, authPolicy auth.Policy) error {
	for _, stanza := range manifest.GetLaunchableStanzas() {
		if stanza.DigestLocation == "" {
			continue
		}
		launchable, err := pod.getLaunchable(stanza, manifest.RunAsUser(), manifest.GetRestartPolicy())
		if err != nil {
			return err
		}

		// Retrieve the digest data
		launchableDigest, err := digest.ParseUris(
			launchable.Fetcher(),
			stanza.DigestLocation,
			stanza.DigestSignatureLocation,
		)
		if err != nil {
			return err
		}

		// Check that the digest is certified
		err = authPolicy.CheckDigest(launchableDigest)
		if err != nil {
			return err
		}

		// Check that the installed files match the digest
		err = launchableDigest.VerifyDir(launchable.InstallDir())
		if err != nil {
			return err
		}
	}
	return nil
}

// setupConfig does the following:
//
// 1) creates a directory in the pod's home directory called "config" which
// contains YAML configuration files (named with pod's ID and the SHA of its
// manifest's content) the path to which will be exported to a pods launchables
// via the CONFIG_PATH environment variable
//
// 2) writes an "env" directory in the pod's home directory called "env" which
// contains environment variables written as files that will be exported to all
// processes started by all launchables (as described in
// http://smarden.org/runit/chpst.8.html, with the -e option), including
// CONFIG_PATH
//
// 3) writes an "env" directory for each launchable. The "env" directory
// contains environment files specific to a launchable (such as
// LAUNCHABLE_ROOT)
//
// We may wish to provide a "config" directory per launchable at some point as
// well, so that launchables can have different config namespaces
func (pod *Pod) setupConfig(manifest Manifest, launchables []launch.Launchable) error {
	uid, gid, err := user.IDs(manifest.RunAsUser())
	if err != nil {
		return util.Errorf("Could not determine pod UID/GID: %s", err)
	}

	err = util.MkdirChownAll(pod.ConfigDir(), uid, gid, 0755)
	if err != nil {
		return util.Errorf("Could not create config directory for pod %s: %s", manifest.ID(), err)
	}
	configFileName, err := manifest.ConfigFileName()
	if err != nil {
		return err
	}
	configPath := filepath.Join(pod.ConfigDir(), configFileName)

	file, err := os.OpenFile(configPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	defer file.Close()
	if err != nil {
		return util.Errorf("Could not open config file for pod %s for writing: %s", manifest.ID(), err)
	}
	err = manifest.WriteConfig(file)
	if err != nil {
		return err
	}
	err = file.Chown(uid, gid)
	if err != nil {
		return err
	}

	platConfigFileName, err := manifest.PlatformConfigFileName()
	if err != nil {
		return err
	}
	platConfigPath := filepath.Join(pod.ConfigDir(), platConfigFileName)
	platFile, err := os.OpenFile(platConfigPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	defer platFile.Close()
	if err != nil {
		return util.Errorf("Could not open config file for pod %s for writing: %s", manifest.ID(), err)
	}
	err = manifest.WritePlatformConfig(platFile)
	if err != nil {
		return err
	}
	err = platFile.Chown(uid, gid)
	if err != nil {
		return err
	}

	err = util.MkdirChownAll(pod.EnvDir(), uid, gid, 0755)
	if err != nil {
		return util.Errorf("Could not create the environment dir for pod %s: %s", manifest.ID(), err)
	}
	err = writeEnvFile(pod.EnvDir(), "CONFIG_PATH", configPath, uid, gid)
	if err != nil {
		return err
	}
	err = writeEnvFile(pod.EnvDir(), "PLATFORM_CONFIG_PATH", platConfigPath, uid, gid)
	if err != nil {
		return err
	}
	err = writeEnvFile(pod.EnvDir(), "POD_HOME", pod.Path(), uid, gid)
	if err != nil {
		return err
	}

	for _, launchable := range launchables {
		err = util.MkdirChownAll(launchable.EnvDir(), uid, gid, 0755)
		if err != nil {
			return util.Errorf("Could not create the environment dir for pod %s launchable %s: %s", manifest.ID(), launchable.ID(), err)
		}
		err = writeEnvFile(launchable.EnvDir(), "LAUNCHABLE_ROOT", launchable.InstallDir(), uid, gid)
		if err != nil {
			return err
		}
	}

	return nil
}

// writeEnvFile takes an environment directory (as described in http://smarden.org/runit/chpst.8.html, with the -e option)
// and writes a new file with the given value.
func writeEnvFile(envDir, name, value string, uid, gid int) error {
	fpath := filepath.Join(envDir, name)

	buf := bytes.NewBufferString(value)

	err := ioutil.WriteFile(fpath, buf.Bytes(), 0644)
	if err != nil {
		return util.Errorf("Could not write environment config file at %s: %s", fpath, err)
	}

	err = os.Chown(fpath, uid, gid)
	if err != nil {
		return util.Errorf("Could not chown environment config file at %s: %s", fpath, err)
	}
	return nil
}

func (pod *Pod) Launchables(manifest Manifest) ([]launch.Launchable, error) {
	launchableStanzas := manifest.GetLaunchableStanzas()
	launchables := make([]launch.Launchable, 0, len(launchableStanzas))

	for _, launchableStanza := range launchableStanzas {
		launchable, err := pod.getLaunchable(launchableStanza, manifest.RunAsUser(), manifest.GetRestartPolicy())
		if err != nil {
			return nil, err
		}
		launchables = append(launchables, launchable)
	}

	return launchables, nil
}

func (pod *Pod) getLaunchable(launchableStanza LaunchableStanza, runAsUser string, restartPolicy runit.RestartPolicy) (launch.Launchable, error) {
	launchableRootDir := filepath.Join(pod.path, launchableStanza.LaunchableId)
	launchableId := strings.Join([]string{pod.Id, "__", launchableStanza.LaunchableId}, "")

	restartTimeout := pod.DefaultTimeout

	if launchableStanza.RestartTimeout != "" {
		possibleTimeout, err := time.ParseDuration(launchableStanza.RestartTimeout)
		if err != nil {
			pod.logger.WithError(err).Errorf("%v is not a valid restart timeout - must be parseable by time.ParseDuration(). Using default time %v", launchableStanza.RestartTimeout, restartTimeout)
		} else {
			restartTimeout = possibleTimeout
		}
	}

	if launchableStanza.LaunchableType == "hoist" {
		ret := &hoist.Launchable{
			Location:         launchableStanza.Location,
			Id:               launchableId,
			RunAs:            runAsUser,
			PodEnvDir:        pod.EnvDir(),
			Fetcher:          uri.DefaultFetcher,
			RootDir:          launchableRootDir,
			P2Exec:           pod.P2Exec,
			ExecNoLimit:      true,
			RestartTimeout:   restartTimeout,
			RestartPolicy:    restartPolicy,
			CgroupConfig:     launchableStanza.CgroupConfig,
			CgroupConfigName: launchableStanza.LaunchableId,
		}
		ret.CgroupConfig.Name = ret.Id
		return ret.If(), nil
	} else if *ExperimentalOpencontainer && launchableStanza.LaunchableType == "opencontainer" {
		ret := &opencontainer.Launchable{
			Location:       launchableStanza.Location,
			ID_:            launchableId,
			RunAs:          runAsUser,
			RootDir:        launchableRootDir,
			P2Exec:         pod.P2Exec,
			RestartTimeout: restartTimeout,
			RestartPolicy:  restartPolicy,
			CgroupConfig:   launchableStanza.CgroupConfig,
		}
		ret.CgroupConfig.Name = launchableId
		return ret, nil
	} else {
		err := fmt.Errorf("launchable type '%s' is not supported", launchableStanza.LaunchableType)
		pod.logLaunchableError(launchableStanza.LaunchableId, err, "Unknown launchable type")
		return nil, err
	}
}

func (p *Pod) logError(err error, message string) {
	p.logger.WithError(err).
		Error(message)
}

func (p *Pod) logLaunchableError(launchableId string, err error, message string) {
	p.logger.WithErrorAndFields(err, logrus.Fields{
		"launchable": launchableId}).Error(message)
}

func (p *Pod) logLaunchableWarning(launchableId string, err error, message string) {
	p.logger.WithErrorAndFields(err, logrus.Fields{
		"launchable": launchableId}).Warn(message)
}

func (p *Pod) logInfo(message string) {
	p.logger.WithFields(logrus.Fields{}).Info(message)
}
