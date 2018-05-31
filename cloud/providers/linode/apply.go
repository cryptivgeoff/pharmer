package linode

import (
	"fmt"
	"strings"

	api "github.com/pharmer/pharmer/apis/v1"
	. "github.com/pharmer/pharmer/cloud"
	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	clustercommon "sigs.k8s.io/cluster-api/pkg/apis/cluster/common"
	clusterv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
	"sigs.k8s.io/cluster-api/pkg/client/clientset_generated/clientset"
)

func (cm *ClusterManager) Apply(in *api.Cluster, dryRun bool) ([]api.Action, error) {
	var err error
	var acts []api.Action

	if in.Status.Phase == "" {
		return nil, errors.Errorf("cluster `%s` is in unknown phase", cm.cluster.Name)
	}
	if in.Status.Phase == api.ClusterDeleted {
		return nil, nil
	}
	cm.cluster = in
	cm.namer = namer{cluster: cm.cluster}
	if err = cm.PrepareCloud(in.Name); err != nil {
		return nil, err
	}

	if err = cm.InitializeActuator(nil); err != nil {
		return nil, err
	}

	clusterConf := cm.cluster.ProviderConfig()

	if clusterConf.InstanceImage, err = cm.conn.DetectInstanceImage(); err != nil {
		return nil, err
	}

	Logger(cm.ctx).Debugln("Linode instance image", clusterConf.InstanceImage)
	if clusterConf.Linode.KernelId, err = cm.conn.DetectKernel(); err != nil {
		return nil, err
	}
	Logger(cm.ctx).Infof("Linode kernel %v found", clusterConf.Linode.KernelId)

	if err = cm.cluster.SetProviderConfig(clusterConf); err != nil {
		return nil, err
	}
	Store(cm.ctx).Clusters().Update(cm.cluster)
	if cm.cluster.Status.Phase == api.ClusterUpgrading {
		return nil, errors.Errorf("cluster `%s` is upgrading. Retry after cluster returns to Ready state", cm.cluster.Name)
	}
	if cm.cluster.Status.Phase == api.ClusterReady {
		var kc kubernetes.Interface
		kc, err = cm.GetAdminClient()
		if err != nil {
			return nil, err
		}
		if upgrade, err := NewKubeVersionGetter(kc, cm.cluster).IsUpgradeRequested(); err != nil {
			return nil, err
		} else if upgrade {
			cm.cluster.Status.Phase = api.ClusterUpgrading
			Store(cm.ctx).Clusters().UpdateStatus(cm.cluster)
			return cm.applyUpgrade(dryRun)
		}
	}

	if cm.cluster.Status.Phase == api.ClusterPending {
		a, err := cm.applyCreate(dryRun)
		if err != nil {
			return nil, err
		}
		acts = append(acts, a...)
	}

	if cm.cluster.DeletionTimestamp != nil && cm.cluster.Status.Phase != api.ClusterDeleted {
		nodeGroups, err := Store(cm.ctx).NodeGroups(cm.cluster.Name).List(metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for _, ng := range nodeGroups {
			ng.Spec.Nodes = 0
			_, err := Store(cm.ctx).NodeGroups(cm.cluster.Name).Update(ng)
			if err != nil {
				return nil, err
			}
		}
	}

	{
		a, err := cm.applyScale(dryRun)
		if err != nil {
			return nil, err
		}
		acts = append(acts, a...)
	}

	if cm.cluster.DeletionTimestamp != nil && cm.cluster.Status.Phase != api.ClusterDeleted {
		a, err := cm.applyDelete(dryRun)
		if err != nil {
			return nil, err
		}
		acts = append(acts, a...)
	}
	return acts, nil
}

// Creates network, and creates ready master(s)
func (cm *ClusterManager) applyCreate(dryRun bool) (acts []api.Action, err error) {
	// FYI: Linode does not support tagging.

	// -------------------------------------------------------------------ASSETS
	var masterNG []*clusterv1.Machine
	masterNG, err = FindMasterMachines(cm.cluster)
	if err != nil {
		return
	}
	var lbIp string
	haSetup := IsHASetup(cm.cluster)
	if haSetup {
		Logger(cm.ctx).Info("Creating loadbalancer")
		lbIp, err = cm.conn.createLoadBalancer(cm.namer.LoadBalancerName())
		if err != nil {
			return
		}
		Logger(cm.ctx).Infof("Created loadbalancer lbIp = %v", lbIp)
		for m := range cm.cluster.Spec.Masters {
			cm.cluster.Spec.Masters[m].Labels[api.PharmerHASetup] = "true"
			cm.cluster.Spec.Masters[m].Labels[api.PharmerLoadBalancerIP] = lbIp
		}
		Store(cm.ctx).Clusters().Update(cm.cluster)
	}

	leaderMaster := masterNG[0]
	if d, _ := cm.conn.instanceIfExists(leaderMaster); d == nil {
		Logger(cm.ctx).Info("Creating master instance")
		acts = append(acts, api.Action{
			Action:   api.ActionAdd,
			Resource: "MasterInstance",
			Message:  fmt.Sprintf("Master instance %s will be created", cm.namer.MasterName()),
		})
		if !dryRun {
			if err = cm.Create(cm.cluster.Spec.ClusterAPI, leaderMaster); err != nil {
				return
			}

		}
	} else {
		acts = append(acts, api.Action{
			Action:   api.ActionNOP,
			Resource: "MasterInstance",
			Message:  fmt.Sprintf("master instance %v already exist", leaderMaster.Name),
		})
	}
	if haSetup {
		cm.cluster.Spec.ClusterAPI.Status.APIEndpoints = []clusterv1.APIEndpoint{
			{
				Host: lbIp,
				Port: int(cm.cluster.Spec.API.BindPort),
			},
		}

	}
	for m := range cm.cluster.Spec.Masters {
		cm.cluster.Spec.Masters[m].Labels[api.EtcdServerAddress] = strings.Join(cm.cluster.Spec.ETCDServers, ",")
	}
	Store(cm.ctx).Clusters().Update(cm.cluster)

	var kc kubernetes.Interface
	kc, err = cm.GetAdminClient()
	if err != nil {
		return
	}
	// wait for nodes to start
	if err = WaitForReadyMaster(cm.ctx, kc); err != nil {
		return
	}

	// needed to get master_internal_ip
	cm.cluster.Status.Phase = api.ClusterReady
	if _, err = Store(cm.ctx).Clusters().UpdateStatus(cm.cluster); err != nil {
		return
	}

	// need to run ccm
	if err = CreateCredentialSecret(cm.ctx, kc, cm.cluster); err != nil {
		return
	}

	ca, err := NewClusterApi(cm.ctx, cm.cluster, kc)
	if err != nil {
		return acts, err
	}
	if err := ca.Apply(); err != nil {
		return acts, err
	}
	return acts, err
}

// Scales up/down regular node groups
func (cm *ClusterManager) applyScale(dryRun bool) (acts []api.Action, err error) {
	Logger(cm.ctx).Infoln("scaling machine set")
	var cs clientset.Interface
	cs, err = NewClusterApiClient(cm.ctx, cm.cluster)
	if err != nil {
		return
	}
	client := cs.ClusterV1alpha1()

	var machineSet []*clusterv1.MachineSet
	//var msc *clusterv1.MachineSet
	machineSet, err = Store(cm.ctx).MachineSet(cm.cluster.Name).List(metav1.ListOptions{})
	if err != nil {
		return
	}
	for _, ms := range machineSet {
		if ms.DeletionTimestamp != nil {
			if err = client.MachineSets(core.NamespaceDefault).Delete(ms.Name, &metav1.DeleteOptions{}); err != nil {
				return
			}
			err = Store(cm.ctx).MachineSet(cm.cluster.Name).Delete(ms.Name)
			return
		}

		_, err = client.MachineSets(core.NamespaceDefault).Get(ms.Name, metav1.GetOptions{})
		if kerr.IsNotFound(err) {
			_, err = client.MachineSets(core.NamespaceDefault).Create(ms)
			if err != nil {
				return
			}
		} else {
			if _, err = client.MachineSets(core.NamespaceDefault).Update(ms); err != nil {
				return
			}

			//patch makes provider config null :(. TODO(): why??
			/*if _, err = PatchMachineSet(cs, msc, ms); err != nil {
				return
			}*/
		}

	}

	return
}

// Deletes master(s) and releases other cloud resources
func (cm *ClusterManager) applyDelete(dryRun bool) (acts []api.Action, err error) {
	if cm.cluster.Status.Phase == api.ClusterReady {
		cm.cluster.Status.Phase = api.ClusterDeleting
	}
	_, err = Store(cm.ctx).Clusters().UpdateStatus(cm.cluster)
	if err != nil {
		return
	}

	var kc kubernetes.Interface
	kc, err = cm.GetAdminClient()
	if err != nil {
		return
	}

	nodes := &core.NodeList{}
	nodes, err = kc.CoreV1().Nodes().List(metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{
			//api.NodePoolKey: masterNG.Name,
		}).String(),
	})
	if err != nil && !kerr.IsNotFound(err) {
		return
	} else if err == nil {
		acts = append(acts, api.Action{
			Action:   api.ActionDelete,
			Resource: "MasterInstance",
			Message:  fmt.Sprintf("Will delete master instance with name %v", cm.namer.MasterName()),
		})
		if !dryRun {
			for _, instance := range nodes.Items {
				err = cm.conn.DeleteInstanceByProviderID(instance.Spec.ProviderID)
				if err != nil {
					Logger(cm.ctx).Infof("Failed to delete instance %s. Reason: %s", instance.Spec.ProviderID, err)
				}
				err = cm.conn.deleteStackScript(instance.Name, string(clustercommon.MasterRole))
				if err != nil {
					Logger(cm.ctx).Infof("Failed to delete stack script %s. Reason: %s", instance.Name, err)
				}
			}
		}
	}

	if IsHASetup(cm.cluster) {
		cm.conn.deleteLoadBalancer(cm.namer.LoadBalancerName())
	}

	// Failed
	cm.cluster.Status.Phase = api.ClusterDeleted
	_, err = Store(cm.ctx).Clusters().UpdateStatus(cm.cluster)
	if err != nil {
		return
	}

	Logger(cm.ctx).Infof("Cluster %v deletion is deleted successfully", cm.cluster.Name)
	return
}

func (cm *ClusterManager) applyUpgrade(dryRun bool) (acts []api.Action, err error) {
	var kc kubernetes.Interface
	if kc, err = cm.GetAdminClient(); err != nil {
		return
	}

	upm := NewUpgradeManager(cm.ctx, cm, kc, cm.cluster)
	a, err := upm.Apply(dryRun)
	if err != nil {
		return
	}
	acts = append(acts, a...)
	if !dryRun {
		cm.cluster.Status.Phase = api.ClusterReady
		if _, err = Store(cm.ctx).Clusters().UpdateStatus(cm.cluster); err != nil {
			return
		}
	}
	return
}
