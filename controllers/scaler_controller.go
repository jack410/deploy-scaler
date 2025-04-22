/*
Copyright 2024.

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
	v1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"time"

	apiv1alpha1 "github.com/jack410/deploy-scaler/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const finalizer = "scalers.api.scaler.com/finalizer"

var logger = log.Log.WithName("scaler_controller")

var originalDeploymentInfo = make(map[string]apiv1alpha1.DeploymentInfo)
var annotations = make(map[string]string)

// ScalerReconciler reconciles a Scaler object
type ScalerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=api.scaler.com,resources=scalers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=api.scaler.com,resources=scalers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=api.scaler.com,resources=scalers/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Scaler object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.12.1/pkg/reconcile
func (r *ScalerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logger.WithValues("Request.Namespace", req.Namespace, "Request.Name", req.Name)
	log.Info("Reconcile called")

	fmt.Printf("reconcile: %+v\n", req)

	//创建一个scaler实例
	scaler := &apiv1alpha1.Scaler{}
	err := r.Get(ctx, req.NamespacedName, scaler)
	if err != nil {
		//如果没有发现这个scaler实例，我们就用 client.IgnoreNotFound(err)来忽略错误，让进程不中断
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if scaler.ObjectMeta.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(scaler, finalizer) {
			controllerutil.AddFinalizer(scaler, finalizer)
			log.Info("add finalizer.")
			err := r.Update(ctx, scaler)
			if err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}
		if scaler.Status.Status == "" {
			scaler.Status.Status = apiv1alpha1.PENDING
			err := r.Status().Update(ctx, scaler)
			if err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}

			//将scaler中管理的deployments的副本数和namespace名称加到annotations里
			if err := addAnnotations(scaler, r, ctx); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}

		//开始执行scaler的逻辑
		startTime := scaler.Spec.Start
		endTime := scaler.Spec.End
		replicas := scaler.Spec.Replicas

		currenHour := time.Now().Local().Hour()
		log.Info(fmt.Sprintf("currentTIme: %d", currenHour))

		//从startime开始endtime为止
		if currenHour >= startTime && currenHour < endTime {
			if scaler.Status.Status != apiv1alpha1.SCALED {
				log.Info("starting to call scaleDeployment func.")
				err := scaleDeployment(scaler, r, ctx, replicas)
				if err != nil {
					return ctrl.Result{}, err
				}
			}
		} else {
			if scaler.Status.Status == apiv1alpha1.SCALED {
				restoreDeployment(scaler, r, ctx)
			}
		}
	} else {
		log.Info("starting deletion flow.")
		if scaler.Status.Status == apiv1alpha1.SCALED {
			log.Info("restore Deployment.")
			err := restoreDeployment(scaler, r, ctx)
			if err != nil {
				return ctrl.Result{}, err
			}
			log.Info("remove finalizer.")
			controllerutil.RemoveFinalizer(scaler, finalizer)
			err = r.Update(ctx, scaler)
			if err != nil {
				return ctrl.Result{}, err
			}
			log.Info("finalizer removed.")
		} else {
			controllerutil.RemoveFinalizer(scaler, finalizer)
			err = r.Update(ctx, scaler)
			if err != nil {
				return ctrl.Result{}, err
			}
			log.Info("finalizer removed.")
		}
		log.Info("scaler removed.")
	}

	return ctrl.Result{RequeueAfter: time.Duration(10 * time.Second)}, nil
}

func restoreDeployment(scaler *apiv1alpha1.Scaler, r *ScalerReconciler, ctx context.Context) error {
	logger.Info("restoring deployments to original replicas")

	for _, deploy := range scaler.Spec.Deployments {
		// 从 Annotations 获取原始副本信息
		infoJSON, exists := scaler.Annotations[deploy.Name]
		if !exists {
			continue
		}

		var originalInfo apiv1alpha1.DeploymentInfo
		if err := json.Unmarshal([]byte(infoJSON), &originalInfo); err != nil {
			return err
		}

		deployment := &v1.Deployment{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      deploy.Name,
			Namespace: originalInfo.Namespace,
		}, deployment); err != nil {
			return err
		}

		if *deployment.Spec.Replicas != originalInfo.Replicas {
			deployment.Spec.Replicas = &originalInfo.Replicas
			if err := r.Update(ctx, deployment); err != nil {
				return err
			}
		}
	}

	scaler.Status.Status = apiv1alpha1.RESTORED
	return r.Status().Update(ctx, scaler)
}

func scaleDeployment(scaler *apiv1alpha1.Scaler, r *ScalerReconciler, ctx context.Context, replicas int32) error {
	//从scaler实例中遍历deployments
	for _, deploy := range scaler.Spec.Deployments {
		//创建一个新的deployment实例
		deployment := &v1.Deployment{}
		err := r.Get(ctx, types.NamespacedName{
			Name:      deploy.Name,
			Namespace: deploy.Namespace,
		}, deployment)
		if err != nil {
			return err
		}

		//判断当前k8s集群中的deployment副本数是否等于scaler中指定的副本数
		if deployment.Spec.Replicas != &replicas {
			deployment.Spec.Replicas = &replicas
			err := r.Update(ctx, deployment)
			if err != nil {
				scaler.Status.Status = apiv1alpha1.FAILED
				r.Status().Update(ctx, scaler)
				return err
			}

			scaler.Status.Status = apiv1alpha1.SCALED
			err = r.Status().Update(ctx, scaler)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// 存储原始副本数到 Annotations
func addAnnotations(scaler *apiv1alpha1.Scaler, r *ScalerReconciler, ctx context.Context) error {
	if scaler.Annotations == nil {
		scaler.Annotations = make(map[string]string)
	}

	for _, deploy := range scaler.Spec.Deployments {
		deployment := &v1.Deployment{}
		err := r.Get(ctx, types.NamespacedName{
			Namespace: deploy.Namespace,
			Name:      deploy.Name,
		}, deployment)
		if err != nil {
			return err
		}

		// 记录原始副本数
		originalInfo := apiv1alpha1.DeploymentInfo{
			Namespace: deployment.Namespace,
			Replicas:  *deployment.Spec.Replicas,
		}

		infoJSON, err := json.Marshal(originalInfo)
		if err != nil {
			return err
		}

		scaler.Annotations[deployment.Name] = string(infoJSON)
	}

	return r.Update(ctx, scaler)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ScalerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1alpha1.Scaler{}).
		Complete(r)
}
