package sync

import (
	"context"
	"sort"

	concurrency "sync"

	"github.com/ghodss/yaml"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	configsv1alpha1 "github.com/openshift/static-config-operator/pkg/apis/configs/v1alpha1"
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
}

type Sync struct {
	store     map[string]unstructured.Unstructured
	storeLock concurrency.Mutex
	config    *configsv1alpha1.Config

	client client.Client
	scheme *runtime.Scheme
}

func New(config *configsv1alpha1.Config, client client.Client, scheme *runtime.Scheme) (*Sync, error) {
	s := &Sync{
		config: config,
		client: client,
		scheme: scheme,
	}

	err := s.readDB()
	if err != nil {
		return nil, err
	}

	return s, nil
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

	return nil
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

func (s *Sync) Sync(config *configsv1alpha1.Config) (reconcile.Result, error) {
	// impose an order to improve debuggability.
	var keys []string
	for k := range s.store {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		o := s.store[k]

		// Set Config instance as the owner and controller
		obj := o.DeepCopyObject()
		if err := controllerutil.SetControllerReference(config, obj.(metav1.Object), s.scheme); err != nil {
			return reconcile.Result{}, err
		}

		// TODO: Add filtering here and ordering
		log.Info("create resource", o.GetNamespace(), o.GetName())
		if err := s.client.Create(context.TODO(), obj); err != nil {
			//TODO: check for update
			if errors.IsAlreadyExists(err) {
				continue
			}
			return reconcile.Result{}, err
		}

	}

	return reconcile.Result{}, nil
}
