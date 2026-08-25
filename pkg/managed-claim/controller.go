/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 *
 */

package managedclaim

import (
	"context"
	"time"

	k8sv1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	v1 "kubevirt.io/api/core/v1"
	corev1alpha1 "kubevirt.io/api/core/v1alpha1"
	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/controller"
)

// FailedProvisioningReason is the event reason recorded on a VMI when its
// managed ResourceClaims cannot be generated. VMI status has a single writer
// (virt-controller), so an event is the only signal this controller may emit
// about a VMI.
const FailedProvisioningReason = "FailedManagedClaimProvisioning"

// Controller drives Reconciler off informer events for a single provisioner.
//
// It watches VMIs (the source of managed claim entries), the ManagedClaims it
// owns (so out-of-band edits or deletes are corrected), and the
// ManagedClaimProvisioners that configure generation, funnelling every change
// to the owning VMI's key.
type Controller struct {
	reconciler *Reconciler
	recorder   record.EventRecorder

	vmiInformer         cache.SharedIndexInformer
	provisionerInformer cache.SharedIndexInformer
	claimInformer       cache.SharedIndexInformer

	// Queue is exported so tests can wrap it with a synchronous mock.
	Queue     workqueue.TypedRateLimitingInterface[string]
	hasSynced func() bool
}

func NewController(
	reconciler *Reconciler,
	recorder record.EventRecorder,
	vmiInformer cache.SharedIndexInformer,
	provisionerInformer cache.SharedIndexInformer,
	claimInformer cache.SharedIndexInformer,
) (*Controller, error) {
	c := &Controller{
		reconciler:          reconciler,
		recorder:            recorder,
		vmiInformer:         vmiInformer,
		provisionerInformer: provisionerInformer,
		claimInformer:       claimInformer,
		Queue: workqueue.NewTypedRateLimitingQueueWithConfig[string](
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "managed-claim"},
		),
	}

	c.hasSynced = func() bool {
		return vmiInformer.HasSynced() &&
			provisionerInformer.HasSynced() &&
			claimInformer.HasSynced()
	}

	if _, err := vmiInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.enqueueVMI(obj) },
		DeleteFunc: func(obj interface{}) { c.enqueueVMI(obj) },
		UpdateFunc: func(_, cur interface{}) { c.enqueueVMI(cur) },
	}); err != nil {
		return nil, err
	}

	if _, err := claimInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.enqueueResourceClaim(obj) },
		DeleteFunc: func(obj interface{}) { c.enqueueResourceClaim(obj) },
		UpdateFunc: func(_, cur interface{}) { c.enqueueResourceClaim(cur) },
	}); err != nil {
		return nil, err
	}

	if _, err := provisionerInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.enqueueProvisioner(obj) },
		DeleteFunc: func(obj interface{}) { c.enqueueProvisioner(obj) },
		UpdateFunc: func(_, cur interface{}) { c.enqueueProvisioner(cur) },
	}); err != nil {
		return nil, err
	}

	return c, nil
}

func (c *Controller) enqueueVMI(obj interface{}) {
	vmi, ok := obj.(*v1.VirtualMachineInstance)
	if !ok {
		return
	}
	key, err := controller.KeyFunc(vmi)
	if err != nil {
		log.Log.Object(vmi).Reason(err).Error("failed to extract key from VMI")
		return
	}
	c.Queue.Add(key)
}

// enqueueResourceClaim routes an owned ResourceClaim back to its VMI. The claim
// carries a controller owner reference to the VMI it was generated for.
func (c *Controller) enqueueResourceClaim(obj interface{}) {
	claim, ok := obj.(*resourcev1.ResourceClaim)
	if !ok {
		return
	}
	owner := metav1.GetControllerOf(&claim.ObjectMeta)
	if owner == nil || owner.Kind != v1.VirtualMachineInstanceGroupVersionKind.Kind {
		return
	}
	c.Queue.Add(controller.NamespacedKey(claim.Namespace, owner.Name))
}

// enqueueProvisioner fans a provisioner change out to every VMI that references
// it, since a change to its DeviceTypes changes the claims those VMIs need.
func (c *Controller) enqueueProvisioner(obj interface{}) {
	provisioner, ok := obj.(*corev1alpha1.ManagedClaimProvisioner)
	if !ok {
		return
	}
	for _, item := range c.vmiInformer.GetStore().List() {
		vmi, ok := item.(*v1.VirtualMachineInstance)
		if !ok {
			continue
		}
		if vmiReferencesProvisioner(vmi, provisioner.Name) {
			c.enqueueVMI(vmi)
		}
	}
}

func vmiReferencesProvisioner(vmi *v1.VirtualMachineInstance, name string) bool {
	for i := range vmi.Spec.ResourceClaims {
		ref := vmi.Spec.ResourceClaims[i].ManagedClaimProvisionerName
		if ref != nil && *ref == name {
			return true
		}
	}
	return false
}

func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) {
	defer controller.HandlePanic()
	defer c.Queue.ShutDown()

	log.Log.Info("starting managed claim controller.")

	cache.WaitForCacheSync(stopCh, c.hasSynced)

	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	log.Log.Info("stopping managed claim controller.")
}

func (c *Controller) runWorker() {
	for c.Execute() {
	}
}

func (c *Controller) Execute() bool {
	key, quit := c.Queue.Get()
	if quit {
		return false
	}
	defer c.Queue.Done(key)

	if err := c.execute(key); err != nil {
		log.Log.Reason(err).Infof("reenqueuing VMI %v", key)
		c.Queue.AddRateLimited(key)
	} else {
		c.Queue.Forget(key)
	}
	return true
}

func (c *Controller) execute(key string) error {
	obj, exists, err := c.vmiInformer.GetStore().GetByKey(key)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	vmi := obj.(*v1.VirtualMachineInstance)

	if err := c.reconciler.Reconcile(context.Background(), vmi); err != nil {
		c.recorder.Eventf(vmi, k8sv1.EventTypeWarning, FailedProvisioningReason,
			"failed to provision managed resource claims: %v", err)
		return err
	}
	return nil
}
