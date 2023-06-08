/*
Copyright 2021.

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
	"encoding/json"
	"fmt"
	"net"
	"os"
	"reflect"
	"strings"

	"github.com/k8snetworkplumbingwg/sriov-network-operator/pkg/apply"
	"github.com/openshift/cluster-network-operator/pkg/render"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	mcrender "github.com/k8snetworkplumbingwg/sriov-network-operator/pkg/render"
	mcfgv1 "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"

	"github.com/openshift/dpu-network-operator/api"
	dpuv1alpha1 "github.com/openshift/dpu-network-operator/api/v1alpha1"
	syncer "github.com/openshift/dpu-network-operator/pkg/ovnkube-syncer"
	"github.com/openshift/dpu-network-operator/pkg/utils"
)

const (
	dpuMcRole = "dpu-worker"
)

var logger = log.Log.WithName("controller_dpuclusterconfig")

const (
	OVN_NB_PORT = "9641"
	OVN_SB_PORT = "9642"
)

// DpuClusterConfigReconciler reconciles a DpuClusterConfig object
type DpuClusterConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	syncer *syncer.OvnkubeSyncer
}

//+kubebuilder:rbac:groups=dpu.openshift.io,resources=dpuclusterconfigs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=dpu.openshift.io,resources=dpuclusterconfigs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=dpu.openshift.io,resources=dpuclusterconfigs/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=machineconfiguration.openshift.io,resources=machineconfigpools,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=machineconfiguration.openshift.io,resources=machineconfigs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,resourceNames=anyuid;hostnetwork,verbs=use

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the DpuClusterConfig object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.9.2/pkg/reconcile
func (r *DpuClusterConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var err error
	logger := log.FromContext(ctx).WithValues("reconcile DpuClusterConfig", req.NamespacedName)
	logger.Info("Reconcile")

	cfgList := &dpuv1alpha1.DpuClusterConfigList{}
	err = r.List(ctx, cfgList, &client.ListOptions{Namespace: req.Namespace})
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(cfgList.Items) > 1 {
		logger.Error(fmt.Errorf("more than one DpuClusterConfig CR is found in"), "namespace", req.Namespace)
		return ctrl.Result{}, err
	} else if len(cfgList.Items) == 1 {
		return r.ReconcileDpuClusterConfig(ctx, req, &cfgList.Items[0])
	} else if len(cfgList.Items) == 0 {
		if r.syncer != nil {
			r.syncer.Stop()
			r.syncer = nil
		}
	}

	return ctrl.Result{}, nil
}

func (r *DpuClusterConfigReconciler) ReconcileDpuClusterConfig(ctx context.Context, req ctrl.Request, dpuClusterConfig *dpuv1alpha1.DpuClusterConfig) (ctrl.Result, error) {
	defer func() {
		if err := r.Status().Update(context.TODO(), dpuClusterConfig); err != nil {
			logger.Error(err, "unable to update OVNKubeConfig status")
		}
	}()

	if err := r.validateDPUHostBootstrap(dpuClusterConfig); err != nil {
		logger.Error(err, "Error while validating OVNKubeConfig")
		return ctrl.Result{}, err
	}
	if err := r.ensureMcpReady(dpuClusterConfig); err != nil {
		logger.Error(err, "Failed to get Mcp into ready state ")
		return ctrl.Result{}, err
	}
	if err := r.validateTenantKubeConfig(dpuClusterConfig); err != nil {
		logger.Error(err, "kubeconfig of tenant cluster is not provided")
		return ctrl.Result{}, err
	}
	if err := r.ensureTenantObjsSynced(ctx, req, dpuClusterConfig); err != nil {
		logger.Error(err, "Failed to sync tenant objects")
		return ctrl.Result{}, err
	}
	if err := r.ensureDeamonSetRunning(ctx, req, dpuClusterConfig); err != nil {
		logger.Error(err, "Failed to ensure that ovn kube deamonset is running")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *DpuClusterConfigReconciler) ensureMcpReady(dpuClusterConfig *dpuv1alpha1.DpuClusterConfig) error {
	if err := r.syncMachineConfigObjs(dpuClusterConfig.Spec); err != nil {
		dpuClusterConfig.SetStatus(*api.Conditions().NotMcpReady().Reason(api.ReasonFailedCreated).Msg(err.Error()).Build())
		return err
	}
	dpuClusterConfig.SetStatus(*api.Conditions().McpReady().Reason(api.ReasonCreated).Build())
	return nil
}

func (r *DpuClusterConfigReconciler) ensureTenantObjsSynced(ctx context.Context, req ctrl.Request, dpuClusterConfig *dpuv1alpha1.DpuClusterConfig) error {
	if err := r.startTenantSyncerIfNeeded(ctx, dpuClusterConfig); err != nil {
		dpuClusterConfig.SetStatus(*api.Conditions().NotTenantObjsSynced().Reason(api.ReasonFailedStart).Msg(err.Error()).Build())
		return err
	}
	if err := r.isTenantObjsSynced(ctx, req.Namespace); err != nil {
		dpuClusterConfig.SetStatus(*api.Conditions().NotTenantObjsSynced().Reason(api.ReasonNotFound).Msg(err.Error()).Build())
		return err
	}
	dpuClusterConfig.SetStatus(*api.Conditions().TenantObjsSynced().Reason(api.ReasonCreated).Build())
	return nil
}

func (r *DpuClusterConfigReconciler) ensureDeamonSetRunning(ctx context.Context, req ctrl.Request, dpuClusterConfig *dpuv1alpha1.DpuClusterConfig) error {
	if err := r.syncOvnkubeDaemonSet(ctx, dpuClusterConfig); err != nil {
		dpuClusterConfig.SetStatus(*api.Conditions().NotOvnKubeReady().Reason(api.ReasonFailedCreated).Msg(err.Error()).Build())
		return err
	}
    if err := r.checkDeamonSetState(ctx, req, dpuClusterConfig); err != nil {
		dpuClusterConfig.SetStatus(*api.Conditions().NotOvnKubeReady().Reason(api.ReasonProgressing).Msg(err.Error()).Build())
		return err
	}
	dpuClusterConfig.SetStatus(*api.Conditions().OvnKubeReady().Reason(api.ReasonCreated).Build())
    return nil
}

func (r *DpuClusterConfigReconciler) checkDeamonSetState(ctx context.Context, req ctrl.Request, dpuClusterConfig *dpuv1alpha1.DpuClusterConfig) error {
	ds := appsv1.DaemonSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: "ovnkube-node"}, &ds); err != nil {
		return err
	}
	if ds.Status.DesiredNumberScheduled != ds.Status.NumberReady {
		return fmt.Errorf("DaemonSet 'ovnkube-node' is rolling out")
	}
	return nil
}

func (r *DpuClusterConfigReconciler) validateTenantKubeConfig(dpuClusterConfig *dpuv1alpha1.DpuClusterConfig) error {
	if dpuClusterConfig.Spec.KubeConfigFile == "" {
		return fmt.Errorf("No Kubeconfig provided for Tenant cluster")
	}
	return nil
}

func (r *DpuClusterConfigReconciler) validateDPUHostBootstrap(dpuClusterConfig *dpuv1alpha1.DpuClusterConfig) error {
	if dpuClusterConfig.Spec.PoolName == "" {
		return fmt.Errorf("PoolName not provided")
	}
	if dpuClusterConfig.Spec.NodeSelector.String() == "" {
		return fmt.Errorf("Missing node selector")
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DpuClusterConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dpuv1alpha1.DpuClusterConfig{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&appsv1.DaemonSet{}).
		Complete(r)
}

func (r *DpuClusterConfigReconciler) startTenantSyncerIfNeeded(ctx context.Context, cfg *dpuv1alpha1.DpuClusterConfig) error {
	if r.syncer != nil {
		return nil
	}

	logger.Info("Starting the tenant syncer")
	var err error
	s := &corev1.Secret{}

	err = r.Client.Get(ctx, types.NamespacedName{Name: cfg.Spec.KubeConfigFile, Namespace: cfg.Namespace}, s)
	if err != nil {
		return err
	}
	bytes, ok := s.Data["config"]
	if !ok {
		return fmt.Errorf("key 'config' cannot be found in secret %s", cfg.Spec.KubeConfigFile)
	}

	utils.TenantRestConfig, err = clientcmd.RESTConfigFromKubeConfig(bytes)
	if err != nil {
		return err
	}

	r.syncer, err = syncer.New(syncer.SyncerConfig{
		// LocalClusterID:   cfg.Namespace,
		LocalRestConfig:  ctrl.GetConfigOrDie(),
		LocalNamespace:   cfg.Namespace,
		TenantRestConfig: utils.TenantRestConfig,
		TenantNamespace:  utils.TenantNamespace}, cfg, r.Scheme)
	if err != nil {
		return err
	}
	go func() {
		if err = r.syncer.Start(); err != nil {
			logger.Error(err, "Error running the ovnkube syncer")
		}
	}()
	if err != nil {
		return err
	}

	return nil
}

func (r *DpuClusterConfigReconciler) syncOvnkubeDaemonSet(ctx context.Context, cfg *dpuv1alpha1.DpuClusterConfig) error {
	logger.Info("Start to sync ovnkube daemonset")
	var err error
	mcp := &mcfgv1.MachineConfigPool{}
	err = r.Get(context.TODO(), types.NamespacedName{Name: cfg.Spec.PoolName}, mcp)
	if err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("MachineConfigPool %s not found: %v", cfg.Spec.PoolName, err)
		}
	}

	masterIPs, err := r.getTenantClusterMasterIPs(ctx)
	if err != nil {
		logger.Error(err, "failed to get the ovnkube master IPs")
		return nil
	}

	image := os.Getenv("OVNKUBE_IMAGE")
	if image == "" {
		image, err = r.getLocalOvnkubeImage()
		if err != nil {
			return err
		}
	}

	data := render.MakeRenderData()
	data.Data["OvnKubeImage"] = image
	data.Data["Namespace"] = cfg.Namespace
	data.Data["TenantKubeconfig"] = cfg.Spec.KubeConfigFile
	data.Data["OVN_NB_DB_LIST"] = dbList(masterIPs, OVN_NB_PORT)
	data.Data["OVN_SB_DB_LIST"] = dbList(masterIPs, OVN_SB_PORT)

	objs, err := render.RenderDir(utils.OvnkubeNodeManifestPath, &data)
	if err != nil {
		logger.Error(err, "Fail to render ovnkube-node daemon manifests")
		return err
	}
	// Sync DaemonSets
	for _, obj := range objs {
		switch obj.GetKind() {
		case "DaemonSet":
			scheme := scheme.Scheme
			ds := &appsv1.DaemonSet{}
			err = scheme.Convert(obj, ds, nil)
			if err != nil {
				logger.Error(err, "Fail to convert to DaemonSet")
				return err
			}
			for k, v := range mcp.Spec.NodeSelector.MatchLabels {
				ds.Spec.Template.Spec.NodeSelector[k] = v
			}
			err = scheme.Convert(ds, obj, nil)
			if err != nil {
				logger.Error(err, "Fail to convert to Unstructured")
				return err
			}
			if err := ctrl.SetControllerReference(cfg, obj, r.Scheme); err != nil {
				return err
			}
		default:
			if err := ctrl.SetControllerReference(cfg, obj, r.Scheme); err != nil {
				return err
			}
		}
		if err := apply.ApplyObject(context.TODO(), r.Client, obj); err != nil {
			return fmt.Errorf("failed to apply object %v with err: %v", obj, err)
		}
	}
	return nil
}

func (r *DpuClusterConfigReconciler) getLocalOvnkubeImage() (string, error) {
	ds := &appsv1.DaemonSet{}
	name := types.NamespacedName{Namespace: utils.LocalOvnkbueNamespace, Name: utils.LocalOvnkbueNodeDsName}
	err := r.Get(context.TODO(), name, ds)
	if err != nil {
		return "", err
	}
	return ds.Spec.Template.Spec.Containers[0].Image, nil
}

func (r *DpuClusterConfigReconciler) syncMachineConfigObjs(cs dpuv1alpha1.DpuClusterConfigSpec) error {
	var err error
	foundMc := &mcfgv1.MachineConfig{}
	foundMcp := &mcfgv1.MachineConfigPool{}
	mcp := &mcfgv1.MachineConfigPool{}
	mcp.Name = cs.PoolName
	mcSelector, err := metav1.ParseToLabelSelector(fmt.Sprintf("%s in (worker,%s)", mcfgv1.MachineConfigRoleLabelKey, dpuMcRole))
	if err != nil {
		return err
	}
	mcp.Spec = mcfgv1.MachineConfigPoolSpec{
		MachineConfigSelector: mcSelector,
		NodeSelector:          cs.NodeSelector,
	}
	if cs.PoolName == "master" || cs.PoolName == "worker" {
		return fmt.Errorf("%s pools is not allowed", cs.PoolName)
	}

	err = r.Get(context.TODO(), types.NamespacedName{Name: cs.PoolName}, foundMcp)
	if err != nil {
		if errors.IsNotFound(err) {

			err = r.Create(context.TODO(), mcp)
			if err != nil {
				return fmt.Errorf("couldn't create MachineConfigPool: %v", err)
			}
			logger.Info("Created MachineConfigPool:", "name", cs.PoolName)
		}
	} else {
		if !(equality.Semantic.DeepEqual(foundMcp.Spec.MachineConfigSelector, mcSelector) && equality.Semantic.DeepEqual(foundMcp.Spec.NodeSelector, cs.NodeSelector)) {
			logger.Info("MachineConfigPool already exists, updating")
			foundMcp.Spec = mcp.Spec
			err = r.Update(context.TODO(), foundMcp)
			if err != nil {
				return fmt.Errorf("couldn't update MachineConfigPool: %v", err)
			}
		} else {
			logger.Info("No content change, skip updating MCP")
		}
	}

	mcName := "00-" + cs.PoolName + "-" + "bluefield-switchdev"

	data := mcrender.MakeRenderData()
	mc, err := mcrender.GenerateMachineConfig("bindata/machine-config", mcName, dpuMcRole, true, &data)
	if err != nil {
		return err
	}

	err = r.Get(context.TODO(), types.NamespacedName{Name: mcName}, foundMc)
	if err != nil {
		if errors.IsNotFound(err) {
			err = r.Create(context.TODO(), mc)
			if err != nil {
				return fmt.Errorf("couldn't create MachineConfig: %v", err)
			}
			logger.Info("Created MachineConfig CR in MachineConfigPool", mcName, cs.PoolName)
		} else {
			return fmt.Errorf("failed to get MachineConfig: %v", err)
		}
	} else {
		var foundIgn, renderedIgn interface{}
		// The Raw config JSON string may have the fields reordered.
		// For example the "path" field may come before the "contents"
		// field in the rendered ignition JSON; while the found
		// MachineConfig's ignition JSON would have it the other way around.
		// Thus we need to unmarshal the JSON for both found and rendered
		// ignition and compare.
		json.Unmarshal(foundMc.Spec.Config.Raw, &foundIgn)
		json.Unmarshal(mc.Spec.Config.Raw, &renderedIgn)
		if !reflect.DeepEqual(foundIgn, renderedIgn) {
			logger.Info("MachineConfig already exists, updating")
			foundMc.Spec.Config.Raw = mc.Spec.Config.Raw
			mc.SetResourceVersion(foundMc.GetResourceVersion())
			err = r.Update(context.TODO(), mc)
			if err != nil {
				return fmt.Errorf("couldn't update MachineConfig: %v", err)
			}
		} else {
			logger.Info("No content change, skip updating MachineConfig")
		}
	}
	return nil
}

func (r *DpuClusterConfigReconciler) getTenantClusterMasterIPs(ctx context.Context) ([]string, error) {
	c, err := client.New(utils.TenantRestConfig, client.Options{})
	if err != nil {
		logger.Error(err, "Fail to create client for the tenant cluster")
		return []string{}, err
	}
	ovnkubeMasterPods := corev1.PodList{}
	labelSelector := labels.SelectorFromSet(map[string]string{"app": "ovnkube-master"})
	listOps := &client.ListOptions{LabelSelector: labelSelector}
	err = c.List(ctx, &ovnkubeMasterPods, listOps)
	if err != nil {
		logger.Error(err, "Fail to get the ovnkube-master pods of the tenant cluster")
		return []string{}, err
	}
	masterIPs := []string{}
	for _, pod := range ovnkubeMasterPods.Items {
		masterIPs = append(masterIPs, pod.Status.PodIP)
	}
	return masterIPs, nil
}

func (r *DpuClusterConfigReconciler) isTenantObjsSynced(ctx context.Context, namespace string) error {
	cm := corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: utils.CmNameOvnCa}, &cm); err != nil {
		return err
	}

	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: utils.CmNameOvnkubeConfig}, &cm); err != nil {
		return err
	}

	s := corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: utils.SecretNameOvnCert}, &s); err != nil {
		return err
	}

	return nil
}

func dbList(masterIPs []string, port string) string {
	addrs := make([]string, len(masterIPs))
	for i, ip := range masterIPs {
		addrs[i] = "ssl:" + net.JoinHostPort(ip, port)
	}
	return strings.Join(addrs, ",")
}
