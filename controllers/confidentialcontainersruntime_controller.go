/*
Copyright 2021 CNCF.

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
	"github.com/confidential-containers-operator/api/v1beta1"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodeapi "k8s.io/api/node/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	confidentialcontainersorgv1beta1 "github.com/confidential-containers/confidential-containers-operator/api/v1beta1"
)

// ConfidentialContainersRuntimeReconciler reconciles a ConfidentialContainersRuntime object
type ConfidentialContainersRuntimeReconciler struct {
	client.Client
	Scheme                        *runtime.Scheme
	Log                           logr.Logger
	confidentialContainersRuntime *v1beta1.ConfidentialContainersRuntime
}

//+kubebuilder:rbac:groups=confidentialcontainers.org,resources=confidentialcontainersruntimes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=confidentialcontainers.org,resources=confidentialcontainersruntimes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=confidentialcontainers.org,resources=confidentialcontainersruntimes/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ConfidentialContainersRuntime object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.9.2/pkg/reconcile
func (r *ConfidentialContainersRuntimeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	_ = r.Log.WithValues("confidentialcontainersruntime", req.NamespacedName)
	r.Log.Info("Reconciling ConfidentialContainersRuntime in Kubernetes Cluster")

	// Fetch the ConfidentialContainersRuntime instance
	r.confidentialContainersRuntime = &v1beta1.ConfidentialContainersRuntime{}
	err := r.Client.Get(context.TODO(), req.NamespacedName, r.confidentialContainersRuntime)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	// Check if the ConfidentialContainersRuntime instance is marked to be deleted, which is
	// indicated by the deletion timestamp being set.
	if r.confidentialContainersRuntime.GetDeletionTimestamp() != nil {
		return r.processKataConfigDeleteRequest()
	}

	return r.processKataConfigInstallRequest()
}

func (r *ConfidentialContainersRuntimeReconciler) processKataConfigDeleteRequest() (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

func (r *ConfidentialContainersRuntimeReconciler) processKataConfigInstallRequest() (ctrl.Result, error) {
	if r.confidentialContainersRuntime.Status.TotalNodesCount == 0 {

		nodesList := &corev1.NodeList{}

		if r.confidentialContainersRuntime.Spec.ConfidentialContainersNodeSelector == nil {
			r.confidentialContainersRuntime.Spec.ConfidentialContainersNodeSelector = &metav1.LabelSelector{
				MatchLabels: map[string]string{"node-role.kubernetes.io/worker": ""},
			}
		}

		listOpts := []client.ListOption{
			client.MatchingLabels(r.confidentialContainersRuntime.Spec.ConfidentialContainersNodeSelector.MatchLabels),
		}

		err := r.Client.List(context.TODO(), nodesList, listOpts...)
		if err != nil {
			return ctrl.Result{}, err
		}
		r.confidentialContainersRuntime.Status.TotalNodesCount = len(nodesList.Items)

		if r.confidentialContainersRuntime.Status.TotalNodesCount == 0 {
			return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second},
				fmt.Errorf("no suitable worker nodes found for runtime installation. Please make sure to label the nodes with labels specified in ConfidentialContainersNodeSelector")
		}

		if r.confidentialContainersRuntime.Spec.Config.SourceImage == "" {
			return ctrl.Result{Requeue: true, RequeueAfter: 15 * time.Second},
				fmt.Errorf("SourceImage must be specified to download the runtime binaries")
		}

		if r.confidentialContainersRuntime.Status.ConfidentialContainersRuntimeImage == "" {
			// TODO - placeholder. This will change in future.
			r.confidentialContainersRuntime.Status.ConfidentialContainersRuntimeImage = r.confidentialContainersRuntime.Spec.Config.SourceImage
		}

		err = r.Client.Status().Update(context.TODO(), r.confidentialContainersRuntime)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	// Don't create the daemonset if the runtime is already installed on the cluster nodes
	if r.confidentialContainersRuntime.Status.TotalNodesCount > 0 &&
		r.confidentialContainersRuntime.Status.InstallationStatus.Completed.CompletedNodesCount != r.confidentialContainersRuntime.Status.TotalNodesCount {
		ds := r.processDaemonset(InstallOperation)
		// Set ConfidentialContainersRuntime instance as the owner and controller
		if err := controllerutil.SetControllerReference(r.confidentialContainersRuntime, ds, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		foundDs := &appsv1.DaemonSet{}
		err := r.Client.Get(context.TODO(), types.NamespacedName{Name: ds.Name, Namespace: ds.Namespace}, foundDs)
		if err != nil && errors.IsNotFound(err) {
			r.Log.Info("Creating a new installation Daemonset", "ds.Namespace", ds.Namespace, "ds.Name", ds.Name)
			err = r.Client.Create(context.TODO(), ds)
			if err != nil {
				return ctrl.Result{}, err
			}
		} else if err != nil {
			return ctrl.Result{}, err
		}

		return r.monitorKataConfigInstallation()
	}

	// Add finalizer for this CR
	// if !contains(r.confidentialContainersRuntime.GetFinalizers(), kataConfigFinalizer) {
	// 	if err := r.addFinalizer(); err != nil {
	// 		return ctrl.Result{}, err
	// 	}
	// }

	return ctrl.Result{}, nil
}

func (r *ConfidentialContainersRuntimeReconciler) monitorKataConfigInstallation() (ctrl.Result, error) {
	// If the installation of the binaries is successful on all nodes, proceed with creating the runtime classes
	if r.confidentialContainersRuntime.Status.TotalNodesCount > 0 && r.confidentialContainersRuntime.Status.InstallationStatus.InProgress.InProgressNodesCount == r.confidentialContainersRuntime.Status.TotalNodesCount {
		rs, err := r.setRuntimeClass()
		if err != nil {
			return rs, err
		}

		r.confidentialContainersRuntime.Status.InstallationStatus.Completed.CompletedNodesList = r.confidentialContainersRuntime.Status.InstallationStatus.InProgress.BinariesInstalledNodesList
		r.confidentialContainersRuntime.Status.InstallationStatus.Completed.CompletedNodesCount = len(r.confidentialContainersRuntime.Status.InstallationStatus.Completed.CompletedNodesList)
		r.confidentialContainersRuntime.Status.InstallationStatus.InProgress.BinariesInstalledNodesList = []string{}
		r.confidentialContainersRuntime.Status.InstallationStatus.InProgress.InProgressNodesCount = 0

		err = r.Client.Status().Update(context.TODO(), r.confidentialContainersRuntime)
		if err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	nodesList := &corev1.NodeList{}

	if r.confidentialContainersRuntime.Spec.ConfidentialContainersNodeSelector == nil {
		r.confidentialContainersRuntime.Spec.ConfidentialContainersNodeSelector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"node-role.kubernetes.io/worker": ""},
		}
	}

	listOpts := []client.ListOption{
		client.MatchingLabels(r.confidentialContainersRuntime.Spec.ConfidentialContainersNodeSelector.MatchLabels),
	}

	err := r.Client.List(context.TODO(), nodesList, listOpts...)
	if err != nil {
		return ctrl.Result{}, err
	}

	for _, node := range nodesList.Items {
		if !contains(r.confidentialContainersRuntime.Status.InstallationStatus.InProgress.BinariesInstalledNodesList, node.Name) {
			for k, v := range node.GetLabels() {
				if k == "confidentialcontainers.org/runtime" && v == "true" {
					r.confidentialContainersRuntime.Status.InstallationStatus.InProgress.BinariesInstalledNodesList = append(r.confidentialContainersRuntime.Status.InstallationStatus.InProgress.BinariesInstalledNodesList, node.Name)
					r.confidentialContainersRuntime.Status.InstallationStatus.InProgress.InProgressNodesCount++

					err = r.Client.Status().Update(context.TODO(), r.confidentialContainersRuntime)
					if err != nil {
						return ctrl.Result{}, err
					}
				}
			}
		}
		if r.confidentialContainersRuntime.Status.InstallationStatus.InProgress.InProgressNodesCount == r.confidentialContainersRuntime.Status.TotalNodesCount {
			return ctrl.Result{Requeue: true}, nil
		}
	}

	return ctrl.Result{}, nil
}

func (r *ConfidentialContainersRuntimeReconciler) setRuntimeClass() (ctrl.Result, error) {
	runtimeClassNames := []string{"kata-qemu-virtiofs", "kata-qemu", "kata-clh", "kata-fc", "kata"}

	for _, runtimeClassName := range runtimeClassNames {
		rc := func() *nodeapi.RuntimeClass {
			rc := &nodeapi.RuntimeClass{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "node.k8s.io/v1beta1",
					Kind:       "RuntimeClass",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name: runtimeClassName,
				},
				Handler: runtimeClassName,
			}

			if r.confidentialContainersRuntime.Spec.ConfidentialContainersNodeSelector != nil {
				rc.Scheduling = &nodeapi.Scheduling{
					NodeSelector: r.confidentialContainersRuntime.Spec.ConfidentialContainersNodeSelector.MatchLabels,
				}
			}
			return rc
		}()

		// Set ConfidentialContainersRuntime r.confidentialContainersRuntime as the owner and controller
		if err := controllerutil.SetControllerReference(r.confidentialContainersRuntime, rc, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}

		foundRc := &nodeapi.RuntimeClass{}
		err := r.Client.Get(context.TODO(), types.NamespacedName{Name: rc.Name}, foundRc)
		if err != nil && errors.IsNotFound(err) {
			r.Log.Info("Creating a new RuntimeClass", "rc.Name", rc.Name)
			err = r.Client.Create(context.TODO(), rc)
			if err != nil {
				return ctrl.Result{}, err
			}
		}

	}

	r.confidentialContainersRuntime.Status.RuntimeClass = strings.Join(runtimeClassNames, ",")
	err := r.Client.Status().Update(context.TODO(), r.confidentialContainersRuntime)
	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *ConfidentialContainersRuntimeReconciler) processDaemonset(operation DaemonOperation) *appsv1.DaemonSet {
	runPrivileged := true
	var runAsUser int64 = 0
	hostPt := corev1.HostPathType("DirectoryOrCreate")

	dsName := "kata-operator-daemon-" + string(operation)
	labels := map[string]string{
		"name": dsName,
	}

	var nodeSelector map[string]string
	if r.confidentialContainersRuntime.Spec.ConfidentialContainersNodeSelector != nil {
		nodeSelector = r.confidentialContainersRuntime.Spec.ConfidentialContainersNodeSelector.MatchLabels
	} else {
		nodeSelector = map[string]string{
			"node-role.kubernetes.io/worker": "",
		}
	}

	return &appsv1.DaemonSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "DaemonSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      dsName,
			Namespace: "kata-operator",
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{
				Type: "RollingUpdate",
				RollingUpdate: &appsv1.RollingUpdateDaemonSet{
					MaxUnavailable: &intstr.IntOrString{
						Type:   intstr.Int,
						IntVal: 1,
					},
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "kata-operator",
					NodeSelector:       nodeSelector,
					Containers: []corev1.Container{
						{
							Name:            "kata-install-pod",
							Image:           r.confidentialContainersRuntime.Status.ConfidentialContainersRuntimeImage,
							ImagePullPolicy: "Always",
							Lifecycle: &corev1.Lifecycle{
								PreStop: &corev1.Handler{
									Exec: &corev1.ExecAction{
										Command: []string{"bash", "-c", "/opt/kata-artifacts/scripts/kata-deploy.sh cleanup"},
									},
								},
							},
							SecurityContext: &corev1.SecurityContext{
								// TODO - do we really need to run as root?
								Privileged: &runPrivileged,
								RunAsUser:  &runAsUser,
							},
							Command: []string{"bash", "-c", "/opt/kata-artifacts/scripts/kata-deploy.sh install"},
							Env: []corev1.EnvVar{
								{
									Name: "NODE_NAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "spec.nodeName",
										},
									},
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "crio-conf",
									MountPath: "/etc/crio/",
								},
								{
									Name:      "containerd-conf",
									MountPath: "/etc/containerd/",
								},
								{
									Name:      "kata-artifacts",
									MountPath: "/opt/kata/",
								},
								{
									Name:      "dbus",
									MountPath: "/var/run/dbus",
								},
								{
									Name:      "systemd",
									MountPath: "/run/systemd",
								},
								{
									Name:      "local-bin",
									MountPath: "/usr/local/bin/",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "crio-conf",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/etc/crio/",
								},
							},
						},
						{
							Name: "containerd-conf",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/etc/containerd/",
								},
							},
						},
						{
							Name: "kata-artifacts",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/opt/kata/",
									Type: &hostPt,
								},
							},
						},
						{
							Name: "dbus",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/run/dbus",
								},
							},
						},
						{
							Name: "systemd",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/run/systemd",
								},
							},
						},
						{
							Name: "local-bin",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/usr/local/bin/",
								},
							},
						},
					},
				},
			},
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *ConfidentialContainersRuntimeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&confidentialcontainersorgv1beta1.ConfidentialContainersRuntime{}).
		Complete(r)
}