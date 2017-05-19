/*-
 * Copyright (c) 2016,2017, F5 Networks, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package appmanager

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"time"

	log "f5/vlogger"
	"tools/writer"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	watch "k8s.io/apimachinery/pkg/watch"
	rest "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const DefaultConfigMapLabel = "f5type in (virtual-server)"
const vsBindAddrAnnotation = "status.virtual-server.f5.com/ip"

type VirtualServerPortMap map[int32]*VirtualServerConfig

type Manager struct {
	vservers     *VirtualServers
	kubeClient   kubernetes.Interface
	restClient   rest.Interface
	configWriter writer.Writer
	// Use internal node IPs
	useNodeInternal bool
	// Running in nodeport (or cluster) mode
	isNodePort bool
	// Mutex to control access to node data
	// FIXME: Simple synchronization for now, it remains to be determined if we'll
	// need something more complicated (channels, etc?)
	oldNodesMutex sync.Mutex
	// Nodes from previous iteration of node polling
	oldNodes []string
	// Mutex for all informers (for informer CRUD)
	informersMutex sync.Mutex
	// App informer support
	vsQueue      workqueue.RateLimitingInterface
	appInformers map[string]*appInformer
	// Namespace informer support (namespace labels)
	nsQueue    workqueue.RateLimitingInterface
	nsInformer cache.SharedIndexInformer
}

// Struct to allow NewManager to receive all or only specific parameters.
type Params struct {
	KubeClient      kubernetes.Interface
	restClient      rest.Interface // package local for unit testing only
	ConfigWriter    writer.Writer
	UseNodeInternal bool
	IsNodePort      bool
}

// Create and return a new app manager that meets the Manager interface
func NewManager(params *Params) *Manager {
	vsQueue := workqueue.NewNamedRateLimitingQueue(
		workqueue.DefaultControllerRateLimiter(), "virtual-server-controller")
	nsQueue := workqueue.NewNamedRateLimitingQueue(
		workqueue.DefaultControllerRateLimiter(), "namespace-controller")
	manager := Manager{
		vservers:        NewVirtualServers(),
		kubeClient:      params.KubeClient,
		restClient:      params.restClient,
		configWriter:    params.ConfigWriter,
		useNodeInternal: params.UseNodeInternal,
		isNodePort:      params.IsNodePort,
		vsQueue:         vsQueue,
		nsQueue:         nsQueue,
		appInformers:    make(map[string]*appInformer),
	}
	if nil != manager.kubeClient && nil == manager.restClient {
		// This is the normal production case, but need the checks for unit tests.
		manager.restClient = manager.kubeClient.Core().RESTClient()
	}
	return &manager
}

func (appMgr *Manager) watchingAllNamespacesLocked() bool {
	if 0 == len(appMgr.appInformers) {
		// Not watching any namespaces.
		return false
	}
	_, watchingAll := appMgr.appInformers[""]
	return watchingAll
}

func (appMgr *Manager) AddNamespace(
	namespace string,
	cfgMapSelector labels.Selector,
	resyncPeriod time.Duration,
) error {
	appMgr.informersMutex.Lock()
	defer appMgr.informersMutex.Unlock()
	_, err := appMgr.addNamespaceLocked(namespace, cfgMapSelector, resyncPeriod)
	return err
}

func (appMgr *Manager) addNamespaceLocked(
	namespace string,
	cfgMapSelector labels.Selector,
	resyncPeriod time.Duration,
) (*appInformer, error) {
	if appMgr.watchingAllNamespacesLocked() {
		return nil, fmt.Errorf(
			"Cannot add additional namespaces when already watching all.")
	}
	if len(appMgr.appInformers) > 0 && "" == namespace {
		return nil, fmt.Errorf(
			"Cannot watch all namespaces when already watching specific ones.")
	}
	var appInf *appInformer
	if appInf, found := appMgr.appInformers[namespace]; found {
		return appInf, nil
	}
	appInf = appMgr.newAppInformer(namespace, cfgMapSelector, resyncPeriod)
	appMgr.appInformers[namespace] = appInf
	return appInf, nil
}

func (appMgr *Manager) removeNamespace(namespace string) error {
	appMgr.informersMutex.Lock()
	defer appMgr.informersMutex.Unlock()
	err := appMgr.removeNamespaceLocked(namespace)
	return err
}

func (appMgr *Manager) removeNamespaceLocked(namespace string) error {
	if _, found := appMgr.appInformers[namespace]; !found {
		return fmt.Errorf("No informers exist for namespace %v\n", namespace)
	}
	delete(appMgr.appInformers, namespace)
	return nil
}

func (appMgr *Manager) AddNamespaceInformer(
	labelSelector labels.Selector,
	resyncPeriod time.Duration,
) error {
	appMgr.informersMutex.Lock()
	defer appMgr.informersMutex.Unlock()
	if nil != appMgr.nsInformer {
		return fmt.Errorf("Already have a namespace informer added.")
	}
	if 0 != len(appMgr.appInformers) {
		return fmt.Errorf("Cannot set a namespace informer when informers " +
			"have been installed for one or more namespaces.")
	}
	appMgr.nsInformer = cache.NewSharedIndexInformer(
		newListWatchWithLabelSelector(
			appMgr.restClient,
			"namespaces",
			"",
			labelSelector,
		),
		&v1.Namespace{},
		resyncPeriod,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)
	appMgr.nsInformer.AddEventHandlerWithResyncPeriod(
		&cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { appMgr.enqueueNamespace(obj) },
			UpdateFunc: func(old, cur interface{}) { appMgr.enqueueNamespace(cur) },
			DeleteFunc: func(obj interface{}) { appMgr.enqueueNamespace(obj) },
		},
		resyncPeriod,
	)

	return nil
}

func (appMgr *Manager) enqueueNamespace(obj interface{}) {
	ns := obj.(*v1.Namespace)
	appMgr.nsQueue.Add(ns.ObjectMeta.Name)
}

func (appMgr *Manager) namespaceWorker() {
	for appMgr.processNextNamespace() {
	}
	fmt.Printf("exiting namespaceWorker\n")
}

func (appMgr *Manager) processNextNamespace() bool {
	key, quit := appMgr.nsQueue.Get()
	if quit {
		return false
	}
	defer appMgr.nsQueue.Done(key)

	err := appMgr.syncNamespace(key.(string))
	if err == nil {
		appMgr.nsQueue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
	appMgr.nsQueue.AddRateLimited(key)

	return true
}

func (appMgr *Manager) syncNamespace(nsName string) error {
	startTime := time.Now()
	defer func() {
		endTime := time.Now()
		log.Debugf("Finished syncing namespace %+v (%v)",
			nsName, endTime.Sub(startTime))
	}()
	_, exists, err := appMgr.nsInformer.GetIndexer().GetByKey(nsName)
	if nil != err {
		log.Warningf("Error looking up namespace '%v': %v\n", nsName, err)
		return err
	}

	appMgr.informersMutex.Lock()
	defer appMgr.informersMutex.Unlock()
	appInf, found := appMgr.getNamespaceInformerLocked(nsName)
	if exists && found {
		return nil
	}
	if exists {
		// exists but not found in informers map, add
		cfgMapSelector, err := labels.Parse(DefaultConfigMapLabel)
		if err != nil {
			return fmt.Errorf("Failed to parse Label Selector string: %v", err)
		}
		appInf, err = appMgr.addNamespaceLocked(nsName, cfgMapSelector, 0)
		if err != nil {
			return fmt.Errorf("Failed to add informers for namespace %v: %v",
				nsName, err)
		}
		appInf.start()
		appInf.waitForCacheSync()
	} else {
		// does not exist but found in informers map, delete
		// Clean up all virtual servers that reference a removed namespace
		appInf.stopInformers()
		appMgr.removeNamespaceLocked(nsName)
		appMgr.vservers.Lock()
		defer appMgr.vservers.Unlock()
		vsDeleted := 0
		appMgr.vservers.ForEach(func(key serviceKey, cfg *VirtualServerConfig) {
			if key.Namespace == nsName {
				if appMgr.vservers.Delete(key, "") {
					vsDeleted += 1
				}
			}
		})
		if vsDeleted > 0 {
			appMgr.outputConfigLocked()
		}
	}

	return nil
}

func (appMgr *Manager) GetWatchedNamespaces() []string {
	appMgr.informersMutex.Lock()
	defer appMgr.informersMutex.Unlock()
	var namespaces []string
	for k, _ := range appMgr.appInformers {
		namespaces = append(namespaces, k)
	}
	return namespaces
}

func (appMgr *Manager) GetNamespaceInformer() cache.SharedIndexInformer {
	return appMgr.nsInformer
}

type vsQueueKey struct {
	Namespace   string
	ServiceName string
}

type appInformer struct {
	namespace      string
	cfgMapInformer cache.SharedIndexInformer
	svcInformer    cache.SharedIndexInformer
	endptInformer  cache.SharedIndexInformer
	stopCh         chan struct{}
}

func (appMgr *Manager) newAppInformer(
	namespace string,
	cfgMapSelector labels.Selector,
	resyncPeriod time.Duration,
) *appInformer {
	appInf := appInformer{
		namespace: namespace,
		stopCh:    make(chan struct{}),
		cfgMapInformer: cache.NewSharedIndexInformer(
			newListWatchWithLabelSelector(
				appMgr.restClient,
				"configmaps",
				namespace,
				cfgMapSelector,
			),
			&v1.ConfigMap{},
			resyncPeriod,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
		),
		svcInformer: cache.NewSharedIndexInformer(
			newListWatchWithLabelSelector(
				appMgr.restClient,
				"services",
				namespace,
				labels.Everything(),
			),
			&v1.Service{},
			resyncPeriod,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
		),
		endptInformer: cache.NewSharedIndexInformer(
			newListWatchWithLabelSelector(
				appMgr.restClient,
				"endpoints",
				namespace,
				labels.Everything(),
			),
			&v1.Endpoints{},
			resyncPeriod,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
		),
	}

	appInf.cfgMapInformer.AddEventHandlerWithResyncPeriod(
		&cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { appMgr.enqueueConfigMap(obj) },
			UpdateFunc: func(old, cur interface{}) { appMgr.enqueueConfigMap(cur) },
			DeleteFunc: func(obj interface{}) { appMgr.enqueueConfigMap(obj) },
		},
		resyncPeriod,
	)

	appInf.svcInformer.AddEventHandlerWithResyncPeriod(
		&cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { appMgr.enqueueService(obj) },
			UpdateFunc: func(old, cur interface{}) { appMgr.enqueueService(cur) },
			DeleteFunc: func(obj interface{}) { appMgr.enqueueService(obj) },
		},
		resyncPeriod,
	)

	appInf.endptInformer.AddEventHandlerWithResyncPeriod(
		&cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { appMgr.enqueueEndpoints(obj) },
			UpdateFunc: func(old, cur interface{}) { appMgr.enqueueEndpoints(cur) },
			DeleteFunc: func(obj interface{}) { appMgr.enqueueEndpoints(obj) },
		},
		resyncPeriod,
	)
	return &appInf
}

func newListWatchWithLabelSelector(
	c cache.Getter,
	resource string,
	namespace string,
	labelSelector labels.Selector,
) cache.ListerWatcher {
	listFunc := func(options metav1.ListOptions) (runtime.Object, error) {
		return c.Get().
			Namespace(namespace).
			Resource(resource).
			VersionedParams(&options, metav1.ParameterCodec).
			LabelsSelectorParam(labelSelector).
			Do().
			Get()
	}
	watchFunc := func(options metav1.ListOptions) (watch.Interface, error) {
		return c.Get().
			Prefix("watch").
			Namespace(namespace).
			Resource(resource).
			VersionedParams(&options, metav1.ParameterCodec).
			LabelsSelectorParam(labelSelector).
			Watch()
	}
	return &cache.ListWatch{ListFunc: listFunc, WatchFunc: watchFunc}
}

func (appMgr *Manager) getNamespaceInformer(
	ns string,
) (*appInformer, bool) {
	appMgr.informersMutex.Lock()
	defer appMgr.informersMutex.Unlock()
	appInf, found := appMgr.getNamespaceInformerLocked(ns)
	return appInf, found
}

func (appMgr *Manager) getNamespaceInformerLocked(
	ns string,
) (*appInformer, bool) {
	toFind := ns
	if appMgr.watchingAllNamespacesLocked() {
		toFind = ""
	}
	appInf, found := appMgr.appInformers[toFind]
	return appInf, found
}

func (appInf *appInformer) start() {
	go appInf.cfgMapInformer.Run(appInf.stopCh)
	go appInf.svcInformer.Run(appInf.stopCh)
	go appInf.endptInformer.Run(appInf.stopCh)
}

func (appInf *appInformer) waitForCacheSync() {
	cache.WaitForCacheSync(
		appInf.stopCh,
		appInf.cfgMapInformer.HasSynced,
		appInf.svcInformer.HasSynced,
		appInf.endptInformer.HasSynced,
	)
}

func (appInf *appInformer) stopInformers() {
	close(appInf.stopCh)
}

func (appMgr *Manager) IsNodePort() bool {
	return appMgr.isNodePort
}

func (appMgr *Manager) UseNodeInternal() bool {
	return appMgr.useNodeInternal
}

func (appMgr *Manager) ConfigWriter() writer.Writer {
	return appMgr.configWriter
}

func (appMgr *Manager) Run(stopCh <-chan struct{}) {
	go appMgr.runImpl(stopCh)
}

func (appMgr *Manager) runImpl(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer appMgr.vsQueue.ShutDown()

	if nil != appMgr.nsInformer {
		// Using one worker for namespace label changes.
		appMgr.startAndSyncNamespaceInformer(stopCh)
		go wait.Until(appMgr.namespaceWorker, time.Second, stopCh)
	}

	appMgr.startAndSyncAppInformers()

	// Using only one virtual server worker currently.
	go wait.Until(appMgr.virtualServerWorker, time.Second, stopCh)

	<-stopCh
	appMgr.stopAppInformers()
}

func (appMgr *Manager) startAndSyncNamespaceInformer(stopCh <-chan struct{}) {
	appMgr.informersMutex.Lock()
	defer appMgr.informersMutex.Unlock()
	go appMgr.nsInformer.Run(stopCh)
	cache.WaitForCacheSync(stopCh, appMgr.nsInformer.HasSynced)
}

func (appMgr *Manager) startAndSyncAppInformers() {
	appMgr.informersMutex.Lock()
	defer appMgr.informersMutex.Unlock()
	appMgr.startAppInformersLocked()
	appMgr.waitForCacheSyncLocked()
}

func (appMgr *Manager) startAppInformersLocked() {
	for _, appInf := range appMgr.appInformers {
		appInf.start()
	}
}

func (appMgr *Manager) waitForCacheSync() {
	appMgr.informersMutex.Lock()
	defer appMgr.informersMutex.Unlock()
	appMgr.waitForCacheSyncLocked()
}

func (appMgr *Manager) waitForCacheSyncLocked() {
	for _, appInf := range appMgr.appInformers {
		appInf.waitForCacheSync()
	}
}

func (appMgr *Manager) stopAppInformers() {
	appMgr.informersMutex.Lock()
	defer appMgr.informersMutex.Unlock()
	for _, appInf := range appMgr.appInformers {
		appInf.stopInformers()
	}
}

func (appMgr *Manager) virtualServerWorker() {
	for appMgr.processNextVirtualServer() {
	}
}

func (appMgr *Manager) processNextVirtualServer() bool {
	key, quit := appMgr.vsQueue.Get()
	if quit {
		// The controller is shutting down.
		return false
	}
	defer appMgr.vsQueue.Done(key)

	err := appMgr.syncVirtualServer(key.(vsQueueKey))
	if err == nil {
		appMgr.vsQueue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("Sync %v failed with %v", key, err))
	appMgr.vsQueue.AddRateLimited(key)

	return true
}

func (appMgr *Manager) syncVirtualServer(vsKey vsQueueKey) error {
	startTime := time.Now()
	defer func() {
		endTime := time.Now()
		log.Debugf("Finished syncing virtual servers %+v (%v)",
			vsKey, endTime.Sub(startTime))
	}()

	// Get the informers for the namespace. This will tell us if we care about
	// this item.
	appInf, haveNamespace := appMgr.getNamespaceInformer(vsKey.Namespace)
	if !haveNamespace {
		// This shouldn't happen as the namespace is checked for every item before
		// it is added to the queue, but issue a warning if it does.
		log.Warningf(
			"Received an update for an item from an un-watched namespace %v",
			vsKey.Namespace)
		return nil
	}

	// Lookup the service
	svcKey := vsKey.Namespace + "/" + vsKey.ServiceName
	obj, svcFound, err := appInf.svcInformer.GetIndexer().GetByKey(svcKey)
	if nil != err {
		// Returning non-nil err will re-queue this item with rate-limiting.
		log.Warningf("Error looking up service '%v': %v\n", svcKey, err)
		return err
	}

	// Use a map to allow ports in the service to be looked up quickly while
	// looping through the ConfigMaps. The value is not currently used.
	svcPortMap := make(map[int32]bool)
	var svc *v1.Service
	if svcFound {
		svc = obj.(*v1.Service)
		for _, portSpec := range svc.Spec.Ports {
			svcPortMap[portSpec.Port] = false
		}
	}

	// vsMap stores all config maps currently in vservers matching vsKey,
	// indexed by port.
	vsMap := appMgr.getVirtualServersForKey(vsKey)

	vsFound := 0
	vsUpdated := 0
	vsDeleted := 0
	cfgMapsByIndex, err := appInf.cfgMapInformer.GetIndexer().ByIndex(
		"namespace", vsKey.Namespace)
	if nil != err {
		log.Warningf("Unable to list config maps for namespace '%v': %v",
			vsKey.Namespace, err)
		return err
	}
	for _, obj := range cfgMapsByIndex {
		// We need to look at all config maps in the store, parse the data blob,
		// and see if it belongs to the service that has changed.
		cm := obj.(*v1.ConfigMap)
		if cm.ObjectMeta.Namespace != vsKey.Namespace {
			continue
		}
		vsCfg, err := parseVirtualServerConfig(cm)
		if nil != err {
			// Ignore this config map for the time being. When the user updates it
			// so that it is valid it will be requeued.
			fmt.Errorf("Error parsing ConfigMap %v_%v",
				cm.ObjectMeta.Namespace, cm.ObjectMeta.Name)
			continue
		}
		if vsCfg.VirtualServer.Backend.ServiceName != vsKey.ServiceName {
			continue
		}

		// Match, remove from vsMap so we don't delete it at the end.
		delete(vsMap, vsCfg.VirtualServer.Backend.ServicePort)
		svcKey := serviceKey{
			Namespace:   vsKey.Namespace,
			ServiceName: vsKey.ServiceName,
			ServicePort: vsCfg.VirtualServer.Backend.ServicePort,
		}
		vsName := formatVirtualServerName(cm)
		if _, ok := svcPortMap[vsCfg.VirtualServer.Backend.ServicePort]; !ok {
			log.Debugf("Process Service delete - name: %v namespace: %v",
				vsKey.ServiceName, vsKey.Namespace)
			if appMgr.deactivateVirtualServer(svcKey, vsName, vsCfg) {
				vsUpdated += 1
			}
		}

		// Set the virtual server name in our parsed copy so we can compare it
		// later to see if it has actually changed.
		vsCfg.VirtualServer.Frontend.VirtualServerName = vsName

		if !svcFound {
			// The service is gone, de-activate it in the config.
			if appMgr.deactivateVirtualServer(svcKey, vsName, vsCfg) {
				vsUpdated += 1
			}
			continue
		}

		// Update pool members.
		vsFound += 1
		if appMgr.IsNodePort() {
			appMgr.updatePoolMembersForNodePort(svc, svcKey, vsCfg)
		} else {
			appMgr.updatePoolMembersForCluster(svc, svcKey, vsCfg, appInf)
		}

		// Set a status annotation to contain the virtualAddress bindAddr
		if vsCfg.VirtualServer.Frontend.IApp == "" &&
			vsCfg.VirtualServer.Frontend.VirtualAddress != nil &&
			vsCfg.VirtualServer.Frontend.VirtualAddress.BindAddr != "" {
			appMgr.setBindAddrAnnotation(cm, vsKey, vsCfg)
		}

		// This will only update the config if the vs actually changed.
		if appMgr.saveVirtualServer(svcKey, vsName, vsCfg) {
			vsUpdated += 1
		}
	}

	if len(vsMap) > 0 {
		// We get here when there are ports defined in the service that don't
		// have a corresponding config map.
		vsDeleted = appMgr.deleteUnusedVirtualServers(vsKey, vsMap)
	}

	log.Debugf("Updated %v of %v virtual server configs, deleted %v",
		vsUpdated, vsFound, vsDeleted)

	if vsUpdated > 0 || vsDeleted > 0 {
		appMgr.outputConfig()
	}

	return nil
}

func (appMgr *Manager) updatePoolMembersForNodePort(
	svc *v1.Service,
	vsKey serviceKey,
	vsCfg *VirtualServerConfig,
) {
	if svc.Spec.Type == v1.ServiceTypeNodePort {
		for _, portSpec := range svc.Spec.Ports {
			if portSpec.Port == vsKey.ServicePort {
				log.Debugf("Service backend matched %+v: using node port %v",
					vsKey, portSpec.NodePort)
				vsCfg.MetaData.Active = true
				vsCfg.MetaData.NodePort = portSpec.NodePort
				vsCfg.VirtualServer.Backend.PoolMemberAddrs =
					appMgr.getEndpointsForNodePort(portSpec.NodePort)
			}
		}
	} else {
		log.Debugf("Requested service backend %+v not of NodePort type", vsKey)
	}
}

func (appMgr *Manager) updatePoolMembersForCluster(
	svc *v1.Service,
	vsKey serviceKey,
	vsCfg *VirtualServerConfig,
	appInf *appInformer,
) {
	svcKey := vsKey.Namespace + "/" + vsKey.ServiceName
	item, found, _ := appInf.endptInformer.GetStore().GetByKey(svcKey)
	if !found {
		log.Debugf("Endpoints for service '%v' not found!", svcKey)
		return
	}
	eps, _ := item.(*v1.Endpoints)
	for _, portSpec := range svc.Spec.Ports {
		if portSpec.Port == vsKey.ServicePort {
			ipPorts := getEndpointsForService(portSpec.Name, eps)
			log.Debugf("Found endpoints for backend %+v: %v", vsKey, ipPorts)
			vsCfg.MetaData.Active = true
			vsCfg.VirtualServer.Backend.PoolMemberAddrs = ipPorts
		}
	}
}

func (appMgr *Manager) deactivateVirtualServer(
	vsKey serviceKey,
	vsName string,
	vsCfg *VirtualServerConfig,
) bool {
	updateConfig := false
	appMgr.vservers.Lock()
	defer appMgr.vservers.Unlock()
	if vs, ok := appMgr.vservers.Get(vsKey, vsName); ok {
		vsCfg.MetaData.Active = false
		vsCfg.VirtualServer.Backend.PoolMemberAddrs = nil
		if !reflect.DeepEqual(vs, vsCfg) {
			log.Debugf("Service delete matching backend %v %v deactivating config",
				vsKey, vsName)
			updateConfig = true
		}
	} else {
		// We have a config map but not a server. Put in the virtual server from
		// the config map.
		updateConfig = true
	}
	if updateConfig {
		appMgr.vservers.Assign(vsKey, vsName, vsCfg)
	}
	return updateConfig
}

func (appMgr *Manager) saveVirtualServer(
	vsKey serviceKey,
	vsName string,
	newVsCfg *VirtualServerConfig,
) bool {
	appMgr.vservers.Lock()
	defer appMgr.vservers.Unlock()
	if oldVsCfg, ok := appMgr.vservers.Get(vsKey, vsName); ok {
		if reflect.DeepEqual(oldVsCfg, newVsCfg) {
			// not changed, don't trigger a config write
			return false
		}
		log.Warningf("Overwriting existing entry for backend %+v", vsKey)
	}
	appMgr.vservers.Assign(vsKey, vsName, newVsCfg)
	return true
}

func (appMgr *Manager) getVirtualServersForKey(
	vsKey vsQueueKey,
) VirtualServerPortMap {
	// Return a copy of what is stored in vservers, mapped by port.
	appMgr.vservers.Lock()
	defer appMgr.vservers.Unlock()
	vsMap := make(VirtualServerPortMap)
	appMgr.vservers.ForEach(func(key serviceKey, cfg *VirtualServerConfig) {
		if key.Namespace == vsKey.Namespace &&
			key.ServiceName == vsKey.ServiceName {
			vsMap[cfg.VirtualServer.Backend.ServicePort] = cfg
		}
	})
	return vsMap
}

func (appMgr *Manager) deleteUnusedVirtualServers(
	vsKey vsQueueKey,
	vsMap VirtualServerPortMap,
) int {
	vsDeleted := 0
	appMgr.vservers.Lock()
	defer appMgr.vservers.Unlock()
	for port, cfg := range vsMap {
		tmpKey := serviceKey{
			Namespace:   vsKey.Namespace,
			ServiceName: vsKey.ServiceName,
			ServicePort: port,
		}
		vsName := cfg.VirtualServer.Frontend.VirtualServerName
		if appMgr.vservers.Delete(tmpKey, vsName) {
			vsDeleted += 1
		}
	}
	return vsDeleted
}

func (appMgr *Manager) setBindAddrAnnotation(
	cm *v1.ConfigMap,
	vsKey vsQueueKey,
	vsCfg *VirtualServerConfig,
) {
	var doUpdate bool
	if cm.ObjectMeta.Annotations == nil {
		cm.ObjectMeta.Annotations = make(map[string]string)
		doUpdate = true
	} else if cm.ObjectMeta.Annotations[vsBindAddrAnnotation] !=
		vsCfg.VirtualServer.Frontend.VirtualAddress.BindAddr {
		doUpdate = true
	}
	if doUpdate {
		cm.ObjectMeta.Annotations[vsBindAddrAnnotation] =
			vsCfg.VirtualServer.Frontend.VirtualAddress.BindAddr
		_, err := appMgr.kubeClient.CoreV1().ConfigMaps(vsKey.Namespace).Update(cm)
		if nil != err {
			log.Warningf("Error when creating status IP annotation: %s", err)
		} else {
			log.Debugf("Updating ConfigMap %+v annotation - %v: %v",
				vsKey, vsBindAddrAnnotation,
				vsCfg.VirtualServer.Frontend.VirtualAddress.BindAddr)
		}
	}
}

func (appMgr *Manager) checkValidConfigMap(
	obj interface{},
) (bool, *vsQueueKey) {
	// Identify the specific service being referenced, and return it if it's
	// one we care about.
	cm := obj.(*v1.ConfigMap)
	namespace := cm.ObjectMeta.Namespace
	_, ok := appMgr.getNamespaceInformer(namespace)
	if !ok {
		// Not watching this namespace
		return false, nil
	}
	cfg, err := parseVirtualServerConfig(cm)
	if nil != err {
		if handleVirtualServerConfigParseFailure(appMgr, cm, cfg, err) {
			// vservers is updated if true is returned, write out the config.
			appMgr.outputConfig()
		}
		return false, nil
	}

	return true, &vsQueueKey{
		Namespace:   namespace,
		ServiceName: cfg.VirtualServer.Backend.ServiceName,
	}
}

func (appMgr *Manager) enqueueConfigMap(obj interface{}) {
	if ok, key := appMgr.checkValidConfigMap(obj); ok {
		appMgr.vsQueue.Add(*key)
	}
}

func (appMgr *Manager) checkValidService(
	obj interface{},
) (bool, *vsQueueKey) {
	// Check if the service to see if we care about it.
	svc := obj.(*v1.Service)
	namespace := svc.ObjectMeta.Namespace
	_, ok := appMgr.getNamespaceInformer(namespace)
	if !ok {
		// Not watching this namespace
		return false, nil
	}
	return true, &vsQueueKey{
		Namespace:   namespace,
		ServiceName: svc.ObjectMeta.Name,
	}
}

func (appMgr *Manager) enqueueService(obj interface{}) {
	if ok, key := appMgr.checkValidService(obj); ok {
		appMgr.vsQueue.Add(*key)
	}
}

func (appMgr *Manager) checkValidEndpoints(
	obj interface{},
) (bool, *vsQueueKey) {
	eps := obj.(*v1.Endpoints)
	namespace := eps.ObjectMeta.Namespace
	// Check if the service to see if we care about it.
	_, ok := appMgr.getNamespaceInformer(namespace)
	if !ok {
		// Not watching this namespace
		return false, nil
	}
	return true, &vsQueueKey{
		Namespace:   namespace,
		ServiceName: eps.ObjectMeta.Name,
	}
}

func (appMgr *Manager) enqueueEndpoints(obj interface{}) {
	if ok, key := appMgr.checkValidEndpoints(obj); ok {
		appMgr.vsQueue.Add(*key)
	}
}

func getEndpointsForService(
	portName string,
	eps *v1.Endpoints,
) []string {
	var ipPorts []string

	if eps == nil {
		return ipPorts
	}

	for _, subset := range eps.Subsets {
		for _, p := range subset.Ports {
			if portName == p.Name {
				port := strconv.Itoa(int(p.Port))
				for _, addr := range subset.Addresses {
					var b bytes.Buffer
					b.WriteString(addr.IP)
					b.WriteRune(':')
					b.WriteString(port)
					ipPorts = append(ipPorts, b.String())
				}
			}
		}
	}
	if 0 != len(ipPorts) {
		sort.Strings(ipPorts)
	}
	return ipPorts
}

func (appMgr *Manager) getEndpointsForNodePort(
	nodePort int32,
) []string {
	port := strconv.Itoa(int(nodePort))
	nodes := appMgr.getNodesFromCache()
	for i, v := range nodes {
		var b bytes.Buffer
		b.WriteString(v)
		b.WriteRune(':')
		b.WriteString(port)
		nodes[i] = b.String()
	}

	return nodes
}

func handleVirtualServerConfigParseFailure(
	appMgr *Manager,
	cm *v1.ConfigMap,
	cfg *VirtualServerConfig,
	err error,
) bool {
	log.Warningf("Could not get config for ConfigMap: %v - %v",
		cm.ObjectMeta.Name, err)
	// If virtual server exists for invalid configmap, delete it
	if nil != cfg {
		serviceName := cfg.VirtualServer.Backend.ServiceName
		servicePort := cfg.VirtualServer.Backend.ServicePort
		vsKey := serviceKey{serviceName, servicePort, cm.ObjectMeta.Namespace}
		vsName := formatVirtualServerName(cm)
		if _, ok := appMgr.vservers.Get(vsKey, vsName); ok {
			appMgr.vservers.Lock()
			defer appMgr.vservers.Unlock()
			appMgr.vservers.Delete(vsKey, vsName)
			delete(cm.ObjectMeta.Annotations, vsBindAddrAnnotation)
			appMgr.kubeClient.CoreV1().ConfigMaps(cm.ObjectMeta.Namespace).Update(cm)
			log.Warningf("Deleted virtual server associated with ConfigMap: %v",
				cm.ObjectMeta.Name)
			return true
		}
	}
	return false
}

// Check for a change in Node state
func (appMgr *Manager) ProcessNodeUpdate(
	obj interface{}, err error,
) {
	if nil != err {
		log.Warningf("Unable to get list of nodes, err=%+v", err)
		return
	}

	newNodes, err := appMgr.getNodeAddresses(obj)
	if nil != err {
		log.Warningf("Unable to get list of nodes, err=%+v", err)
		return
	}
	sort.Strings(newNodes)

	appMgr.vservers.Lock()
	defer appMgr.vservers.Unlock()
	appMgr.oldNodesMutex.Lock()
	defer appMgr.oldNodesMutex.Unlock()
	// Compare last set of nodes with new one
	if !reflect.DeepEqual(newNodes, appMgr.oldNodes) {
		log.Infof("ProcessNodeUpdate: Change in Node state detected")
		appMgr.vservers.ForEach(func(key serviceKey, cfg *VirtualServerConfig) {
			port := strconv.Itoa(int(cfg.MetaData.NodePort))
			var newAddrPorts []string
			for _, node := range newNodes {
				var b bytes.Buffer
				b.WriteString(node)
				b.WriteRune(':')
				b.WriteString(port)
				newAddrPorts = append(newAddrPorts, b.String())
			}
			cfg.VirtualServer.Backend.PoolMemberAddrs = newAddrPorts
		})
		// Output the Big-IP config
		appMgr.outputConfigLocked()

		// Update node cache
		appMgr.oldNodes = newNodes
	}
}

// Dump out the Virtual Server configs to a file
func (appMgr *Manager) outputConfig() {
	appMgr.vservers.Lock()
	appMgr.outputConfigLocked()
	appMgr.vservers.Unlock()
}

// Dump out the Virtual Server configs to a file
// This function MUST be called with the virtualServers
// lock held.
func (appMgr *Manager) outputConfigLocked() {

	// Initialize the Services array as empty; json.Marshal() writes
	// an uninitialized array as 'null', but we want an empty array
	// written as '[]' instead
	services := VirtualServerConfigs{}

	// Filter the configs to only those that have active services
	appMgr.vservers.ForEach(func(key serviceKey, cfg *VirtualServerConfig) {
		if cfg.MetaData.Active == true {
			services = append(services, cfg)
		}
	})

	doneCh, errCh, err := appMgr.ConfigWriter().SendSection("services", services)
	if nil != err {
		log.Warningf("Failed to write Big-IP config data: %v", err)
	} else {
		select {
		case <-doneCh:
			log.Infof("Wrote %v Virtual Server configs", len(services))
			if log.LL_DEBUG == log.GetLogLevel() {
				output, err := json.Marshal(services)
				if nil != err {
					log.Warningf("Failed creating output debug log: %v", err)
				} else {
					log.Debugf("Services: %s", output)
				}
			}
		case e := <-errCh:
			log.Warningf("Failed to write Big-IP config data: %v", e)
		case <-time.After(time.Second):
			log.Warning("Did not receive config write response in 1s")
		}
	}
}

// Return a copy of the node cache
func (appMgr *Manager) getNodesFromCache() []string {
	appMgr.oldNodesMutex.Lock()
	defer appMgr.oldNodesMutex.Unlock()
	nodes := make([]string, len(appMgr.oldNodes))
	copy(nodes, appMgr.oldNodes)

	return nodes
}

// Get a list of Node addresses
func (appMgr *Manager) getNodeAddresses(
	obj interface{},
) ([]string, error) {
	nodes, ok := obj.([]v1.Node)
	if false == ok {
		return nil,
			fmt.Errorf("poll update unexpected type, interface is not []v1.Node")
	}

	addrs := []string{}

	var addrType v1.NodeAddressType
	if appMgr.UseNodeInternal() {
		addrType = v1.NodeInternalIP
	} else {
		addrType = v1.NodeExternalIP
	}

	for _, node := range nodes {
		if node.Spec.Unschedulable {
			// Skip master node
			continue
		} else {
			nodeAddrs := node.Status.Addresses
			for _, addr := range nodeAddrs {
				if addr.Type == addrType {
					addrs = append(addrs, addr.Address)
				}
			}
		}
	}

	return addrs, nil
}
