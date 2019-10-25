package sync

import (
	"sort"
	concurrency "sync"
	"time"

	kapiextensions "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	deprecated_dynamic "k8s.io/client-go/deprecated-dynamic"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/util/flowcontrol"
	kaggregator "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	configsv1alpha1 "github.com/openshift/static-config-operator/pkg/apis/staticcontent/v1alpha1"
)

var log = logf.Log.WithName("sync")

type ClusterPlatform string

const (
	syncOpenshiftManagedClusterLabelKey     = "api.openshift.com/managed"
	syncHiveClusterPlatformLabelKey         = "hive.openshift.io/cluster-platform"
	syncOpenshiftManagedGitHashLabelKey     = "managed.openshift.io/gitHash"
	syncOpenshiftManagedGitRepoNameLabelKey = "managed.openshift.io/gitRepoName"
	syncOwnedByPodLabelKey                  = "managed.openshift.io/static-config-operator-owned"

	ClusterPlatformAWS ClusterPlatform = "aws"
	ClusterPlatformGCP ClusterPlatform = "gcp"
)

type Interface interface {
	Sync() (reconcile.Result, error)
	SyncPeriodic(*time.Duration)
}

type Sync struct {
	store     map[string]unstructured.Unstructured
	storeLock concurrency.Mutex
	config    *configsv1alpha1.Config

	client client.Client
	scheme *runtime.Scheme

	restconfig *rest.Config
	ac         *kaggregator.Clientset
	ae         *kapiextensions.Clientset
	cli        *discovery.DiscoveryClient
	// TODO: Rebase this
	dyn deprecated_dynamic.ClientPool
	grs []*restmapper.APIGroupResources
	kc  kubernetes.Interface
}

func New(config *configsv1alpha1.Config, manager manager.Manager) (*Sync, error) {
	s := &Sync{
		config: config,
		client: manager.GetClient(),
		scheme: manager.GetScheme(),
	}

	var err error
	s.restconfig = manager.GetConfig()
	if err != nil {
		return nil, err
	}
	s.restconfig.RateLimiter = flowcontrol.NewFakeAlwaysRateLimiter()

	s.kc, err = kubernetes.NewForConfig(s.restconfig)
	if err != nil {
		return nil, err
	}

	s.ac, err = kaggregator.NewForConfig(s.restconfig)
	if err != nil {
		return nil, err
	}

	s.ae, err = kapiextensions.NewForConfig(s.restconfig)
	if err != nil {
		return nil, err
	}

	s.cli, err = discovery.NewDiscoveryClientForConfig(s.restconfig)
	if err != nil {
		return nil, err
	}

	s.grs, err = restmapper.GetAPIGroupResources(s.cli)
	if err != nil {
		return nil, err
	}
	rm := restmapper.NewDiscoveryRESTMapper(s.grs)
	//TODO: Rebase this
	s.dyn = deprecated_dynamic.NewClientPool(s.restconfig, rm, dynamic.LegacyAPIPathResolverFunc)

	err = s.readDB()
	if err != nil {
		return nil, err
	}

	return s, nil
}

// SyncPeriodic syncs all resources with predefined timer
// ourside standart operator sync loop
func (s *Sync) SyncPeriodic(interval *time.Duration) {
	t := time.NewTicker(*interval)
	for {
		log.Info("starting sync")
		if _, err := s.Sync(); err != nil {
			log.Error(err, "sync error")
		} else {
			log.Info("sync done")
		}
		<-t.C
	}
}

// Sync syncs all resources into the cluster
func (s *Sync) Sync() (reconcile.Result, error) {
	log.Info("start sync process")
	var keys []string
	for k := range s.store {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// crd needs to land early to get initialized
	log.Info("applying crd resources")
	if err := s.applyResources(crdFilter, keys); err != nil {
		return reconcile.Result{}, err
	}
	// namespaces must exist before namespaced objects.
	log.Info("applying ns resources")
	if err := s.applyResources(nsFilter, keys); err != nil {
		return reconcile.Result{}, err
	}
	// create serviceaccounts
	log.Info("applying sa resources")
	if err := s.applyResources(saFilter, keys); err != nil {
		return reconcile.Result{}, err
	}
	// create all secrets and configmaps
	log.Info("applying cfg resources")
	if err := s.applyResources(cfgFilter, keys); err != nil {
		return reconcile.Result{}, err
	}
	// default storage class must be created before PVCs as the admission controller is edge-triggered
	log.Info("applying storageClass resources")
	if err := s.applyResources(storageClassFilter, keys); err != nil {
		return reconcile.Result{}, err
	}

	// refresh dynamic client for CRD to be added
	if err := s.updateDynamicClient(); err != nil {
		return reconcile.Result{}, err
	}

	// create all, except targeted CRDs resources
	log.Info("applying other resources")
	if err := s.applyResources(everythingElseFilter, keys); err != nil {
		return reconcile.Result{}, err
	}

	return s.deleteOrphans()
}
