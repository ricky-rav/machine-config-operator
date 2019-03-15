package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	imgref "github.com/containers/image/docker/reference"
	ignv2 "github.com/coreos/ignition/config/v2_2"
	ignv2_2types "github.com/coreos/ignition/config/v2_2/types"
	"github.com/golang/glog"
	drain "github.com/openshift/kubernetes-drain"
	"github.com/openshift/machine-config-operator/lib/resourceread"
	mcfgv1 "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"
	"github.com/openshift/machine-config-operator/pkg/daemon/constants"
	mcfginformersv1 "github.com/openshift/machine-config-operator/pkg/generated/informers/externalversions/machineconfiguration.openshift.io/v1"
	mcfglistersv1 "github.com/openshift/machine-config-operator/pkg/generated/listers/machineconfiguration.openshift.io/v1"
	"github.com/pkg/errors"
	"github.com/vincent-petithory/dataurl"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformersv1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	clientsetcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisterv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
)

// Daemon is the dispatch point for the functions of the agent on the
// machine. it keeps track of connections and the current state of the update
// process.
type Daemon struct {
	// name is the node name.
	name string
	// OperatingSystem the operating system the MCD is running on
	OperatingSystem string

	// NodeUpdaterClient an instance of the client which interfaces with host content deployments
	NodeUpdaterClient NodeUpdaterClient

	// bootID is a unique value per boot (generated by the kernel)
	bootID string

	// bootedOSImageURL is the currently booted URL of the operating system
	bootedOSImageURL string

	// kubeClient allows interaction with Kubernetes, including the node we are running on.
	kubeClient kubernetes.Interface
	// recorder sends events to the apiserver
	recorder record.EventRecorder

	// rootMount is the location for the MCD to chroot in
	rootMount string

	// nodeLister is used to watch for updates via the informer
	nodeLister       corelisterv1.NodeLister
	nodeListerSynced cache.InformerSynced

	mcLister       mcfglistersv1.MachineConfigLister
	mcListerSynced cache.InformerSynced

	// onceFrom defines where the source config is to run the daemon once and exit
	onceFrom string

	kubeletHealthzEnabled  bool
	kubeletHealthzEndpoint string

	installedSigterm bool

	nodeWriter *NodeWriter

	// channel used by callbacks to signal Run() of an error
	exitCh chan<- error

	// channel used to ensure all spawned goroutines exit when we exit.
	stopCh <-chan struct{}

	// node is the current instance of the node being processed through handleNodeUpdate
	// or the very first instance grabbed when the daemon starts
	node *corev1.Node

	// remove the funcs below once proper e2e testing is done for updating ssh keys
	atomicSSHKeysWriter func(ignv2_2types.PasswdUser, string) error

	queue       workqueue.RateLimitingInterface
	enqueueNode func(*corev1.Node)
	syncHandler func(node string) error

	booting bool
}

// pendingConfigState is stored as JSON at pathStateJSON; it is only
// written after an update is complete, and across the reboot to
// denote success.
type pendingConfigState struct {
	PendingConfig string `json:"pendingConfig,omitempty"`
	BootID        string `json:"bootID,omitempty"`
}

const (
	// pathSystemd is the path systemd modifiable units, services, etc.. reside
	pathSystemd = "/etc/systemd/system"
	// wantsPathSystemd is the path where enabled units should be linked
	wantsPathSystemd = "/etc/systemd/system/multi-user.target.wants/"
	// pathDevNull is the systems path to and endless blackhole
	pathDevNull = "/dev/null"
	// pathStateJSON is where we store temporary state across config changes
	pathStateJSON = "/etc/machine-config-daemon/state.json"

	// machineConfigMCFileType denotes when an MC config has been provided
	machineConfigMCFileType = "MACHINECONFIG"
	// machineConfigIgnitionFileType denotes when an Ignition config has provided
	machineConfigIgnitionFileType = "IGNITION"

	// machineConfigOnceFromRemoteConfig denotes that the config was pulled from a remote source
	machineConfigOnceFromRemoteConfig = "REMOTE"
	// machineConfigOnceFromLocalConfig denotes that the config was found locally
	machineConfigOnceFromLocalConfig = "LOCAL"

	kubeletHealthzEndpoint         = "http://localhost:10248/healthz"
	kubeletHealthzPollingInterval  = time.Duration(30 * time.Second)
	kubeletHealthzTimeout          = time.Duration(30 * time.Second)
	kubeletHealthzFailureThreshold = 3

	// TODO(runcom): increase retry and backoff?
	//
	// maxRetries is the number of times a node will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the times
	// a node is going to be requeued:
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	maxRetries = 15

	// updateDelay is a pause to deal with churn in Node
	updateDelay = 5 * time.Second
)

// getBootID loads the unique "boot id" which is generated by the Linux kernel.
func getBootID() (string, error) {
	currentBootIDBytes, err := ioutil.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(currentBootIDBytes)), nil
}

// New sets up the systemd and kubernetes connections needed to update the
// machine.
func New(
	rootMount string,
	nodeName string,
	operatingSystem string,
	nodeUpdaterClient NodeUpdaterClient,
	onceFrom string,
	kubeletHealthzEnabled bool,
	kubeletHealthzEndpoint string,
	nodeWriter *NodeWriter,
	exitCh chan<- error,
	stopCh <-chan struct{},
) (*Daemon, error) {

	osImageURL := ""
	osVersion := ""
	// Only pull the osImageURL from OSTree when we are on RHCOS
	if operatingSystem == machineConfigDaemonOSRHCOS {
		var err error
		osImageURL, osVersion, err = nodeUpdaterClient.GetBootedOSImageURL(rootMount)
		if err != nil {
			return nil, fmt.Errorf("error reading osImageURL from rpm-ostree: %v", err)
		}
		glog.Infof("Booted osImageURL: %s (%s)", osImageURL, osVersion)
	}

	bootID, err := getBootID()
	if err != nil {
		return nil, err
	}

	dn := &Daemon{
		name:                   nodeName,
		OperatingSystem:        operatingSystem,
		NodeUpdaterClient:      nodeUpdaterClient,
		rootMount:              rootMount,
		bootID:                 bootID,
		bootedOSImageURL:       osImageURL,
		onceFrom:               onceFrom,
		kubeletHealthzEnabled:  kubeletHealthzEnabled,
		kubeletHealthzEndpoint: kubeletHealthzEndpoint,
		nodeWriter:             nodeWriter,
		exitCh:                 exitCh,
		stopCh:                 stopCh,
	}
	dn.atomicSSHKeysWriter = dn.atomicallyWriteSSHKey

	return dn, nil
}

// NewClusterDrivenDaemon sets up the systemd and kubernetes connections needed to update the
// machine.
func NewClusterDrivenDaemon(
	rootMount string,
	nodeName string,
	operatingSystem string,
	nodeUpdaterClient NodeUpdaterClient,
	mcInformer mcfginformersv1.MachineConfigInformer,
	kubeClient kubernetes.Interface,
	onceFrom string,
	nodeInformer coreinformersv1.NodeInformer,
	kubeletHealthzEnabled bool,
	kubeletHealthzEndpoint string,
	nodeWriter *NodeWriter,
	exitCh chan<- error,
	stopCh <-chan struct{},
) (*Daemon, error) {
	dn, err := New(
		rootMount,
		nodeName,
		operatingSystem,
		nodeUpdaterClient,
		onceFrom,
		kubeletHealthzEnabled,
		kubeletHealthzEndpoint,
		nodeWriter,
		exitCh,
		stopCh,
	)

	if err != nil {
		return nil, err
	}

	dn.kubeClient = kubeClient
	dn.queue = workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "machineconfigdaemon")

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.V(2).Infof)
	eventBroadcaster.StartRecordingToSink(&clientsetcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	dn.recorder = eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "machineconfigdaemon", Host: nodeName})

	glog.Infof("Managing node: %s", nodeName)

	go dn.runLoginMonitor(dn.stopCh, dn.exitCh)

	nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: dn.handleNodeUpdate,
	})
	dn.nodeLister = nodeInformer.Lister()
	dn.nodeListerSynced = nodeInformer.Informer().HasSynced
	dn.mcLister = mcInformer.Lister()
	dn.mcListerSynced = mcInformer.Informer().HasSynced

	dn.enqueueNode = dn.enqueueDefault
	dn.syncHandler = dn.syncNode
	dn.booting = true

	return dn, nil
}

// worker runs a worker thread that just dequeues items, processes them, and marks them done.
// It enforces that the syncHandler is never invoked concurrently with the same key.
func (dn *Daemon) worker() {
	for dn.processNextWorkItem() {
	}
}

func (dn *Daemon) processNextWorkItem() bool {
	if dn.booting {
		// any error here in bootstrap will cause a retry
		if err := dn.bootstrapNode(); err != nil {
			glog.Warningf("Booting the MCD errored with %v", err)
		}
		return true
	}
	key, quit := dn.queue.Get()
	if quit {
		return false
	}
	defer dn.queue.Done(key)

	err := dn.syncHandler(key.(string))
	dn.handleErr(err, key)

	return true
}

// bootstrapNode takes care of the very first sync of the MCD on a node.
// It loads the node annotation from the bootstrap (if we're really bootstrapping)
// and then proceed to checking the state of the node, which includes
// finalizing an update and/or reconciling the current and desired machine configs.
func (dn *Daemon) bootstrapNode() error {
	node, err := dn.nodeLister.Get(dn.name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			glog.V(2).Infof("can't find node %s: %v", dn.name, err)
			return nil
		}
		return err
	}
	node, err = dn.loadNodeAnnotations(node)
	if err != nil {
		return err
	}
	dn.node = node
	if err := dn.CheckStateOnBoot(); err != nil {
		return err
	}
	// finished syncing node for the first time
	dn.booting = false
	return nil
}

func (dn *Daemon) handleErr(err error, key interface{}) {
	if err == nil {
		dn.queue.Forget(key)
		return
	}

	if dn.queue.NumRequeues(key) < maxRetries {
		glog.V(2).Infof("Error syncing node %v: %v", key, err)
		dn.queue.AddRateLimited(key)
		return
	}

	dn.nodeWriter.SetDegraded(err, dn.kubeClient.CoreV1().Nodes(), dn.nodeLister, dn.name)

	utilruntime.HandleError(err)
	glog.V(2).Infof("Dropping node %q out of the queue: %v", key, err)
	dn.queue.Forget(key)
	dn.queue.AddAfter(key, 1*time.Minute)
}

func (dn *Daemon) syncNode(key string) error {
	startTime := time.Now()
	glog.V(4).Infof("Started syncing node %q (%v)", key, startTime)
	defer func() {
		glog.V(4).Infof("Finished syncing node %q (%v)", key, time.Since(startTime))
	}()

	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	node, err := dn.nodeLister.Get(name)
	if apierrors.IsNotFound(err) {
		glog.V(2).Infof("node %v has been deleted", key)
		return nil
	}
	if err != nil {
		return err
	}

	// Deep-copy otherwise we are mutating our cache.
	node = node.DeepCopy()

	// Check for Deleted Node
	if node.DeletionTimestamp != nil {
		return nil
	}

	// First check if the node that was updated is this daemon's node
	if node.Name == dn.name {
		// stash the current node being processed
		dn.node = node
		// Pass to the shared update prep method
		needUpdate, err := dn.prepUpdateFromCluster()
		if err != nil {
			glog.Infof("Unable to prep update: %s", err)
			return err
		}
		if needUpdate {
			if err := dn.triggerUpdateWithMachineConfig(nil, nil); err != nil {
				glog.Infof("Unable to apply update: %s", err)
				return err
			}
		}
	}
	return nil
}

// enqueueAfter will enqueue a node after the provided amount of time.
func (dn *Daemon) enqueueAfter(node *corev1.Node, after time.Duration) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(node)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", node, err))
		return
	}

	dn.queue.AddAfter(key, after)
}

// enqueueDefault calls a default enqueue function
func (dn *Daemon) enqueueDefault(node *corev1.Node) {
	dn.enqueueAfter(node, updateDelay)
}

const (
	// IDs are taken from https://cgit.freedesktop.org/systemd/systemd/plain/src/systemd/sd-messages.h
	sdMessageSessionStart = "8d45620c1a4348dbb17410da57c60c66"
)

// detectEarlySSHAccessesFromBoot taints the node if we find a login before the daemon started up.
func (dn *Daemon) detectEarlySSHAccessesFromBoot() error {
	journalOutput, err := exec.Command("journalctl", "-b", "-o", "cat", "MESSAGE_ID="+sdMessageSessionStart).CombinedOutput()
	if err != nil {
		return err
	}
	if len(journalOutput) > 0 {
		glog.Info("Detected a login session before the daemon took over on first boot")
		glog.Infof("Applying annotation: %v", machineConfigDaemonSSHAccessAnnotationKey)
		if err := dn.applySSHAccessedAnnotation(); err != nil {
			return err
		}
	}
	return nil
}

// Run finishes informer setup and then blocks, and the informer will be
// responsible for triggering callbacks to handle updates. Successful
// updates shouldn't return, and should just reboot the node.
func (dn *Daemon) Run(stopCh <-chan struct{}, exitCh <-chan error) error {
	defer utilruntime.HandleCrash()
	defer dn.queue.ShutDown()

	if dn.kubeletHealthzEnabled {
		glog.Info("Enabling Kubelet Healthz Monitor")
		go dn.runKubeletHealthzMonitor(stopCh, dn.exitCh)
	}

	// Catch quickly if we've been asked to run once.
	if dn.onceFrom != "" {
		genericConfig, configType, contentFrom, err := dn.SenseAndLoadOnceFrom()
		if err != nil {
			glog.Warningf("Unable to decipher onceFrom config type: %s", err)
			return err
		}
		if configType == machineConfigIgnitionFileType {
			glog.V(2).Info("Daemon running directly from Ignition")
			ignConfig := genericConfig.(ignv2_2types.Config)
			return dn.runOnceFromIgnition(ignConfig)
		}
		if configType == machineConfigMCFileType {
			glog.V(2).Info("Daemon running directly from MachineConfig")
			mcConfig := genericConfig.(*(mcfgv1.MachineConfig))
			// this already sets the node as degraded on error in the in-cluster path
			return dn.runOnceFromMachineConfig(*mcConfig, contentFrom)
		}
	}

	if !cache.WaitForCacheSync(stopCh, dn.nodeListerSynced, dn.mcListerSynced) {
		return errors.New("failed to sync initial listers cache")
	}

	go wait.Until(dn.worker, time.Second, stopCh)

	for {
		select {
		case <-stopCh:
			return nil
		case err := <-exitCh:
			// This channel gets errors from auxiliary goroutines like loginmonitor and kubehealth
			glog.Warningf("Got an error from auxiliary tools: %v", err)
		}
	}
}

// BindPodMounts ensures that the daemon can still see e.g. /run/secrets/kubernetes.io
// service account tokens after chrooting.  This function must be called before chroot.
func (dn *Daemon) BindPodMounts() error {
	targetSecrets := filepath.Join(dn.rootMount, "/run/secrets")
	if err := os.MkdirAll(targetSecrets, 0755); err != nil {
		return err
	}
	// This will only affect our mount namespace, not the host
	mnt := exec.Command("mount", "--rbind", "/run/secrets", targetSecrets)
	return mnt.Run()
}

func (dn *Daemon) runLoginMonitor(stopCh <-chan struct{}, exitCh chan<- error) {
	cmd := exec.Command("journalctl", "-b", "-f", "-o", "cat", "MESSAGE_ID="+sdMessageSessionStart)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		exitCh <- err
		return
	}
	if err := cmd.Start(); err != nil {
		exitCh <- err
		return
	}
	worker := make(chan struct{})
 	go func() {
		for {
			select {
			case <-worker:
 				return
			default:
				buf := make([]byte, 1024)
				l, err := stdout.Read(buf)
				if err != nil {
					if err == io.EOF {
						return
					}
					exitCh <- err
					return
				}
				if l > 0 {
					line := strings.Split(string(buf), "\n")[0]
					glog.Infof("Detected a new login session: %s", line)
					glog.Infof("Login access is discouraged! Applying annotation: %v", machineConfigDaemonSSHAccessAnnotationKey)
					if err := dn.applySSHAccessedAnnotation(); err != nil {
						exitCh <- err
					}
				}
 			}
 		}
 	}()
	select {
	case <-stopCh:
		close(worker)
		cmd.Process.Kill()
	}
}

func (dn *Daemon) applySSHAccessedAnnotation() error {
	if err := dn.nodeWriter.SetSSHAccessed(dn.kubeClient.CoreV1().Nodes(), dn.nodeLister, dn.name); err != nil {
		return fmt.Errorf("error: cannot apply annotation for SSH access due to: %v", err)
	}
	return nil
}

func (dn *Daemon) runKubeletHealthzMonitor(stopCh <-chan struct{}, exitCh chan<- error) {
	failureCount := 0
	for {
		select {
		case <-stopCh:
			return
		case <-time.After(kubeletHealthzPollingInterval):
			if err := dn.getHealth(); err != nil {
				glog.Warningf("Failed kubelet health check: %v", err)
				failureCount++
				if failureCount >= kubeletHealthzFailureThreshold {
					exitCh <- fmt.Errorf("kubelet health failure threshold reached")
				}
			} else {
				failureCount = 0 // reset failure count on success
			}
		}
	}
}

func (dn *Daemon) getHealth() error {
	glog.V(2).Info("Kubelet health running")
	ctx, cancel := context.WithTimeout(context.Background(), kubeletHealthzTimeout)
	defer cancel()

	req, err := http.NewRequest("GET", dn.kubeletHealthzEndpoint, nil)
	if err != nil {
		return err
	}
	req = req.WithContext(ctx)

	client := http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respData, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if string(respData) != "ok" {
		glog.Warningf("Kubelet Healthz Endpoint returned: %s", string(respData))
		return nil
	}

	glog.V(2).Info("Kubelet health ok")

	return nil
}

// stateAndConfigs is the "state" node annotation plus parsed machine configs
// referenced by the currentConfig and desiredConfig annotations.  If we have
// a "pending" config (we're coming up after a reboot attempting to apply a config),
// we'll load that as well - otherwise it will be nil.
//
// If any of the object names are the same, they will be pointer-equal.
type stateAndConfigs struct {
	bootstrapping bool
	state         string
	currentConfig *mcfgv1.MachineConfig
	pendingConfig *mcfgv1.MachineConfig
	desiredConfig *mcfgv1.MachineConfig
}

func (dn *Daemon) getStateAndConfigs(pendingConfigName string) (*stateAndConfigs, error) {
	_, err := os.Lstat(constants.InitialNodeAnnotationsFilePath)
	var bootstrapping bool
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		// The node annotation file (laid down by the MCS)
		// doesn't exist, we must not be bootstrapping
	} else {
		bootstrapping = true
		glog.Info("In bootstrap mode")
	}

	currentConfigName, err := getNodeAnnotation(dn.node, constants.CurrentMachineConfigAnnotationKey)
	if err != nil {
		return nil, err
	}
	desiredConfigName, err := getNodeAnnotation(dn.node, constants.DesiredMachineConfigAnnotationKey)
	if err != nil {
		return nil, err
	}
	currentConfig, err := dn.mcLister.Get(currentConfigName)
	if err != nil {
		return nil, err
	}
	state, err := getNodeAnnotationExt(dn.node, constants.MachineConfigDaemonStateAnnotationKey, true)
	if err != nil {
		return nil, err
	}
	// Temporary hack: the MCS used to not write the state=done annotation
	// key.  If it's unset, let's write it now.
	if state == "" {
		state = constants.MachineConfigDaemonStateDone
	}

	var desiredConfig *mcfgv1.MachineConfig
	if currentConfigName == desiredConfigName {
		desiredConfig = currentConfig
		glog.Infof("Current+desired config: %s", currentConfigName)
	} else {
		desiredConfig, err = dn.mcLister.Get(desiredConfigName)
		if err != nil {
			return nil, err
		}

		glog.Infof("Current config: %s", currentConfigName)
		glog.Infof("Desired config: %s", desiredConfigName)
	}

	var pendingConfig *mcfgv1.MachineConfig
	// We usually expect that if current != desired, pending == desired; however,
	// it can happen that desiredConfig changed while we were rebooting.
	if pendingConfigName == desiredConfigName {
		pendingConfig = desiredConfig
	} else if pendingConfigName != "" {
		pendingConfig, err = dn.mcLister.Get(pendingConfigName)
		if err != nil {
			return nil, err
		}

		glog.Infof("Pending config: %s", pendingConfigName)
	}

	return &stateAndConfigs{
		bootstrapping: bootstrapping,
		currentConfig: currentConfig,
		pendingConfig: pendingConfig,
		desiredConfig: desiredConfig,
		state:         state,
	}, nil
}

// getPendingConfig loads the JSON state we cache across attempting to apply
// a config+reboot.  If no pending state is available, ("", nil) will be returned.
// The bootID is stored in the pending state; if it is unchanged, we assume
// that we failed to reboot; that for now should be a fatal error, in order to avoid
// reboot loops.
func (dn *Daemon) getPendingConfig() (string, error) {
	s, err := ioutil.ReadFile(pathStateJSON)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", errors.Wrapf(err, "loading transient state")
		}
		return "", nil
	}
	var p pendingConfigState
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return "", errors.Wrapf(err, "parsing transient state")
	}

	if p.BootID == dn.bootID {
		return "", fmt.Errorf("pending config %s bootID %s matches current! Failed to reboot?", p.PendingConfig, dn.bootID)
	}
	return p.PendingConfig, nil
}

// CheckStateOnBoot is a core entrypoint for our state machine.
// It determines whether we're in our desired state, or if we're
// transitioning between states, and whether or not we need to update
// to a new state. It also checks if someone jumped on a node before
// the daemon took over.
//
// Some more background in this PR: https://github.com/openshift/machine-config-operator/pull/245
func (dn *Daemon) CheckStateOnBoot() error {
	// Print status if available
	if dn.OperatingSystem == machineConfigDaemonOSRHCOS {
		status, err := dn.NodeUpdaterClient.GetStatus()
		if err != nil {
			glog.Fatalf("unable to get rpm-ostree status: %s", err)
		}
		glog.Info(status)
	}

	pendingConfigName, err := dn.getPendingConfig()
	if err != nil {
		return err
	}
	state, err := dn.getStateAndConfigs(pendingConfigName)
	if err != nil {
		return err
	}
	if err := dn.detectEarlySSHAccessesFromBoot(); err != nil {
		return fmt.Errorf("error detecting previous SSH accesses: %v", err)
	}

	if state.bootstrapping {
		targetOSImageURL := state.currentConfig.Spec.OSImageURL
		osMatch, err := dn.checkOS(targetOSImageURL)
		if err != nil {
			return err
		}
		if !osMatch {
			glog.Infof("Bootstrap pivot required to: %s", targetOSImageURL)
			// This only returns on error
			return dn.updateOSAndReboot(state.currentConfig)
		}
		glog.Info("No bootstrap pivot required; unlinking bootstrap node annotations")

		// Delete the bootstrap node annotations; the
		// currentConfig's osImageURL should now be *truth*.
		// In other words if it drifts somehow, we go degraded.
		if err := os.Remove(constants.InitialNodeAnnotationsFilePath); err != nil {
			return errors.Wrapf(err, "removing initial node annotations file")
		}
	}

	// Validate the on-disk state against what we *expect*.
	//
	// In the case where we're booting a node for the first time, or the MCD
	// is restarted, that will be the current config.
	//
	// In the case where we have
	// a pending config, this is where we validate that it actually applied.
	// We currently just do this on startup, but in the future it could e.g. be
	// a once-a-day or week cron job.
	var expectedConfig *mcfgv1.MachineConfig
	if state.pendingConfig != nil {
		expectedConfig = state.pendingConfig
	} else {
		expectedConfig = state.currentConfig
	}
	if isOnDiskValid := dn.validateOnDiskState(expectedConfig); !isOnDiskValid {
		return errors.New("unexpected on-disk state")
	}
	glog.Info("Validated on-disk state")

	// We've validated our state.  In the case where we had a pendingConfig,
	// make that now currentConfig.  We update the node annotation, delete the
	// state file, etc.
	//
	// However, it may be the case that desiredConfig changed while we
	// were coming up, so we next look at that before uncordoning the node (so
	// we don't uncordon and then immediately re-cordon)
	if state.pendingConfig != nil {
		if err := dn.nodeWriter.SetUpdateDone(dn.kubeClient.CoreV1().Nodes(), dn.nodeLister, dn.name, state.pendingConfig.GetName()); err != nil {
			return err
		}
		// And remove the pending state file
		if err := os.Remove(pathStateJSON); err != nil {
			return errors.Wrapf(err, "removing transient state file")
		}

		state.currentConfig = state.pendingConfig
	}

	inDesiredConfig := state.currentConfig == state.desiredConfig
	if inDesiredConfig {
		if state.pendingConfig != nil {
			// Great, we've successfully rebooted for the desired config,
			// let's mark it done!
			glog.Infof("Completing pending config %s", state.pendingConfig.GetName())
			if err := dn.completeUpdate(dn.node, state.pendingConfig.GetName()); err != nil {
				return err
			}
		}

		glog.Infof("In desired config %s", state.currentConfig.GetName())

		// All good!
		return nil
	}
	// currentConfig != desiredConfig, and we're not booting up into the desiredConfig.
	// Kick off an update.
	return dn.triggerUpdateWithMachineConfig(state.currentConfig, state.desiredConfig)
}

// runOnceFromMachineConfig utilizes a parsed machineConfig and executes in onceFrom
// mode. If the content was remote, it executes cluster calls, otherwise it assumes
// no cluster is present yet.
func (dn *Daemon) runOnceFromMachineConfig(machineConfig mcfgv1.MachineConfig, contentFrom string) error {
	if contentFrom == machineConfigOnceFromRemoteConfig {
		// NOTE: This case expects a cluster to exists already.
		needUpdate, err := dn.prepUpdateFromCluster()
		if err != nil {
			dn.nodeWriter.SetDegraded(err, dn.kubeClient.CoreV1().Nodes(), dn.nodeLister, dn.name)
			return err
		}
		if !needUpdate {
			return nil
		}
		// At this point we have verified we need to update
		if err := dn.triggerUpdateWithMachineConfig(nil, &machineConfig); err != nil {
			dn.nodeWriter.SetDegraded(err, dn.kubeClient.CoreV1().Nodes(), dn.nodeLister, dn.name)
			return err
		}
		return nil
	}
	if contentFrom == machineConfigOnceFromLocalConfig {
		// NOTE: This case expects that the cluster is NOT CREATED YET.
		oldConfig := mcfgv1.MachineConfig{}
		// Execute update without hitting the cluster
		return dn.update(&oldConfig, &machineConfig)
	}
	// Otherwise return an error as the input format is unsupported
	return fmt.Errorf("%s is not a path nor url; can not run once", dn.onceFrom)
}

// runOnceFromIgnition executes MCD's subset of Ignition functionality in onceFrom mode
func (dn *Daemon) runOnceFromIgnition(ignConfig ignv2_2types.Config) error {
	// Execute update without hitting the cluster
	if err := dn.writeFiles(ignConfig.Storage.Files); err != nil {
		return err
	}
	if err := dn.writeUnits(ignConfig.Systemd.Units); err != nil {
		return err
	}
	return dn.reboot("runOnceFromIgnition complete")
}

func (dn *Daemon) handleNodeUpdate(old, cur interface{}) {
	oldNode := old.(*corev1.Node)
	curNode := cur.(*corev1.Node)

	glog.V(4).Infof("Updating Node %s", oldNode.Name)
	dn.enqueueNode(curNode)
}

// prepUpdateFromCluster handles the shared update prepping functionality for
// flows that expect the cluster to already be available. Returns true if an
// update is required, false otherwise.
func (dn *Daemon) prepUpdateFromCluster() (bool, error) {
	desiredConfigName, err := getNodeAnnotationExt(dn.node, constants.DesiredMachineConfigAnnotationKey, true)
	if err != nil {
		return false, err
	}
	// currentConfig is always expected to be there as loadNodeAnnotations
	// is one of the very first calls when the daemon starts.
	currentConfigName, err := getNodeAnnotation(dn.node, constants.CurrentMachineConfigAnnotationKey)
	if err != nil {
		return false, err
	}

	// Detect if there is an update
	if desiredConfigName == currentConfigName {
		// No actual update to the config
		glog.V(2).Info("No updating is required")
		return false, nil
	}
	return true, nil
}

// completeUpdate marks the node as schedulable again, then deletes the
// "transient state" file, which signifies that all of those prior steps have
// been completed.
func (dn *Daemon) completeUpdate(node *corev1.Node, desiredConfigName string) error {
	if err := drain.Uncordon(dn.kubeClient.CoreV1().Nodes(), node, nil); err != nil {
		return err
	}

	dn.logSystem("machine-config-daemon: completed update for config %s", desiredConfigName)

	return nil
}

// triggerUpdateWithMachineConfig starts the update. It queries the cluster for
// the current and desired config if they weren't passed.
func (dn *Daemon) triggerUpdateWithMachineConfig(currentConfig *mcfgv1.MachineConfig, desiredConfig *mcfgv1.MachineConfig) error {
	if currentConfig == nil {
		ccAnnotation, err := getNodeAnnotation(dn.node, constants.CurrentMachineConfigAnnotationKey)
		if err != nil {
			return err
		}
		currentConfig, err = dn.mcLister.Get(ccAnnotation)
		if err != nil {
			return err
		}
	}

	if desiredConfig == nil {
		dcAnnotation, err := getNodeAnnotation(dn.node, constants.DesiredMachineConfigAnnotationKey)
		if err != nil {
			return err
		}
		desiredConfig, err = dn.mcLister.Get(dcAnnotation)
		if err != nil {
			return err
		}
	}

	// run the update process. this function doesn't currently return.
	return dn.update(currentConfig, desiredConfig)
}

// validateOnDiskState compares the on-disk state against what a configuration
// specifies.  If for example an admin ssh'd into a node, or another operator
// is stomping on our files, we want to highlight that and mark the system
// degraded.
func (dn *Daemon) validateOnDiskState(currentConfig *mcfgv1.MachineConfig) bool {
	// Be sure we're booted into the OS we expect
	osMatch, err := dn.checkOS(currentConfig.Spec.OSImageURL)
	if err != nil {
		glog.Errorf("%s", err)
		return false
	}
	if !osMatch {
		glog.Errorf("expected target osImageURL %s", currentConfig.Spec.OSImageURL)
		return false
	}
	// And the rest of the disk state
	if !checkFiles(currentConfig.Spec.Config.Storage.Files) {
		return false
	}
	if !checkUnits(currentConfig.Spec.Config.Systemd.Units) {
		return false
	}
	return true
}

// getRefDigest parses a Docker/OCI image reference and returns
// its digest, or an error if the string fails to parse as
// a "canonical" image reference with a digest.
func getRefDigest(ref string) (string, error) {
	refParsed, err := imgref.ParseNamed(ref)
	if err != nil {
		return "", errors.Wrapf(err, "parsing reference: %q", ref)
	}
	canon, ok := refParsed.(imgref.Canonical)
	if !ok {
		return "", fmt.Errorf("not canonical form: %q", ref)
	}

	return canon.Digest().String(), nil
}

// compareOSImageURL is the backend for checkOS.
func compareOSImageURL(current, desired string) (bool, error) {
	// Since https://github.com/openshift/machine-config-operator/pull/426 landed
	// we don't use the "unspecified" osImageURL anymore, but let's keep supporting
	// it for now.
	// The ://dummy syntax is legacy
	if desired == "" || desired == "://dummy" {
		glog.Info(`No target osImageURL provided`)
		return true, nil
	}

	if current == desired {
		return true, nil
	}

	bootedDigest, err := getRefDigest(current)
	if err != nil {
		return false, errors.Wrap(err, "parsing booted osImageURL")
	}
	desiredDigest, err := getRefDigest(desired)
	if err != nil {
		return false, errors.Wrap(err, "parsing desired osImageURL")
	}

	if bootedDigest == desiredDigest {
		glog.Infof("Current and target osImageURL have matching digest %q", bootedDigest)
		return true, nil
	}

	return false, nil
}

// checkOS determines whether the booted system matches the target
// osImageURL and if not whether we need to take action.  This function
// returns `true` if no action is required, which is the case if we're
// not running RHCOS, or if the target osImageURL is "" (unspecified),
// or if the digests match.
// Otherwise if `false` is returned, then we need to perform an update.
func (dn *Daemon) checkOS(osImageURL string) (bool, error) {
	// Nothing to do if we're not on RHCOS
	if dn.OperatingSystem != machineConfigDaemonOSRHCOS {
		glog.Infof(`Not booted into Red Hat CoreOS, ignoring target OSImageURL %s`, osImageURL)
		return true, nil
	}

	return compareOSImageURL(dn.bootedOSImageURL, osImageURL)
}

// checkUnits validates the contents of all the units in the
// target config and retursn true if they match.
func checkUnits(units []ignv2_2types.Unit) bool {
	for _, u := range units {
		for j := range u.Dropins {
			path := filepath.Join(pathSystemd, u.Name+".d", u.Dropins[j].Name)
			if status := checkFileContentsAndMode(path, []byte(u.Dropins[j].Contents), defaultFilePermissions); !status {
				return false
			}
		}

		if u.Contents == "" {
			continue
		}

		path := filepath.Join(pathSystemd, u.Name)
		if u.Mask {
			link, err := filepath.EvalSymlinks(path)
			if err != nil {
				glog.Errorf("state validation: error while evaluation symlink for path: %q, err: %v", path, err)
				return false
			}
			if strings.Compare(pathDevNull, link) != 0 {
				glog.Errorf("state validation: invalid unit masked setting. path: %q; expected: %v; received: %v", path, pathDevNull, link)
				return false
			}
		}
		if status := checkFileContentsAndMode(path, []byte(u.Contents), defaultFilePermissions); !status {
			return false
		}

	}
	return true
}

// checkFiles validates the contents of  all the files in the
// target config.
func checkFiles(files []ignv2_2types.File) bool {
	checkedFiles := make(map[string]bool)
	for i := len(files) - 1; i >= 0; i-- {
		f := files[i]
		// skip over checked validated files
		if _, ok := checkedFiles[f.Path]; ok {
			continue
		}
		mode := defaultFilePermissions
		if f.Mode != nil {
			mode = os.FileMode(*f.Mode)
		}
		contents, err := dataurl.DecodeString(f.Contents.Source)
		if err != nil {
			glog.Errorf("couldn't parse file: %v", err)
			return false
		}
		if status := checkFileContentsAndMode(f.Path, contents.Data, mode); !status {
			return false
		}
		checkedFiles[f.Path] = true
	}
	return true
}

// checkFileContentsAndMode reads the file from the filepath and compares its
// contents and mode with the expectedContent and mode parameters. It logs an
// error in case of an error or mismatch and returns the status of the
// evaluation.
func checkFileContentsAndMode(filePath string, expectedContent []byte, mode os.FileMode) bool {
	fi, err := os.Lstat(filePath)
	if err != nil {
		glog.Errorf("could not stat file: %q, error: %v", filePath, err)
		return false
	}
	if fi.Mode() != mode {
		glog.Errorf("mode mismatch for file: %q; expected: %v; received: %v", filePath, mode, fi.Mode())
		return false
	}
	contents, err := ioutil.ReadFile(filePath)
	if err != nil {
		glog.Errorf("could not read file: %q, error: %v", filePath, err)
		return false
	}
	if !bytes.Equal(contents, expectedContent) {
		glog.Errorf("content mismatch for file: %q", filePath)
		return false
	}
	return true
}

// Close closes all the connections the node agent has open for it's lifetime
func (dn *Daemon) Close() {
}

// ValidPath attempts to see if the path provided is indeed an acceptable
// filesystem path. This function does not check if the path exists.
func ValidPath(path string) bool {
	for _, validStart := range []string{".", "..", "/"} {
		if strings.HasPrefix(path, validStart) {
			return true
		}
	}
	return false
}

// SenseAndLoadOnceFrom gets a hold of the content for supported onceFrom configurations,
// parses to verify the type, and returns back the genericInterface, the type description,
// if it was local or remote, and error.
func (dn *Daemon) SenseAndLoadOnceFrom() (interface{}, string, string, error) {
	var content []byte
	var err error
	var contentFrom string
	// Read the content from a remote endpoint if requested
	if strings.HasPrefix(dn.onceFrom, "http://") || strings.HasPrefix(dn.onceFrom, "https://") {
		contentFrom = machineConfigOnceFromRemoteConfig
		resp, err := http.Get(dn.onceFrom)
		if err != nil {
			return nil, "", contentFrom, err
		}
		defer resp.Body.Close()
		// Read the body content from the request
		content, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, "", contentFrom, err
		}
	} else {
		// Otherwise read it from a local file
		contentFrom = machineConfigOnceFromLocalConfig
		absoluteOnceFrom, err := filepath.Abs(filepath.Clean(dn.onceFrom))
		if err != nil {
			return nil, "", contentFrom, err
		}

		content, err = ioutil.ReadFile(absoluteOnceFrom)
		if err != nil {
			return nil, "", contentFrom, err
		}
	}

	// Try each supported parser
	ignConfig, _, err := ignv2.Parse(content)
	if err == nil && ignConfig.Ignition.Version != "" {
		glog.V(2).Info("onceFrom file is of type Ignition")
		return ignConfig, machineConfigIgnitionFileType, contentFrom, nil
	}

	glog.V(2).Infof("%s is not an Ignition config: %s. Trying MachineConfig.", dn.onceFrom, err)

	// Try to parse as a machine config
	mc, err := resourceread.ReadMachineConfigV1(content)
	if err == nil {
		glog.V(2).Info("onceFrom file is of type MachineConfig")
		return mc, machineConfigMCFileType, contentFrom, nil
	}

	return nil, "", "", fmt.Errorf("unable to decipher onceFrom config type: %s", err)
}
