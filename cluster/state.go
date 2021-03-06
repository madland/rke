package cluster

import (
	"fmt"
	"os"
	"time"

	"github.com/rancher/rke/k8s"
	"github.com/rancher/types/apis/management.cattle.io/v3"
	"github.com/sirupsen/logrus"
	yaml "gopkg.in/yaml.v2"
	"k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

func (c *Cluster) SaveClusterState(rkeConfig *v3.RancherKubernetesEngineConfig) error {
	// Reinitialize kubernetes Client
	var err error
	c.KubeClient, err = k8s.NewClient(c.LocalKubeConfigPath)
	if err != nil {
		return fmt.Errorf("Failed to re-initialize Kubernetes Client: %v", err)
	}
	err = saveClusterCerts(c.KubeClient, c.Certificates)
	if err != nil {
		return fmt.Errorf("[certificates] Failed to Save Kubernetes certificates: %v", err)
	}
	err = saveStateToKubernetes(c.KubeClient, c.LocalKubeConfigPath, rkeConfig)
	if err != nil {
		return fmt.Errorf("[state] Failed to save configuration state: %v", err)
	}
	return nil
}

func (c *Cluster) GetClusterState() (*Cluster, error) {
	var err error
	var currentCluster *Cluster

	// check if local kubeconfig file exists
	if _, err = os.Stat(c.LocalKubeConfigPath); !os.IsNotExist(err) {
		logrus.Infof("[state] Found local kube config file, trying to get state from cluster")

		// to handle if current local admin is down and we need to use new cp from the list
		if !isLocalConfigWorking(c.LocalKubeConfigPath) {
			if err := rebuildLocalAdminConfig(c); err != nil {
				return nil, err
			}
		}

		// initiate kubernetes client
		c.KubeClient, err = k8s.NewClient(c.LocalKubeConfigPath)
		if err != nil {
			logrus.Warnf("Failed to initiate new Kubernetes Client: %v", err)
			return nil, nil
		}
		// Get previous kubernetes state
		currentCluster = getStateFromKubernetes(c.KubeClient, c.LocalKubeConfigPath)
		// Get previous kubernetes certificates
		if currentCluster != nil {
			currentCluster.Certificates, err = getClusterCerts(c.KubeClient)
			currentCluster.DockerDialerFactory = c.DockerDialerFactory
			if err != nil {
				return nil, fmt.Errorf("Failed to Get Kubernetes certificates: %v", err)
			}
			// setting cluster defaults for the fetched cluster as well
			currentCluster.setClusterDefaults()

			if err := currentCluster.InvertIndexHosts(); err != nil {
				return nil, fmt.Errorf("Failed to classify hosts from fetched cluster: %v", err)
			}
			currentCluster.Certificates, err = regenerateAPICertificate(c, currentCluster.Certificates)
			if err != nil {
				return nil, fmt.Errorf("Failed to regenerate KubeAPI certificate %v", err)
			}
		}
	}
	return currentCluster, nil
}

func saveStateToKubernetes(kubeClient *kubernetes.Clientset, kubeConfigPath string, rkeConfig *v3.RancherKubernetesEngineConfig) error {
	logrus.Infof("[state] Saving cluster state to Kubernetes")
	clusterFile, err := yaml.Marshal(*rkeConfig)
	if err != nil {
		return err
	}
	timeout := make(chan bool, 1)
	go func() {
		for {
			err := k8s.UpdateConfigMap(kubeClient, clusterFile, StateConfigMapName)
			if err != nil {
				time.Sleep(time.Second * 5)
				continue
			}
			logrus.Infof("[state] Successfully Saved cluster state to Kubernetes ConfigMap: %s", StateConfigMapName)
			timeout <- true
			break
		}
	}()
	select {
	case <-timeout:
		return nil
	case <-time.After(time.Second * UpdateStateTimeout):
		return fmt.Errorf("[state] Timeout waiting for kubernetes to be ready")
	}
}

func getStateFromKubernetes(kubeClient *kubernetes.Clientset, kubeConfigPath string) *Cluster {
	logrus.Infof("[state] Fetching cluster state from Kubernetes")
	var cfgMap *v1.ConfigMap
	var currentCluster Cluster
	var err error
	timeout := make(chan bool, 1)
	go func() {
		for {
			cfgMap, err = k8s.GetConfigMap(kubeClient, StateConfigMapName)
			if err != nil {
				time.Sleep(time.Second * 5)
				continue
			}
			logrus.Infof("[state] Successfully Fetched cluster state to Kubernetes ConfigMap: %s", StateConfigMapName)
			timeout <- true
			break
		}
	}()
	select {
	case <-timeout:
		clusterData := cfgMap.Data[StateConfigMapName]
		err := yaml.Unmarshal([]byte(clusterData), &currentCluster)
		if err != nil {
			return nil
		}
		return &currentCluster
	case <-time.After(time.Second * GetStateTimeout):
		logrus.Infof("Timed out waiting for kubernetes cluster to get state")
		return nil
	}
}

func GetK8sVersion(localConfigPath string) (string, error) {
	logrus.Debugf("[version] Using %s to connect to Kubernetes cluster..", localConfigPath)
	k8sClient, err := k8s.NewClient(localConfigPath)
	if err != nil {
		return "", fmt.Errorf("Failed to create Kubernetes Client: %v", err)
	}
	discoveryClient := k8sClient.DiscoveryClient
	logrus.Debugf("[version] Getting Kubernetes server version..")
	serverVersion, err := discoveryClient.ServerVersion()
	if err != nil {
		return "", fmt.Errorf("Failed to get Kubernetes server version: %v", err)
	}
	return fmt.Sprintf("%#v", *serverVersion), nil
}
