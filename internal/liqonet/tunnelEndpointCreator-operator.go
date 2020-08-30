/*

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

package controllers

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	discoveryv1alpha1 "github.com/liqoTech/liqo/api/discovery/v1alpha1"
	advtypes "github.com/liqoTech/liqo/api/sharing/v1alpha1"
	"github.com/liqoTech/liqo/internal/crdReplicator"
	liqonetOperator "github.com/liqoTech/liqo/pkg/liqonet"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog"
	"net"
	"sync"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netv1alpha1 "github.com/liqoTech/liqo/api/net/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const (
	TunEndpointNamePrefix = "tun-endpoint-"
	NetConfigNamePrefix   = "net-config-"
	defaultPodCIDRValue   = "None"
)

var (
	result = ctrl.Result{
		Requeue:      false,
		RequeueAfter: 5 * time.Second,
	}
)

type networkParam struct {
	clusterID        string
	gatewayIP        string
	podCIDR          string
	localNatPodCIDR  string
	remoteNatPodCIDR string
}

type TunnelEndpointCreator struct {
	client.Client
	Log                logr.Logger
	Scheme             *runtime.Scheme
	DynClient          dynamic.Interface
	GatewayIP          string
	PodCIDR            string
	ServiceCIDR        string
	netParamPerCluster map[string]networkParam
	ReservedSubnets    map[string]*net.IPNet
	IPManager          liqonetOperator.IpManager
	Mutex              sync.Mutex
	IsConfigured       bool
	Configured         chan bool
	AdvWatcher         chan bool
	PReqWatcher        chan bool
	RunningWatchers    bool
	RetryTimeout       time.Duration
}

// +kubebuilder:rbac:groups=sharing.liqo.io,resources=advertisements,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sharing.liqo.io,resources=advertisements/status,verbs=get;update;patch

//rbac for the net.liqo.io api
// +kubebuilder:rbac:groups=net.liqo.io,resources=tunnelendpoints,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=net.liqo.io,resources=tunnelendpoints/status,verbs=get;update;patch

func (r *TunnelEndpointCreator) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	if !r.IsConfigured {
		<-r.Configured
		klog.Infof("operator configured")
	}
	ctx := context.Background()
	tunnelEndpointCreatorFinalizer := "tunnelEndpointCreator-Finalizer.liqonet.liqo.io"
	// get networkConfig
	var netConfig netv1alpha1.NetworkConfig
	if err := r.Get(ctx, req.NamespacedName, &netConfig); apierrors.IsNotFound(err) {
		// reconcile was triggered by a delete request
		klog.Infof("resource %s not found, probably it was deleted", req.NamespacedName)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	} else if err != nil {
		klog.Errorf("an error occurred while getting resource %s: %s", req.NamespacedName, err)
		return result, err
	}
	// examine DeletionTimestamp to determine if object is under deletion
	if netConfig.ObjectMeta.DeletionTimestamp.IsZero() {
		if !liqonetOperator.ContainsString(netConfig.ObjectMeta.Finalizers, tunnelEndpointCreatorFinalizer) {
			// The object is not being deleted, so if it does not have our finalizer,
			// then lets add the finalizer and update the object. This is equivalent
			// registering our finalizer.
			netConfig.ObjectMeta.Finalizers = append(netConfig.Finalizers, tunnelEndpointCreatorFinalizer)
			if err := r.Update(ctx, &netConfig); err != nil {
				//while updating we check if the a resource version conflict happened
				//which means the version of the object we have is outdated.
				//a solution could be to return an error and requeue the object for later process
				//but if the object has been changed by another instance of the controller running in
				//another host it already has been put in the working queue so decide to forget the
				//current version and process the next item in the queue assured that we handle the object later
				if apierrors.IsConflict(err) {
					return ctrl.Result{}, nil
				}
				klog.Errorf("an error occurred while setting finalizer for resource %s: %s", req.NamespacedName, err)
				return result, err
			}
			return result, nil
		}
	} else {
		//the object is being deleted
		if liqonetOperator.ContainsString(netConfig.Finalizers, tunnelEndpointCreatorFinalizer) {
			/*if err := r.deleteTunEndpoint(&netConfig); err != nil {
				log.Error(err, "error while deleting endpoint")
				return result, err
			}*/

			//remove the finalizer from the list and update it.
			netConfig.Finalizers = liqonetOperator.RemoveString(netConfig.Finalizers, tunnelEndpointCreatorFinalizer)
			if err := r.Update(ctx, &netConfig); err != nil {
				if apierrors.IsConflict(err) {
					return ctrl.Result{}, nil
				}
				klog.Errorf("an error occurred while removing finalizer from resource %s: %s", req.NamespacedName, err)
				return result, err
			}
		}
		//remove the reserved ip for the cluster
		r.IPManager.RemoveReservedSubnet(netConfig.Spec.ClusterID)
		return result, nil
	}

	//check if the netconfig is local or remote
	labels := netConfig.GetLabels()
	if val, ok := labels[crdReplicator.LocalLabelSelector]; ok && val == "true" {
		return result, r.processLocalNetConfig(&netConfig)
	} else {
		return result, r.processRemoteNetConfig(&netConfig)
	}
}

func (r *TunnelEndpointCreator) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&netv1alpha1.NetworkConfig{}).
		Complete(r)
}

func (d *TunnelEndpointCreator) Watcher(dynClient dynamic.Interface, gvr schema.GroupVersionResource, handler func(obj *unstructured.Unstructured), start chan bool) {
	<-start
	klog.Infof("starting watcher for %s", gvr.String())
	watcher, err := dynClient.Resource(gvr).Watch(context.TODO(), metav1.ListOptions{})
	if err != nil {
		klog.Errorf("an error occurred while starting watcher for resource %s: %s", gvr, err)
		return
	}
	for event := range watcher.ResultChan() {
		obj, ok := event.Object.(*unstructured.Unstructured)
		if !ok {
			klog.Infof("an error occurred while casting e.object to *unstructured.Unstructured")
		}
		switch event.Type {
		case watch.Added:
			handler(obj)
		case watch.Modified:
			handler(obj)
		}
	}
}

func (r *TunnelEndpointCreator) createNetConfig(clusterID string) error {
	netConfig := netv1alpha1.NetworkConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: NetConfigNamePrefix + clusterID,
			Labels: map[string]string{
				crdReplicator.LocalLabelSelector: "true",
				crdReplicator.DestinationLabel:   clusterID,
			},
		},
		Spec: netv1alpha1.NetworkConfigSpec{
			ClusterID:      clusterID,
			PodCIDR:        r.PodCIDR,
			TunnelPublicIP: r.GatewayIP,
		},
		Status: netv1alpha1.NetworkConfigStatus{},
	}
	err := r.Create(context.TODO(), &netConfig)
	if apierrors.IsAlreadyExists(err) {
		return nil
	} else if err != nil {
		klog.Errorf("an error occurred while creating resource %s of type %s: %s", netConfig.Name, netv1alpha1.GroupVersion.String(), err)
		return err
	} else {
		klog.Infof("resource %s of type %s created", netConfig.Name, netv1alpha1.GroupVersion.String())
		return nil
	}
}

func (r *TunnelEndpointCreator) processRemoteNetConfig(netConfig *netv1alpha1.NetworkConfig) error {
	if netConfig.Status.NATEnabled == "" {
		//check if the PodCidr of the remote cluster overlaps with any of the subnets on the local cluster
		_, subnet, err := net.ParseCIDR(netConfig.Spec.PodCIDR)
		if err != nil {
			klog.Errorf("an error occurred while parsing the PodCIDR of resource %s: %s", netConfig.Name, err)
			return err
		}
		r.Mutex.Lock()
		defer r.Mutex.Unlock()
		subnet, err = r.IPManager.GetNewSubnetPerCluster(subnet, netConfig.Spec.ClusterID)
		if err != nil {
			klog.Errorf("an error occurred while getting a new subnet for resource %s: %s", netConfig.Name, err)
			return err
		}
		if subnet != nil {
			//update netConfig status
			netConfig.Status.PodCIDRNAT = subnet.String()
			netConfig.Status.NATEnabled = "true"
			err := r.Status().Update(context.Background(), netConfig)
			if err != nil {
				klog.Errorf("an error occurred while updating the status of resource %s: %s", netConfig.Name, err)
				return err
			}
		} else {
			//update netConfig status
			netConfig.Status.PodCIDRNAT = defaultPodCIDRValue
			netConfig.Status.NATEnabled = "false"
			err := r.Status().Update(context.Background(), netConfig)
			if err != nil {
				klog.Errorf("an error occurred while updating the status of resource %s: %s", netConfig.Name, err)
				return err
			}
		}
		return nil
	}
	return nil
}

func (r *TunnelEndpointCreator) processLocalNetConfig(netConfig *netv1alpha1.NetworkConfig) error {
	//check if the resource has been processed by the remote cluster
	if netConfig.Status.PodCIDRNAT == "" {
		return nil
	}
	//we get the remote netconfig related to this one
	netConfigList := &netv1alpha1.NetworkConfigList{}
	labels := client.MatchingLabels{crdReplicator.RemoteLabelSelector: netConfig.Spec.ClusterID}
	err := r.List(context.Background(), netConfigList, labels)
	if err != nil {
		klog.Errorf("an error occurred while listing resources: %s", err)
		return err
	}
	if len(netConfigList.Items) != 1 {
		if len(netConfigList.Items) == 0 {
			return nil
		} else {
			klog.Errorf("more than one instances of type %s exists for remote cluster %s", netv1alpha1.GroupVersion.String(), netConfig.Spec.ClusterID)
			return fmt.Errorf("multiple instances of %s for remote cluster %s", netv1alpha1.GroupVersion.String(), netConfig.Spec.ClusterID)
		}
	} else {
		//check if it has been processed by the operator
		if netConfigList.Items[0].Status.NATEnabled == "" {
			return nil
		}
	}
	//at this point we have all the necessary parameters to create the tunnelEndpoint resource
	remoteNetConf := netConfigList.Items[0]
	netParam := networkParam{
		clusterID:        netConfig.Spec.ClusterID,
		gatewayIP:        remoteNetConf.Spec.TunnelPublicIP,
		podCIDR:          remoteNetConf.Spec.PodCIDR,
		localNatPodCIDR:  netConfig.Status.PodCIDRNAT,
		remoteNatPodCIDR: remoteNetConf.Status.PodCIDRNAT,
	}
	if err := r.ProcessTunnelEndpoint(netParam); err != nil {
		klog.Errorf("an error occurred while processing the tunnelEndpoint: %s", err)
		return err
	}
	return nil
}

func (r *TunnelEndpointCreator) ProcessTunnelEndpoint(param networkParam) error {
	tepName := TunEndpointNamePrefix + param.clusterID
	//try to get the tunnelEndpoint, it may not exist
	_, found, err := r.GetTunnelEndpoint(tepName)
	if err != nil {
		klog.Errorf("an error occurred while getting resource %s: %s", TunEndpointNamePrefix+param.clusterID, err)
		return err
	}
	if !found {
		return r.CreateTunnelEndpoint(param)
	} else {
		if err := r.UpdateSpecTunnelEndpoint(param); err != nil {
			return err
		}
		if err := r.UpdateStatusTunnelEndpoint(param); err != nil {
			return err
		}
		return nil
	}
}

func (r *TunnelEndpointCreator) UpdateSpecTunnelEndpoint(param networkParam) error {
	tepName := TunEndpointNamePrefix + param.clusterID
	tep := &netv1alpha1.TunnelEndpoint{}

	//here we recover from conflicting resource versions
	retryError := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		toBeUpdated := false
		err := r.Get(context.Background(), client.ObjectKey{
			Name: tepName,
		}, tep)
		if err != nil {
			return err
		}
		//check if there are fields to be updated
		if tep.Spec.ClusterID != param.clusterID {
			tep.Spec.ClusterID = param.clusterID
			toBeUpdated = true
		}
		if tep.Spec.TunnelPublicIP != param.gatewayIP {
			tep.Spec.TunnelPublicIP = param.gatewayIP
			toBeUpdated = true
		}
		if tep.Spec.PodCIDR != param.podCIDR {
			tep.Spec.PodCIDR = param.podCIDR
			toBeUpdated = true
		}
		if toBeUpdated {
			err = r.Update(context.Background(), tep)
			return err
		}
		return nil
	})
	if retryError != nil {
		klog.Errorf("an error occurred while updating spec of tunnelEndpoint resource %s: %s", tep.Name, retryError)
		return retryError
	}
	return nil
}

func (r *TunnelEndpointCreator) UpdateStatusTunnelEndpoint(param networkParam) error {
	tepName := TunEndpointNamePrefix + param.clusterID
	tep := &netv1alpha1.TunnelEndpoint{}

	//here we recover from conflicting resource versions
	retryError := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		toBeUpdated := false
		err := r.Get(context.Background(), client.ObjectKey{
			Name: tepName,
		}, tep)
		if err != nil {
			return err
		}
		//check if there are fields to be updated
		if tep.Status.LocalRemappedPodCIDR != param.localNatPodCIDR {
			tep.Status.LocalRemappedPodCIDR = param.localNatPodCIDR
			toBeUpdated = true
		}
		if tep.Status.RemoteRemappedPodCIDR != param.remoteNatPodCIDR {
			tep.Status.RemoteRemappedPodCIDR = param.remoteNatPodCIDR
			toBeUpdated = true
		}
		if tep.Status.Phase == "" {
			tep.Status.Phase = "Processed"
			toBeUpdated = true
		}
		if toBeUpdated {
			err = r.Status().Update(context.Background(), tep)
			return err
		}
		return nil
	})
	if retryError != nil {
		klog.Errorf("an error occurred while updating spec of tunnelEndpoint resource %s: %s", tep.Name, retryError)
		return retryError
	}
	return nil
}

func (r *TunnelEndpointCreator) CreateTunnelEndpoint(param networkParam) error {
	tepName := TunEndpointNamePrefix + param.clusterID
	//here we create it
	tep := &netv1alpha1.TunnelEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name: tepName,
		},
		Spec: netv1alpha1.TunnelEndpointSpec{
			ClusterID:      param.clusterID,
			PodCIDR:        param.podCIDR,
			TunnelPublicIP: param.gatewayIP,
		},
		Status: netv1alpha1.TunnelEndpointStatus{},
	}
	err := r.Create(context.Background(), tep)
	if err != nil {
		klog.Errorf("an error occurred while creating tunnelEndpoint resource %s: %s", tep.Name, err)
		return err
	}
	//first retry is to wait until the resource is created
	retryError := retry.OnError(retry.DefaultRetry, apierrors.IsNotFound, func() error {
		err := r.Get(context.Background(), client.ObjectKey{
			Name: tepName,
		}, tep)
		if err != nil {
			return err
		}
		return nil
	})
	if retryError != nil {
		klog.Errorf("an error occurred while processing tunnelEndpoint resource %s: %s", tep.Name, err)
		return retryError
	}
	//here we recover from conflicting resource versions
	retryErrorConflict := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		err := r.Get(context.Background(), client.ObjectKey{
			Name: tepName,
		}, tep)
		if err != nil {
			return err
		}
		tep.Status.RemoteRemappedPodCIDR = param.remoteNatPodCIDR
		tep.Status.LocalRemappedPodCIDR = param.localNatPodCIDR
		tep.Status.Phase = "Processed"
		err = r.Status().Update(context.Background(), tep)
		return err
	})
	if retryErrorConflict != nil {
		klog.Errorf("an error occurred while updating status of tunnelEndpoint resource %s: %s", tep.Name, err)
		return retryErrorConflict
	}
	return nil
}

func (r *TunnelEndpointCreator) AdvertisementHandler(obj *unstructured.Unstructured) {
	adv := &advtypes.Advertisement{}
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, adv)
	if err != nil {
		klog.Errorf("an error occurred while converting resource %s of type %s to typed object: %s", obj.GetName(), obj.GetKind(), err)
		return
	}
	_ = r.createNetConfig(adv.Spec.ClusterId)
}

func (r *TunnelEndpointCreator) PeeringRequestHandler(obj *unstructured.Unstructured) {
	peeringReq := &discoveryv1alpha1.PeeringRequest{}
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, peeringReq)
	if err != nil {
		klog.Errorf("an error occurred while converting resource %s of type %s to typed object: %s", obj.GetName(), obj.GetKind(), err)
		return
	}
	_ = r.createNetConfig(peeringReq.Spec.ClusterID)
}

func (r *TunnelEndpointCreator) GetTunnelEndpoint(name string) (*netv1alpha1.TunnelEndpoint, bool, error) {
	ctx := context.Background()
	tunEndpoint := &netv1alpha1.TunnelEndpoint{}
	//build the key used to retrieve the tunnelEndpoint CR
	tunEndKey := types.NamespacedName{
		Name: name,
	}
	//retrieve the tunnelEndpoint CR
	err := r.Get(ctx, tunEndKey, tunEndpoint)
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	} else {
		return tunEndpoint, true, nil
	}
}

func (r *TunnelEndpointCreator) deleteTunEndpoint(netConfig *advtypes.Advertisement) error {
	ctx := context.Background()
	var tunEndpoint netv1alpha1.TunnelEndpoint
	//build the key used to retrieve the tunnelEndpoint CR
	tunEndKey := types.NamespacedName{
		Namespace: netConfig.Namespace,
		Name:      netConfig.Spec.ClusterId + TunEndpointNamePrefix,
	}
	//retrieve the tunnelEndpoint CR
	err := r.Get(ctx, tunEndKey, &tunEndpoint)
	//if the CR exist then do nothing and return
	if err == nil {
		err := r.Delete(ctx, &tunEndpoint)
		if err != nil {
			return fmt.Errorf("unable to delete endpoint %s in namespace %s : %v", tunEndpoint.Name, tunEndpoint.Namespace, err)
		} else {
			return nil
		}
	} else if apierrors.IsNotFound(err) {
		return nil
	} else {
		return fmt.Errorf("unable to get endpoint with key %s: %v", tunEndKey.String(), err)
	}
}
