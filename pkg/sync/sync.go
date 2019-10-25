package sync

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	concurrency "sync"

	"github.com/ghodss/yaml"
	"golang.org/x/sync/errgroup"
	kapiextensions "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	deprecated_dynamic "k8s.io/client-go/deprecated-dynamic"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/client-go/util/retry"
	kaggregator "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	configsv1alpha1 "github.com/openshift/static-config-operator/pkg/apis/staticcontent/v1alpha1"
	"github.com/openshift/static-config-operator/pkg/util/cmp"
)

var log = logf.Log.WithName("sync")

type ClusterPlatform string

const (
	syncOpenshiftManagedClusterLabelKey     = "api.openshift.com/managed"
	syncHiveClusterPlatformLabelKey         = "hive.openshift.io/cluster-platform"
	syncOpenshiftManagedGitHashLabelKey     = "managed.openshift.io/gitHash"
	syncOpenshiftManagedGitRepoNameLabelKey = "managed.openshift.io/gitRepoName"

	ClusterPlatformAWS ClusterPlatform = "aws"
	ClusterPlatformGCP ClusterPlatform = "gcp"
)

type Interface interface {
	Sync(*configsv1alpha1.Config) (reconcile.Result, error)
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
		if _, err := s.Sync(s.config); err != nil {
			log.Error(err, "sync error")
		} else {
			log.Info("sync done")
		}
		<-t.C
	}
}

func (s *Sync) Sync(config *configsv1alpha1.Config) (reconcile.Result, error) {
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

	return reconcile.Result{}, nil
}

// ReadDB reads previously exported objects into a map via go-bindata as well as
// populating configuration items via translate().
func (s *Sync) readDB() error {
	s.store = map[string]unstructured.Unstructured{}

	var g errgroup.Group
	for _, asset := range AssetNames() {
		asset := asset // https://golang.org/doc/faq#closures_and_goroutines

		g.Go(func() error {
			b, err := Asset(asset)
			if err != nil {
				return err
			}

			o, err := unmarshal(b)
			if err != nil {
				log.Error(err, "unmarshal error", asset)
				return err
			}

			// defore we even add this object to store for sync,
			// check if platform matched and clean labels
			if !validatePlatform(s.config, o) {
				return nil
			}

			o, err = translateAsset(o, s.config)
			if err != nil {
				log.Error(err, "translateAsset error", asset)
				return err
			}
			s.storeLock.Lock()
			defer s.storeLock.Unlock()

			defaults(o)
			s.store[keyFunc(o.GroupVersionKind().GroupKind(), o.GetNamespace(), o.GetName())] = o

			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	log.Info("finish translate")

	return nil
}

func validatePlatform(config *configsv1alpha1.Config, o unstructured.Unstructured) bool {
	// if no managed-cluster label set - return false. It is mandatory
	if len(o.GetLabels()[syncOpenshiftManagedClusterLabelKey]) == 0 {
		log.Error(fmt.Errorf("objectdoes not contains managed-cluster label key"), o.GetName())
		return false
	}
	// IF platform key is set and match config - return true
	// OR platform key is not set - true - all clouds
	if (len(o.GetLabels()[syncHiveClusterPlatformLabelKey]) > 0 &&
		strings.EqualFold(config.Spec.Platform, o.GetLabels()[syncHiveClusterPlatformLabelKey])) ||
		len(o.GetLabels()[syncHiveClusterPlatformLabelKey]) == 0 {
		return true
	}
	return false
}

// unmarshal has to reimplement yaml.unmarshal because it universally mangles yaml
// integers into float64s, whereas the Kubernetes client library uses int64s
// wherever it can.  Such a difference can cause us to update objects when
// we don't actually need to.
func unmarshal(b []byte) (unstructured.Unstructured, error) {
	json, err := yaml.YAMLToJSON(b)
	if err != nil {
		return unstructured.Unstructured{}, err
	}

	var o unstructured.Unstructured
	_, _, err = unstructured.UnstructuredJSONScheme.Decode(json, nil, &o)
	if err != nil {
		return unstructured.Unstructured{}, err
	}

	return o, nil
}

// updateDynamicClient updates the client's server API group resource
// information and dynamic client pool.
func (s *Sync) updateDynamicClient() error {
	grs, err := restmapper.GetAPIGroupResources(s.cli)
	if err != nil {
		return err
	}
	s.grs = grs

	rm := restmapper.NewDiscoveryRESTMapper(s.grs)
	s.dyn = deprecated_dynamic.NewClientPool(s.restconfig, rm, dynamic.LegacyAPIPathResolverFunc)

	return nil
}

// applyResources creates or updates all resources in db that match the provided
// filter.
func (s *Sync) applyResources(filter func(unstructured.Unstructured) bool, keys []string) error {
	for _, k := range keys {
		o := s.store[k]

		if !filter(o) {
			continue
		}

		if err := s.write(&o); err != nil {
			return err
		}
	}
	return nil
}

// write synchronises a single object with the API server.
func (s *Sync) write(o *unstructured.Unstructured) error {
	dc, err := s.dyn.ClientForGroupVersionKind(o.GroupVersionKind())
	if err != nil {
		return err
	}
	var gr *restmapper.APIGroupResources
	for _, g := range s.grs {
		if g.Group.Name == o.GroupVersionKind().Group {
			gr = g
			break
		}
	}
	if gr == nil {
		return errors.New("couldn't find group " + o.GroupVersionKind().Group)
	}

	o = o.DeepCopy()
	var res *metav1.APIResource
	for _, r := range gr.VersionedResources[o.GroupVersionKind().Version] {
		if gr.Group.Name == "template.openshift.io" && r.Name == "processedtemplates" {
			continue
		}
		if r.Kind == o.GroupVersionKind().Kind {
			res = &r
			break
		}
	}
	if res == nil {
		return errors.New("couldn't find kind " + o.GroupVersionKind().Kind)
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() (err error) {
		var existing *unstructured.Unstructured
		existing, err = dc.Resource(res, o.GetNamespace()).Get(o.GetName(), metav1.GetOptions{})
		if kerrors.IsNotFound(err) {
			log.Info("Create " + keyFunc(o.GroupVersionKind().GroupKind(), o.GetNamespace(), o.GetName()))
			_, err = dc.Resource(res, o.GetNamespace()).Create(o)
			if kerrors.IsAlreadyExists(err) {
				// The "hot path" in write() is Get, check, then maybe Update.
				// Optimising for this has the disadvantage that at cluster
				// creation we can race with API server or controller
				// initialisation. Between Get returning NotFound and us trying
				// to Create, the object might be created.  In this case we
				// return a synthetic Conflict to force a retry.
				log.Error(err, "error while creating", o.GetNamespace(), o.GetName())
				err = kerrors.NewConflict(schema.GroupResource{Group: res.Group, Resource: res.Name}, o.GetName(), errors.New("synthetic"))
			}
			return
		}
		if err != nil {
			return
		}

		rv := existing.GetResourceVersion()

		err = clean(*existing)
		if err != nil {
			return
		}
		defaults(*existing)

		if !s.needsUpdate(existing, o) {
			return
		}
		printDiff(existing, o)

		o.SetResourceVersion(rv)
		_, err = dc.Resource(res, o.GetNamespace()).Update(o)
		if err != nil && strings.Contains(err.Error(), "updates to parameters are forbidden") {
			log.Info("object %s is not updateable, will delete and re-create", o.GetName())
			err = dc.Resource(res, o.GetNamespace()).Delete(o.GetName(), &metav1.DeleteOptions{})
			if err != nil {
				return
			}
			o.SetResourceVersion("")
			_, err = dc.Resource(res, o.GetNamespace()).Create(o)
		}

		return
	})

	return err
}

func printDiff(existing, o *unstructured.Unstructured) bool {
	// Don't show a diff if kind is Secret
	gk := o.GroupVersionKind().GroupKind()
	diffShown := false
	if gk.String() != "Secret" {
		if diff := cmp.Diff(*existing, *o); diff != "" {
			log.Info("diff", diff)
			diffShown = true
		}
	}
	return diffShown
}
