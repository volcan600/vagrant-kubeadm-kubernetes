/*
Copyright 2016 The Rook Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package operator to manage Kubernetes storage.
package operator

import (
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/coreos/pkg/capnslog"
	"github.com/pkg/errors"
	"github.com/rook/rook/pkg/clusterd"
	"github.com/rook/rook/pkg/daemon/ceph/agent/flexvolume"
	"github.com/rook/rook/pkg/daemon/ceph/agent/flexvolume/attachment"
	"github.com/rook/rook/pkg/operator/ceph/agent"
	"github.com/rook/rook/pkg/operator/ceph/cluster"
	"github.com/rook/rook/pkg/operator/ceph/csi"
	"github.com/rook/rook/pkg/operator/ceph/provisioner"
	"github.com/rook/rook/pkg/operator/discover"
	"github.com/rook/rook/pkg/operator/k8sutil"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/sig-storage-lib-external-provisioner/controller"
)

// volume provisioner constant
const (
	provisionerName       = "ceph.rook.io/block"
	provisionerNameLegacy = "rook.io/block"
)

var logger = capnslog.NewPackageLogger("github.com/rook/rook", "operator")

// The supported configurations for the volume provisioner
var provisionerConfigs = map[string]string{
	provisionerName:       flexvolume.FlexvolumeVendor,
	provisionerNameLegacy: flexvolume.FlexvolumeVendorLegacy,
}

var (
	// Whether to enable the flex driver. If true, the rook-ceph-agent daemonset will be started.
	EnableFlexDriver = true
	// Whether to enable the daemon for device discovery. If true, the rook-ceph-discover daemonset will be started.
	EnableDiscoveryDaemon = true

	// ImmediateRetryResult Return this for a immediate retry of the reconciliation loop with the same request object.
	ImmediateRetryResult = reconcile.Result{Requeue: true}
	// WaitForRequeueIfCephClusterNotReadyAfter requeue after 10sec if the operator is not ready
	WaitForRequeueIfCephClusterNotReadyAfter = 10 * time.Second
	// WaitForRequeueIfCephClusterNotReady waits for the CephCluster to be ready
	WaitForRequeueIfCephClusterNotReady = reconcile.Result{Requeue: true, RequeueAfter: WaitForRequeueIfCephClusterNotReadyAfter}
)

// Operator type for managing storage
type Operator struct {
	context           *clusterd.Context
	resources         []k8sutil.CustomResource
	operatorNamespace string
	rookImage         string
	securityAccount   string
	// The custom resource that is global to the kubernetes cluster.
	// The cluster is global because you create multiple clusters in k8s
	clusterController     *cluster.ClusterController
	delayedDaemonsStarted bool
}

// New creates an operator instance
func New(context *clusterd.Context, volumeAttachmentWrapper attachment.Attachment, rookImage, securityAccount string) *Operator {
	schemes := []k8sutil.CustomResource{cluster.ClusterResource, attachment.VolumeResource}

	operatorNamespace := os.Getenv(k8sutil.PodNamespaceEnvVar)
	o := &Operator{
		context:           context,
		resources:         schemes,
		operatorNamespace: operatorNamespace,
		rookImage:         rookImage,
		securityAccount:   securityAccount,
	}
	operatorConfigCallbacks := []func() error{
		o.updateDrivers,
	}
	addCallbacks := []func() error{
		o.startDrivers,
	}
	o.clusterController = cluster.NewClusterController(context, rookImage, volumeAttachmentWrapper, operatorConfigCallbacks, addCallbacks)
	return o
}

// Run the operator instance
func (o *Operator) Run() error {

	if o.operatorNamespace == "" {
		return errors.Errorf("rook operator namespace is not provided. expose it via downward API in the rook operator manifest file using environment variable %s", k8sutil.PodNamespaceEnvVar)
	}

	if EnableDiscoveryDaemon {
		rookDiscover := discover.New(o.context.Clientset)
		if err := rookDiscover.Start(o.operatorNamespace, o.rookImage, o.securityAccount, true); err != nil {
			return errors.Wrapf(err, "error starting device discovery daemonset")
		}
	}

	serverVersion, err := o.context.Clientset.Discovery().ServerVersion()
	if err != nil {
		return errors.Wrapf(err, "error getting server version")
	}

	signalChan := make(chan os.Signal, 1)
	stopChan := make(chan struct{})
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	// Run volume provisioner for each of the supported configurations
	for name, vendor := range provisionerConfigs {
		volumeProvisioner := provisioner.New(o.context, vendor)
		pc := controller.NewProvisionController(
			o.context.Clientset,
			name,
			volumeProvisioner,
			serverVersion.GitVersion,
		)
		go pc.Run(stopChan)
		logger.Infof("rook-provisioner %s started using %s flex vendor dir", name, vendor)
	}

	var namespaceToWatch string
	if os.Getenv("ROOK_CURRENT_NAMESPACE_ONLY") == "true" {
		logger.Infof("Watching the current namespace for a cluster CRD")
		namespaceToWatch = o.operatorNamespace
	} else {
		logger.Infof("Watching all namespaces for cluster CRDs")
		namespaceToWatch = v1.NamespaceAll
	}

	// Start the controller-runtime Manager.
	go o.startManager(namespaceToWatch, stopChan)

	// watch for changes to the rook clusters
	o.clusterController.StartWatch(namespaceToWatch, stopChan)

	for {
		select {
		case <-signalChan:
			logger.Infof("shutdown signal received, exiting...")
			close(stopChan)
			o.clusterController.StopWatch()
			return nil
		}
	}
}

func (o *Operator) startDrivers() error {
	if o.delayedDaemonsStarted {
		return nil
	}

	o.delayedDaemonsStarted = true
	if err := o.updateDrivers(); err != nil {
		o.delayedDaemonsStarted = false // unset because failed to updateDrivers
		return err
	}

	return nil
}

func (o *Operator) updateDrivers() error {
	var err error

	// Skipping CSI driver update since the first cluster hasn't been started yet
	if !o.delayedDaemonsStarted {
		return nil
	}

	if o.operatorNamespace == "" {
		return errors.Errorf("rook operator namespace is not provided. expose it via downward API in the rook operator manifest file using environment variable %s", k8sutil.PodNamespaceEnvVar)
	}

	if EnableFlexDriver {
		rookAgent := agent.New(o.context.Clientset)
		if err := rookAgent.Start(o.operatorNamespace, o.rookImage, o.securityAccount); err != nil {
			return errors.Wrapf(err, "error starting agent daemonset")
		}
	}

	serverVersion, err := o.context.Clientset.Discovery().ServerVersion()
	if err != nil {
		return errors.Wrapf(err, "error getting server version")
	}

	if err = csi.SetParams(o.context.Clientset); err != nil {
		return errors.Wrap(err, "failed to configure CSI parameters")
	}

	if !csi.CSIEnabled() {
		logger.Infof("CSI driver is not enabled")
		return nil
	}

	if serverVersion.Major < csi.KubeMinMajor || serverVersion.Major == csi.KubeMinMajor && serverVersion.Minor < csi.KubeMinMinor {
		logger.Infof("CSI drivers only supported in K8s 1.13 or newer. version=%s", serverVersion.String())
		// disable csi control variables to disable other csi functions
		csi.EnableRBD = false
		csi.EnableCephFS = false
		return nil
	}

	ownerRef, err := getDeploymentOwnerReference(o.context.Clientset, o.operatorNamespace)
	if err != nil {
		logger.Warningf("could not find deployment owner reference to assign to csi drivers. %v", err)
	}
	if ownerRef != nil {
		blockOwnerDeletion := false
		ownerRef.BlockOwnerDeletion = &blockOwnerDeletion
	}

	// create an empty config map. config map will be filled with data
	// later when clusters have mons
	err = csi.CreateCsiConfigMap(o.operatorNamespace, o.context.Clientset, ownerRef)
	if err != nil {
		return errors.Wrap(err, "failed creating csi config map")
	}

	if err = csi.ValidateCSIParam(); err != nil {
		return errors.Wrap(err, "invalid csi params")
	}

	go csi.ValidateAndStartDrivers(o.context.Clientset, o.operatorNamespace, o.rookImage, o.securityAccount, serverVersion, ownerRef)
	return nil
}

// getDeploymentOwnerReference returns an OwnerReference to the rook-ceph-operator deployment
func getDeploymentOwnerReference(clientset kubernetes.Interface, namespace string) (*metav1.OwnerReference, error) {
	var deploymentRef *metav1.OwnerReference
	podName := os.Getenv(k8sutil.PodNameEnvVar)
	pod, err := clientset.CoreV1().Pods(namespace).Get(podName, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrapf(err, "could not find pod %q to find deployment owner reference", podName)
	}
	for _, podOwner := range pod.OwnerReferences {
		if podOwner.Kind == "ReplicaSet" {
			replicaset, err := clientset.AppsV1().ReplicaSets(namespace).Get(podOwner.Name, metav1.GetOptions{})
			if err != nil {
				return nil, errors.Wrapf(err, "could not find replicaset %q to find deployment owner reference", podOwner.Name)
			}
			for _, replicasetOwner := range replicaset.OwnerReferences {
				if replicasetOwner.Kind == "Deployment" {
					deploymentRef = &replicasetOwner
				}
			}
		}
	}
	if deploymentRef == nil {
		return nil, errors.New("could not find owner reference for rook-ceph deployment")
	}
	return deploymentRef, nil
}
