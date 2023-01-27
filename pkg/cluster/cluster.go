/*
Copyright 2020 The Kubernetes Authors.

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

package cluster

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	logf "sigs.k8s.io/controller-runtime/pkg/internal/log"

	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	intrec "sigs.k8s.io/controller-runtime/pkg/internal/recorder"
)

// Cluster provides various methods to interact with a cluster.
type Cluster interface {
	// GetHTTPClient returns an HTTP client that can be used to talk to the apiserver
	GetHTTPClient() *http.Client

	// GetConfig returns an initialized Config
	GetConfig() *rest.Config

	// GetCache returns a cache.Cache
	GetCache() cache.Cache

	// GetScheme returns an initialized Scheme
	GetScheme() *runtime.Scheme

	// GetClient returns a client configured with the Config. This client may
	// not be a fully "direct" client -- it may read from a cache, for
	// instance.  See Options.NewClient for more information on how the default
	// implementation works.
	GetClient() client.Client

	// GetFieldIndexer returns a client.FieldIndexer configured with the client
	GetFieldIndexer() client.FieldIndexer

	// GetEventRecorderFor returns a new EventRecorder for the provided name
	GetEventRecorderFor(name string) record.EventRecorder

	// GetRESTMapper returns a RESTMapper
	GetRESTMapper() meta.RESTMapper

	// GetAPIReader returns a reader that will be configured to use the API server.
	// This should be used sparingly and only when the client does not fit your
	// use case.
	GetAPIReader() client.Reader

	// Start starts the cluster
	Start(ctx context.Context) error
}

// Options are the possible options that can be configured for a Cluster.
type Options struct {
	// Scheme is the scheme used to resolve runtime.Objects to GroupVersionKinds / Resources
	// Defaults to the kubernetes/client-go scheme.Scheme, but it's almost always better
	// idea to pass your own scheme in.  See the documentation in pkg/scheme for more information.
	Scheme *runtime.Scheme

	// MapperProvider provides the rest mapper used to map go types to Kubernetes APIs
	MapperProvider func(c *rest.Config, httpClient *http.Client) (meta.RESTMapper, error)

	// Logger is the logger that should be used by this Cluster.
	// If none is set, it defaults to log.Log global logger.
	Logger logr.Logger

	// SyncPeriod determines the minimum frequency at which watched resources are
	// reconciled. A lower period will correct entropy more quickly, but reduce
	// responsiveness to change if there are many watched resources. Change this
	// value only if you know what you are doing. Defaults to 10 hours if unset.
	// there will a 10 percent jitter between the SyncPeriod of all controllers
	// so that all controllers will not send list requests simultaneously.
	SyncPeriod *time.Duration

	// Namespace if specified restricts the manager's cache to watch objects in
	// the desired namespace Defaults to all namespaces
	//
	// Note: If a namespace is specified, controllers can still Watch for a
	// cluster-scoped resource (e.g Node).  For namespaced resources the cache
	// will only hold objects from the desired namespace.
	Namespace string

	// HTTPClient is the http client that will be used to create the default
	// Cache and Client. If not set the rest.HTTPClientFor function will be used
	// to create the http client.
	HTTPClient *http.Client

	// NewCache is the function that will create the cache to be used
	// by the manager. If not set this will use the default new cache function.
	NewCache cache.NewCacheFunc

	// NewClient is the func that creates the client to be used by the manager.
	// If not set this will create a Client backed by a Cache for read operations
	// and a direct Client for write operations.
	// NOTE: The default client will not cache Unstructured.
	NewClient client.NewClientFunc

	// ClientDisableCacheFor tells the client that, if any cache is used, to bypass it
	// for the given objects.
	ClientDisableCacheFor []client.Object

	// DryRunClient specifies whether the client should be configured to enforce
	// dryRun mode.
	DryRunClient bool

	// EventBroadcaster records Events emitted by the manager and sends them to the Kubernetes API
	// Use this to customize the event correlator and spam filter
	//
	// Deprecated: using this may cause goroutine leaks if the lifetime of your manager or controllers
	// is shorter than the lifetime of your process.
	EventBroadcaster record.EventBroadcaster

	// makeBroadcaster allows deferring the creation of the broadcaster to
	// avoid leaking goroutines if we never call Start on this manager.  It also
	// returns whether or not this is a "owned" broadcaster, and as such should be
	// stopped with the manager.
	makeBroadcaster intrec.EventBroadcasterProducer

	// Dependency injection for testing
	newRecorderProvider func(config *rest.Config, httpClient *http.Client, scheme *runtime.Scheme, logger logr.Logger, makeBroadcaster intrec.EventBroadcasterProducer) (*intrec.Provider, error)
}

// Option can be used to manipulate Options.
type Option func(*Options)

// New constructs a brand new cluster.
func New(config *rest.Config, opts ...Option) (Cluster, error) {
	if config == nil {
		return nil, errors.New("must specify Config")
	}

	options := Options{}
	for _, opt := range opts {
		opt(&options)
	}
	options, err := setOptionsDefaults(options, config)
	if err != nil {
		options.Logger.Error(err, "Failed to set defaults")
		return nil, err
	}

	// Create the mapper provider
	mapper, err := options.MapperProvider(config, options.HTTPClient)
	if err != nil {
		options.Logger.Error(err, "Failed to get API Group-Resources")
		return nil, err
	}

	// Create the cache for the cached read client and registering informers
	cache, err := options.NewCache(config, cache.Options{HTTPClient: options.HTTPClient, Scheme: options.Scheme, Mapper: mapper, Resync: options.SyncPeriod, Namespace: options.Namespace})
	if err != nil {
		return nil, err
	}

	writeObj, err := options.NewClient(config, client.Options{
		HTTPClient: options.HTTPClient,
		Scheme:     options.Scheme,
		Mapper:     mapper,
		Cache: &client.CacheOptions{
			Reader:     cache,
			DisableFor: options.ClientDisableCacheFor,
		},
	})
	if err != nil {
		return nil, err
	}

	if options.DryRunClient {
		writeObj = client.NewDryRunClient(writeObj)
	}

	// Create the API Reader, a client with no cache.
	apiReader, err := client.New(config, client.Options{Scheme: options.Scheme, Mapper: mapper})
	if err != nil {
		return nil, err
	}

	// Create the recorder provider to inject event recorders for the components.
	// TODO(directxman12): the log for the event provider should have a context (name, tags, etc) specific
	// to the particular controller that it's being injected into, rather than a generic one like is here.
	recorderProvider, err := options.newRecorderProvider(config, options.HTTPClient, options.Scheme, options.Logger.WithName("events"), options.makeBroadcaster)
	if err != nil {
		return nil, err
	}

	return &cluster{
		config:           config,
		httpClient:       options.HTTPClient,
		scheme:           options.Scheme,
		cache:            cache,
		fieldIndexes:     cache,
		client:           writeObj,
		apiReader:        apiReader,
		recorderProvider: recorderProvider,
		mapper:           mapper,
		logger:           options.Logger,
	}, nil
}

// setOptionsDefaults set default values for Options fields.
func setOptionsDefaults(options Options, config *rest.Config) (Options, error) {
	if options.HTTPClient == nil {
		var err error
		options.HTTPClient, err = rest.HTTPClientFor(config)
		if err != nil {
			return options, err
		}
	}

	// Use the Kubernetes client-go scheme if none is specified
	if options.Scheme == nil {
		options.Scheme = scheme.Scheme
	}

	if options.MapperProvider == nil {
		options.MapperProvider = func(c *rest.Config, httpClient *http.Client) (meta.RESTMapper, error) {
			return apiutil.NewDynamicRESTMapper(c, httpClient)
		}
	}

	// Allow users to define how to create a new client
	if options.NewClient == nil {
		options.NewClient = client.New
	}

	// Allow newCache to be mocked
	if options.NewCache == nil {
		options.NewCache = cache.New
	}

	// Allow newRecorderProvider to be mocked
	if options.newRecorderProvider == nil {
		options.newRecorderProvider = intrec.NewProvider
	}

	// This is duplicated with pkg/manager, we need it here to provide
	// the user with an EventBroadcaster and there for the Leader election
	if options.EventBroadcaster == nil {
		// defer initialization to avoid leaking by default
		options.makeBroadcaster = func() (record.EventBroadcaster, bool) {
			return record.NewBroadcaster(), true
		}
	} else {
		options.makeBroadcaster = func() (record.EventBroadcaster, bool) {
			return options.EventBroadcaster, false
		}
	}

	if options.Logger.GetSink() == nil {
		options.Logger = logf.RuntimeLog.WithName("cluster")
	}

	return options, nil
}
