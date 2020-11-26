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
	networkingv1 "k8s.io/api/networking/v1"
	"net"
	"strings"
	"time"

	"github.com/netgroup-polito/CrownLabs/operators/pkg/instanceCreation"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	crownlabsv1alpha2 "github.com/netgroup-polito/CrownLabs/operators/api/v1alpha2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	virtv1 "kubevirt.io/client-go/api/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// LabInstanceReconciler reconciles a Instance object
type LabInstanceReconciler struct {
	client.Client
	Log                logr.Logger
	Scheme             *runtime.Scheme
	EventsRecorder     record.EventRecorder
	NamespaceWhitelist metav1.LabelSelector
	WebsiteBaseUrl     string
	NextcloudBaseUrl   string
	WebdavSecretName   string
	Oauth2ProxyImage   string
	OidcClientSecret   string
	OidcProviderUrl    string
}

// +kubebuilder:rbac:groups=crownlabs.polito.it,resources=labinstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=crownlabs.polito.it,resources=labinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events/status,verbs=get

func (r *LabInstanceReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	VMstart := time.Now()
	ctx := context.Background()
	log := r.Log.WithValues("labinstance", req.NamespacedName)

	// get labInstance
	var labInstance crownlabsv1alpha2.Instance
	if err := r.Get(ctx, req.NamespacedName, &labInstance); err != nil {
		// reconcile was triggered by a delete request
		log.Info("Instance " + req.Name + " deleted")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	ns := v1.Namespace{}
	namespaceName := types.NamespacedName{
		Name:      labInstance.Namespace,
		Namespace: "",
	}

	// It performs reconciliation only if the Instance belongs to whitelisted namespaces
	// by checking the existence of keys in labInstance namespace
	if err := r.Get(ctx, namespaceName, &ns); err == nil {
		if !instanceCreation.CheckLabels(ns, r.NamespaceWhitelist.MatchLabels) {
			log.Info("Namespace " + req.Namespace + " does not meet the selector labels")
			return ctrl.Result{}, nil
		}
	} else {
		log.Error(err, "unable to get Instance namespace")
	}
	log.Info("Namespace" + req.Namespace + " met the selector labels")
	// The metadata.generation value is incremented for all changes, except for changes to .metadata or .status
	// if metadata.generation is not incremented there's no need to reconcile
	if labInstance.Status.ObservedGeneration == labInstance.ObjectMeta.Generation {
		return ctrl.Result{}, nil
	}

	// check if labTemplate exists
	templateName := types.NamespacedName{
		Namespace: labInstance.Spec.Template.Namespace,
		Name:      labInstance.Spec.Template.Name,
	}
	var labTemplate crownlabsv1alpha2.Template
	if err := r.Get(ctx, templateName, &labTemplate); err != nil {
		// no Template related exists
		log.Info("Template " + templateName.Name + " doesn't exist. Deleting Instance " + labInstance.Name)
		r.EventsRecorder.Event(&labInstance, "Warning", "LabTemplateNotFound", "Template "+templateName.Name+" not found in namespace "+labTemplate.Namespace)
		_ = r.Delete(ctx, &labInstance, &client.DeleteOptions{})
		return ctrl.Result{}, err
	}

	r.EventsRecorder.Event(&labInstance, "Normal", "LabTemplateFound", "Template "+templateName.Name+" found in namespace "+labTemplate.Namespace)
	labInstance.Labels = map[string]string{
		"course-name":        strings.ReplaceAll(strings.ToLower(labTemplate.Spec.WorkspaceRef.Name), " ", "-"),
		"template-name":      labTemplate.Name,
		"template-namespace": labTemplate.Namespace,
	}
	labInstance.Status.ObservedGeneration = labInstance.ObjectMeta.Generation
	if err := r.Update(ctx, &labInstance); err != nil {
		log.Error(err, "unable to update Instance labels")
	}

	// prepare variables common to all resources
	name := fmt.Sprintf("%v-%.4s", strings.ReplaceAll(labInstance.Name, ".", "-"), uuid.New().String())
	namespace := labInstance.Namespace
	if err := r.CreateEnvironment(&labInstance, &labTemplate, namespace, name, VMstart); err != nil {
		log.Error(err, "unable to Create Laboratory Environment")
		return ctrl.Result{}, err
	}

	// create secret referenced by VirtualMachineInstance (Cloudinit)
	// To be extracted in a configuration flag
	VmElaborationTimestamp := time.Now()
	VMElaborationDuration := VmElaborationTimestamp.Sub(VMstart)
	elaborationTimes.Observe(VMElaborationDuration.Seconds())

	return ctrl.Result{}, nil
}

func (r *LabInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&crownlabsv1alpha2.Instance{}).
		Complete(r)
}

func setLabInstanceStatus(r *LabInstanceReconciler, ctx context.Context, log logr.Logger,
	msg string, eventType string, eventReason string,
	labInstance *crownlabsv1alpha2.Instance, ip, url string) {

	log.Info(msg)
	r.EventsRecorder.Event(labInstance, eventType, eventReason, msg)

	labInstance.Status.Phase = eventReason
	labInstance.Status.IP = ip
	labInstance.Status.Url = url
	labInstance.Status.ObservedGeneration = labInstance.ObjectMeta.Generation
	if err := r.Status().Update(ctx, labInstance); err != nil {
		log.Error(err, "unable to update Instance status")
	}
}

func getVmiStatus(r *LabInstanceReconciler, ctx context.Context, log logr.Logger,
	guiEnabled bool, service v1.Service, ingress networkingv1.Ingress,
	labInstance *crownlabsv1alpha2.Instance, vmi *virtv1.VirtualMachineInstance, startTimeVM time.Time) {

	var vmStatus virtv1.VirtualMachineInstancePhase

	var ip string
	url := ingress.GetAnnotations()["crownlabs.polito.it/probe-url"]

	// iterate until the vm is running
	for {
		err := r.Client.Get(ctx, types.NamespacedName{
			Namespace: vmi.Namespace,
			Name:      vmi.Name,
		}, vmi)
		if err == nil {
			if vmStatus != vmi.Status.Phase {
				vmStatus = vmi.Status.Phase
				if len(vmi.Status.Interfaces) > 0 {
					ip = vmi.Status.Interfaces[0].IP
				}

				msg := "VirtualMachineInstance " + vmi.Name + " in namespace " + vmi.Namespace + " status update to " + string(vmStatus)
				if vmStatus == virtv1.Failed {
					setLabInstanceStatus(r, ctx, log, msg, "Warning", "Vmi"+string(vmStatus), labInstance, "", "")
					return
				}

				setLabInstanceStatus(r, ctx, log, msg, "Normal", "Vmi"+string(vmStatus), labInstance, ip, url)
				if vmStatus == virtv1.Running {
					break
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	// when the vm status is Running, it is still not available for some seconds
	// hence, wait until it starts responding
	host := service.Name + "." + service.Namespace
	port := "6080" // VNC
	if !guiEnabled {
		port = "22" // SSH
	}

	err := waitForConnection(log, host, port)
	if err != nil {
		log.Error(err, fmt.Sprintf("Unable to check whether %v:%v is reachable", host, port))
	} else {
		msg := "VirtualMachineInstance " + vmi.Name + " in namespace " + vmi.Namespace + " status update to VmiReady."
		setLabInstanceStatus(r, ctx, log, msg, "Normal", "VmiReady", labInstance, ip, url)
		readyTime := time.Now()
		bootTime := readyTime.Sub(startTimeVM)
		bootTimes.Observe(bootTime.Seconds())
	}
}

func waitForConnection(log logr.Logger, host, port string) error {
	for retries := 0; retries < 120; retries++ {
		timeout := time.Second
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), timeout)
		if err != nil {
			log.Info(fmt.Sprintf("Unable to check whether %v:%v is reachable: %v", host, port, err))
			time.Sleep(time.Second)
		} else {
			// The connection succeeded, hence the VM is ready
			defer conn.Close()
			return nil
		}
	}

	return fmt.Errorf("Timeout while checking whether %v:%v is reachable", host, port)
}
