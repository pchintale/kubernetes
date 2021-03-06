/*
Copyright 2014 The Kubernetes Authors All rights reserved.

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

package master

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/latest"
	"k8s.io/kubernetes/pkg/api/testapi"
	"k8s.io/kubernetes/pkg/api/unversioned"
	apiutil "k8s.io/kubernetes/pkg/api/util"
	"k8s.io/kubernetes/pkg/api/v1"
	"k8s.io/kubernetes/pkg/apis/extensions"
	"k8s.io/kubernetes/pkg/apiserver"
	"k8s.io/kubernetes/pkg/kubelet/client"
	"k8s.io/kubernetes/pkg/registry/endpoint"
	"k8s.io/kubernetes/pkg/registry/namespace"
	"k8s.io/kubernetes/pkg/registry/registrytest"
	thirdpartyresourcedatastorage "k8s.io/kubernetes/pkg/registry/thirdpartyresourcedata/etcd"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/storage"
	etcdstorage "k8s.io/kubernetes/pkg/storage/etcd"
	"k8s.io/kubernetes/pkg/storage/etcd/etcdtest"
	etcdtesting "k8s.io/kubernetes/pkg/storage/etcd/testing"
	"k8s.io/kubernetes/pkg/util"
	"k8s.io/kubernetes/pkg/util/intstr"

	"github.com/emicklei/go-restful"
	"github.com/stretchr/testify/assert"
	"golang.org/x/net/context"
)

// setUp is a convience function for setting up for (most) tests.
func setUp(t *testing.T) (Master, *etcdtesting.EtcdTestServer, Config, *assert.Assertions) {
	server := etcdtesting.NewEtcdTestClientServer(t)

	master := Master{}
	config := Config{}
	storageVersions := make(map[string]string)
	storageDestinations := NewStorageDestinations()
	storageDestinations.AddAPIGroup(
		api.GroupName, etcdstorage.NewEtcdStorage(server.Client, testapi.Default.Codec(), etcdtest.PathPrefix()))
	storageDestinations.AddAPIGroup(
		extensions.GroupName, etcdstorage.NewEtcdStorage(server.Client, testapi.Extensions.Codec(), etcdtest.PathPrefix()))
	config.StorageDestinations = storageDestinations
	storageVersions[api.GroupName] = testapi.Default.GroupVersion().String()
	storageVersions[extensions.GroupName] = testapi.Extensions.GroupVersion().String()
	config.StorageVersions = storageVersions
	config.PublicAddress = net.ParseIP("192.168.10.4")
	master.nodeRegistry = registrytest.NewNodeRegistry([]string{"node1", "node2"}, api.NodeResources{})

	return master, server, config, assert.New(t)
}

// TestNew verifies that the New function returns a Master
// using the configuration properly.
func TestNew(t *testing.T) {
	_, etcdserver, config, assert := setUp(t)
	defer etcdserver.Terminate(t)

	config.KubeletClient = client.FakeKubeletClient{}

	config.ProxyDialer = func(network, addr string) (net.Conn, error) { return nil, nil }
	config.ProxyTLSClientConfig = &tls.Config{}

	master := New(&config)

	// Verify many of the variables match their config counterparts
	assert.Equal(master.enableCoreControllers, config.EnableCoreControllers)
	assert.Equal(master.enableLogsSupport, config.EnableLogsSupport)
	assert.Equal(master.enableUISupport, config.EnableUISupport)
	assert.Equal(master.enableSwaggerSupport, config.EnableSwaggerSupport)
	assert.Equal(master.enableSwaggerSupport, config.EnableSwaggerSupport)
	assert.Equal(master.enableProfiling, config.EnableProfiling)
	assert.Equal(master.apiPrefix, config.APIPrefix)
	assert.Equal(master.apiGroupPrefix, config.APIGroupPrefix)
	assert.Equal(master.corsAllowedOriginList, config.CorsAllowedOriginList)
	assert.Equal(master.authenticator, config.Authenticator)
	assert.Equal(master.authorizer, config.Authorizer)
	assert.Equal(master.admissionControl, config.AdmissionControl)
	assert.Equal(master.apiGroupVersionOverrides, config.APIGroupVersionOverrides)
	assert.Equal(master.requestContextMapper, config.RequestContextMapper)
	assert.Equal(master.cacheTimeout, config.CacheTimeout)
	assert.Equal(master.masterCount, config.MasterCount)
	assert.Equal(master.externalHost, config.ExternalHost)
	assert.Equal(master.clusterIP, config.PublicAddress)
	assert.Equal(master.publicReadWritePort, config.ReadWritePort)
	assert.Equal(master.serviceReadWriteIP, config.ServiceReadWriteIP)
	assert.Equal(master.tunneler, config.Tunneler)

	// These functions should point to the same memory location
	masterDialer, _ := util.Dialer(master.proxyTransport)
	masterDialerFunc := fmt.Sprintf("%p", masterDialer)
	configDialerFunc := fmt.Sprintf("%p", config.ProxyDialer)
	assert.Equal(masterDialerFunc, configDialerFunc)

	assert.Equal(master.proxyTransport.(*http.Transport).TLSClientConfig, config.ProxyTLSClientConfig)
}

// TestGetServersToValidate verifies the unexported getServersToValidate function
func TestGetServersToValidate(t *testing.T) {
	master, etcdserver, config, assert := setUp(t)
	defer etcdserver.Terminate(t)

	servers := master.getServersToValidate(&config)

	// Expected servers to validate: scheduler, controller-manager and etcd.
	assert.Equal(3, len(servers), "unexpected server list: %#v", servers)

	for _, server := range []string{"scheduler", "controller-manager", "etcd-0"} {
		if _, ok := servers[server]; !ok {
			t.Errorf("server list missing: %s", server)
		}
	}
}

// TestFindExternalAddress verifies both pass and fail cases for the unexported
// findExternalAddress function
func TestFindExternalAddress(t *testing.T) {
	assert := assert.New(t)
	expectedIP := "172.0.0.1"

	nodes := []*api.Node{new(api.Node), new(api.Node), new(api.Node)}
	nodes[0].Status.Addresses = []api.NodeAddress{{"ExternalIP", expectedIP}}
	nodes[1].Status.Addresses = []api.NodeAddress{{"LegacyHostIP", expectedIP}}
	nodes[2].Status.Addresses = []api.NodeAddress{{"ExternalIP", expectedIP}, {"LegacyHostIP", "172.0.0.2"}}

	// Pass Case
	for _, node := range nodes {
		ip, err := findExternalAddress(node)
		assert.NoError(err, "error getting node external address")
		assert.Equal(expectedIP, ip, "expected ip to be %s, but was %s", expectedIP, ip)
	}

	// Fail case
	_, err := findExternalAddress(new(api.Node))
	assert.Error(err, "expected findExternalAddress to fail on a node with missing ip information")
}

// TestApi_v1 verifies that the unexported api_v1 function does indeed
// utilize the correct Version and Codec.
func TestApi_v1(t *testing.T) {
	master, etcdserver, _, assert := setUp(t)
	defer etcdserver.Terminate(t)

	version := master.api_v1()
	assert.Equal(unversioned.GroupVersion{Version: "v1"}, version.GroupVersion, "Version was not v1: %s", version.GroupVersion)
	assert.Equal(v1.Codec, version.Codec, "version.Codec was not for v1: %s", version.Codec)
	for k, v := range master.storage {
		assert.Contains(version.Storage, v, "Value %s not found (key: %s)", k, v)
	}
}

// TestNewBootstrapController verifies master fields are properly copied into controller
func TestNewBootstrapController(t *testing.T) {
	// Tests a subset of inputs to ensure they are set properly in the controller
	master, etcdserver, _, assert := setUp(t)
	defer etcdserver.Terminate(t)

	portRange := util.PortRange{Base: 10, Size: 10}

	master.namespaceRegistry = namespace.NewRegistry(nil)
	master.serviceRegistry = registrytest.NewServiceRegistry()
	master.endpointRegistry = endpoint.NewRegistry(nil)

	master.serviceNodePortRange = portRange
	master.masterCount = 1
	master.serviceReadWritePort = 1000
	master.publicReadWritePort = 1010

	controller := master.NewBootstrapController()

	assert.Equal(controller.NamespaceRegistry, master.namespaceRegistry)
	assert.Equal(controller.EndpointRegistry, master.endpointRegistry)
	assert.Equal(controller.ServiceRegistry, master.serviceRegistry)
	assert.Equal(controller.ServiceNodePortRange, portRange)
	assert.Equal(controller.MasterCount, master.masterCount)
	assert.Equal(controller.ServicePort, master.serviceReadWritePort)
	assert.Equal(controller.PublicServicePort, master.publicReadWritePort)
}

// TestControllerServicePorts verifies master extraServicePorts are
// correctly copied into controller
func TestControllerServicePorts(t *testing.T) {
	master, etcdserver, _, assert := setUp(t)
	defer etcdserver.Terminate(t)

	master.namespaceRegistry = namespace.NewRegistry(nil)
	master.serviceRegistry = registrytest.NewServiceRegistry()
	master.endpointRegistry = endpoint.NewRegistry(nil)

	master.extraServicePorts = []api.ServicePort{
		{
			Name:       "additional-port-1",
			Port:       1000,
			Protocol:   api.ProtocolTCP,
			TargetPort: intstr.FromInt(1000),
		},
		{
			Name:       "additional-port-2",
			Port:       1010,
			Protocol:   api.ProtocolTCP,
			TargetPort: intstr.FromInt(1010),
		},
	}

	controller := master.NewBootstrapController()

	assert.Equal(1000, controller.ExtraServicePorts[0].Port)
	assert.Equal(1010, controller.ExtraServicePorts[1].Port)
}

// TestNewHandlerContainer verifies that NewHandlerContainer uses the
// mux provided
func TestNewHandlerContainer(t *testing.T) {
	assert := assert.New(t)
	mux := http.NewServeMux()
	container := NewHandlerContainer(mux)
	assert.Equal(mux, container.ServeMux, "ServerMux's do not match")
}

// TestHandleWithAuth verifies HandleWithAuth adds the path
// to the muxHelper.RegisteredPaths.
func TestHandleWithAuth(t *testing.T) {
	master, etcdserver, _, assert := setUp(t)
	defer etcdserver.Terminate(t)

	mh := apiserver.MuxHelper{Mux: http.NewServeMux()}
	master.muxHelper = &mh
	handler := func(r http.ResponseWriter, w *http.Request) { w.Write(nil) }
	master.HandleWithAuth("/test", http.HandlerFunc(handler))

	assert.Contains(master.muxHelper.RegisteredPaths, "/test", "Path not found in muxHelper")
}

// TestHandleFuncWithAuth verifies HandleFuncWithAuth adds the path
// to the muxHelper.RegisteredPaths.
func TestHandleFuncWithAuth(t *testing.T) {
	master, etcdserver, _, assert := setUp(t)
	defer etcdserver.Terminate(t)

	mh := apiserver.MuxHelper{Mux: http.NewServeMux()}
	master.muxHelper = &mh
	handler := func(r http.ResponseWriter, w *http.Request) { w.Write(nil) }
	master.HandleFuncWithAuth("/test", handler)

	assert.Contains(master.muxHelper.RegisteredPaths, "/test", "Path not found in muxHelper")
}

// TestInstallSwaggerAPI verifies that the swagger api is added
// at the proper endpoint.
func TestInstallSwaggerAPI(t *testing.T) {
	master, etcdserver, _, assert := setUp(t)
	defer etcdserver.Terminate(t)

	mux := http.NewServeMux()
	master.handlerContainer = NewHandlerContainer(mux)

	// Ensure swagger isn't installed without the call
	ws := master.handlerContainer.RegisteredWebServices()
	if !assert.Equal(len(ws), 0) {
		for x := range ws {
			assert.NotEqual("/swaggerapi", ws[x].RootPath(), "SwaggerAPI was installed without a call to InstallSwaggerAPI()")
		}
	}

	// Install swagger and test
	master.InstallSwaggerAPI()
	ws = master.handlerContainer.RegisteredWebServices()
	if assert.NotEqual(0, len(ws), "SwaggerAPI not installed.") {
		assert.Equal("/swaggerapi/", ws[0].RootPath(), "SwaggerAPI did not install to the proper path. %s != /swaggerapi", ws[0].RootPath())
	}

	// Empty externalHost verification
	mux = http.NewServeMux()
	master.handlerContainer = NewHandlerContainer(mux)
	master.externalHost = ""
	master.clusterIP = net.IPv4(10, 10, 10, 10)
	master.publicReadWritePort = 1010
	master.InstallSwaggerAPI()
	if assert.NotEqual(0, len(ws), "SwaggerAPI not installed.") {
		assert.Equal("/swaggerapi/", ws[0].RootPath(), "SwaggerAPI did not install to the proper path. %s != /swaggerapi", ws[0].RootPath())
	}
}

// TestDefaultAPIGroupVersion verifies that the unexported defaultAPIGroupVersion
// creates the expected APIGroupVersion based off of master.
func TestDefaultAPIGroupVersion(t *testing.T) {
	master, etcdserver, _, assert := setUp(t)
	defer etcdserver.Terminate(t)

	apiGroup := master.defaultAPIGroupVersion()

	assert.Equal(apiGroup.Root, master.apiPrefix)
	assert.Equal(apiGroup.Admit, master.admissionControl)
	assert.Equal(apiGroup.Context, master.requestContextMapper)
	assert.Equal(apiGroup.MinRequestTimeout, master.minRequestTimeout)
}

// TestExpapi verifies that the unexported exapi creates
// the an experimental unversioned.APIGroupVersion.
func TestExpapi(t *testing.T) {
	master, etcdserver, config, assert := setUp(t)
	defer etcdserver.Terminate(t)

	extensionsGroupMeta := latest.GroupOrDie(extensions.GroupName)

	expAPIGroup := master.experimental(&config)
	assert.Equal(expAPIGroup.Root, master.apiGroupPrefix)
	assert.Equal(expAPIGroup.Mapper, extensionsGroupMeta.RESTMapper)
	assert.Equal(expAPIGroup.Codec, extensionsGroupMeta.Codec)
	assert.Equal(expAPIGroup.Linker, extensionsGroupMeta.SelfLinker)
	assert.Equal(expAPIGroup.GroupVersion, extensionsGroupMeta.GroupVersion)
}

// TestGetNodeAddresses verifies that proper results are returned
// when requesting node addresses.
func TestGetNodeAddresses(t *testing.T) {
	master, etcdserver, _, assert := setUp(t)
	defer etcdserver.Terminate(t)

	// Fail case (no addresses associated with nodes)
	nodes, _ := master.nodeRegistry.ListNodes(api.NewDefaultContext(), nil)
	addrs, err := master.getNodeAddresses()

	assert.Error(err, "getNodeAddresses should have caused an error as there are no addresses.")
	assert.Equal([]string(nil), addrs)

	// Pass case with External type IP
	nodes, _ = master.nodeRegistry.ListNodes(api.NewDefaultContext(), nil)
	for index := range nodes.Items {
		nodes.Items[index].Status.Addresses = []api.NodeAddress{{Type: api.NodeExternalIP, Address: "127.0.0.1"}}
	}
	addrs, err = master.getNodeAddresses()
	assert.NoError(err, "getNodeAddresses should not have returned an error.")
	assert.Equal([]string{"127.0.0.1", "127.0.0.1"}, addrs)

	// Pass case with LegacyHost type IP
	nodes, _ = master.nodeRegistry.ListNodes(api.NewDefaultContext(), nil)
	for index := range nodes.Items {
		nodes.Items[index].Status.Addresses = []api.NodeAddress{{Type: api.NodeLegacyHostIP, Address: "127.0.0.2"}}
	}
	addrs, err = master.getNodeAddresses()
	assert.NoError(err, "getNodeAddresses failback should not have returned an error.")
	assert.Equal([]string{"127.0.0.2", "127.0.0.2"}, addrs)
}

func TestDiscoveryAtAPIS(t *testing.T) {
	master, etcdserver, config, assert := setUp(t)
	defer etcdserver.Terminate(t)

	// ================= preparation for master.init() ======================
	portRange := util.PortRange{Base: 10, Size: 10}
	master.serviceNodePortRange = portRange

	_, ipnet, err := net.ParseCIDR("192.168.1.1/24")
	if !assert.NoError(err) {
		t.Errorf("unexpected error: %v", err)
	}
	master.serviceClusterIPRange = ipnet

	mh := apiserver.MuxHelper{Mux: http.NewServeMux()}
	master.muxHelper = &mh
	master.rootWebService = new(restful.WebService)

	master.handlerContainer = restful.NewContainer()

	master.mux = http.NewServeMux()
	master.requestContextMapper = api.NewRequestContextMapper()
	// ======================= end of preparation ===========================

	master.init(&config)
	server := httptest.NewServer(master.handlerContainer.ServeMux)
	resp, err := http.Get(server.URL + "/apis")
	if !assert.NoError(err) {
		t.Errorf("unexpected error: %v", err)
	}

	assert.Equal(http.StatusOK, resp.StatusCode)

	groupList := unversioned.APIGroupList{}
	assert.NoError(decodeResponse(resp, &groupList))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectGroupName := extensions.GroupName
	expectVersions := []unversioned.GroupVersionForDiscovery{
		{
			GroupVersion: testapi.Extensions.GroupVersion().String(),
			Version:      testapi.Extensions.GroupVersion().Version,
		},
	}
	expectPreferredVersion := unversioned.GroupVersionForDiscovery{
		GroupVersion: config.StorageVersions[extensions.GroupName],
		Version:      apiutil.GetVersion(config.StorageVersions[extensions.GroupName]),
	}
	assert.Equal(expectGroupName, groupList.Groups[0].Name)
	assert.Equal(expectVersions, groupList.Groups[0].Versions)
	assert.Equal(expectPreferredVersion, groupList.Groups[0].PreferredVersion)
}

var versionsToTest = []string{"v1", "v3"}

type Foo struct {
	unversioned.TypeMeta `json:",inline"`
	api.ObjectMeta       `json:"metadata,omitempty" description:"standard object metadata"`

	SomeField  string `json:"someField"`
	OtherField int    `json:"otherField"`
}

type FooList struct {
	unversioned.TypeMeta `json:",inline"`
	unversioned.ListMeta `json:"metadata,omitempty" description:"standard list metadata; see http://releases.k8s.io/HEAD/docs/devel/api-conventions.md#metadata"`

	Items []Foo `json:"items"`
}

func initThirdParty(t *testing.T, version string) (*Master, *etcdtesting.EtcdTestServer, *httptest.Server, *assert.Assertions) {
	master, etcdserver, _, assert := setUp(t)

	master.thirdPartyResources = map[string]*thirdpartyresourcedatastorage.REST{}
	api := &extensions.ThirdPartyResource{
		ObjectMeta: api.ObjectMeta{
			Name: "foo.company.com",
		},
		Versions: []extensions.APIVersion{
			{
				APIGroup: "group",
				Name:     version,
			},
		},
	}
	master.handlerContainer = restful.NewContainer()
	master.thirdPartyStorage = etcdstorage.NewEtcdStorage(etcdserver.Client, testapi.Extensions.Codec(), etcdtest.PathPrefix())

	if !assert.NoError(master.InstallThirdPartyResource(api)) {
		t.FailNow()
	}

	server := httptest.NewServer(master.handlerContainer.ServeMux)
	return &master, etcdserver, server, assert
}

func TestInstallThirdPartyAPIList(t *testing.T) {
	for _, version := range versionsToTest {
		testInstallThirdPartyAPIListVersion(t, version)
	}
}

func testInstallThirdPartyAPIListVersion(t *testing.T, version string) {
	tests := []struct {
		items []Foo
	}{
		{},
		{
			items: []Foo{},
		},
		{
			items: []Foo{
				{
					ObjectMeta: api.ObjectMeta{
						Name: "test",
					},
					TypeMeta: unversioned.TypeMeta{
						Kind:       "Foo",
						APIVersion: version,
					},
					SomeField:  "test field",
					OtherField: 10,
				},
				{
					ObjectMeta: api.ObjectMeta{
						Name: "bar",
					},
					TypeMeta: unversioned.TypeMeta{
						Kind:       "Foo",
						APIVersion: version,
					},
					SomeField:  "test field another",
					OtherField: 20,
				},
			},
		},
	}
	for _, test := range tests {
		func() {
			master, etcdserver, server, assert := initThirdParty(t, version)
			defer server.Close()
			defer etcdserver.Terminate(t)

			if test.items != nil {
				storeThirdPartyList(master.thirdPartyStorage, "/ThirdPartyResourceData/company.com/foos/default", test.items)
			}

			resp, err := http.Get(server.URL + "/apis/company.com/" + version + "/namespaces/default/foos")
			if !assert.NoError(err) {
				return
			}
			defer resp.Body.Close()

			assert.Equal(http.StatusOK, resp.StatusCode)

			data, err := ioutil.ReadAll(resp.Body)
			assert.NoError(err)

			list := FooList{}
			if err = json.Unmarshal(data, &list); err != nil {
				t.Errorf("unexpected error: %v", err)
			}

			if test.items == nil {
				if len(list.Items) != 0 {
					t.Errorf("expected no items, saw: %v", list.Items)
				}
				return
			}

			if len(list.Items) != len(test.items) {
				t.Errorf("unexpected length: %d vs %d", len(list.Items), len(test.items))
				return
			}
			// The order of elements in LIST is not guaranteed.
			mapping := make(map[string]int)
			for ix := range test.items {
				mapping[test.items[ix].Name] = ix
			}
			for ix := range list.Items {
				// Copy things that are set dynamically on the server
				expectedObj := test.items[mapping[list.Items[ix].Name]]
				expectedObj.SelfLink = list.Items[ix].SelfLink
				expectedObj.ResourceVersion = list.Items[ix].ResourceVersion
				expectedObj.Namespace = list.Items[ix].Namespace
				expectedObj.UID = list.Items[ix].UID
				expectedObj.CreationTimestamp = list.Items[ix].CreationTimestamp

				// We endure the order of items by sorting them (using 'mapping')
				// so that this function passes.
				if !reflect.DeepEqual(list.Items[ix], expectedObj) {
					t.Errorf("expected:\n%#v\nsaw:\n%#v\n", expectedObj, list.Items[ix])
				}
			}
		}()
	}
}

func encodeToThirdParty(name string, obj interface{}) (runtime.Object, error) {
	serial, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	thirdPartyData := extensions.ThirdPartyResourceData{
		ObjectMeta: api.ObjectMeta{Name: name},
		Data:       serial,
	}
	return &thirdPartyData, nil
}

func storeThirdPartyObject(s storage.Interface, path, name string, obj interface{}) error {
	data, err := encodeToThirdParty(name, obj)
	if err != nil {
		return err
	}
	return s.Set(context.TODO(), etcdtest.AddPrefix(path), data, nil, 0)
}

func storeThirdPartyList(s storage.Interface, path string, list []Foo) error {
	for _, obj := range list {
		if err := storeThirdPartyObject(s, path+"/"+obj.Name, obj.Name, obj); err != nil {
			return err
		}
	}
	return nil
}

func decodeResponse(resp *http.Response, obj interface{}) error {
	defer resp.Body.Close()

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if err := json.Unmarshal(data, obj); err != nil {
		return err
	}
	return nil
}

func TestInstallThirdPartyAPIGet(t *testing.T) {
	for _, version := range versionsToTest {
		testInstallThirdPartyAPIGetVersion(t, version)
	}
}

func testInstallThirdPartyAPIGetVersion(t *testing.T, version string) {
	master, etcdserver, server, assert := initThirdParty(t, version)
	defer server.Close()
	defer etcdserver.Terminate(t)

	expectedObj := Foo{
		ObjectMeta: api.ObjectMeta{
			Name: "test",
		},
		TypeMeta: unversioned.TypeMeta{
			Kind:       "Foo",
			APIVersion: version,
		},
		SomeField:  "test field",
		OtherField: 10,
	}
	if !assert.NoError(storeThirdPartyObject(master.thirdPartyStorage, "/ThirdPartyResourceData/company.com/foos/default/test", "test", expectedObj)) {
		t.FailNow()
		return
	}

	resp, err := http.Get(server.URL + "/apis/company.com/" + version + "/namespaces/default/foos/test")
	if !assert.NoError(err) {
		return
	}

	assert.Equal(http.StatusOK, resp.StatusCode)

	item := Foo{}
	assert.NoError(decodeResponse(resp, &item))
	if !assert.False(reflect.DeepEqual(item, expectedObj)) {
		t.Errorf("expected objects to not be equal:\n%v\nsaw:\n%v\n", expectedObj, item)
	}
	// Fill in data that the apiserver injects
	expectedObj.SelfLink = item.SelfLink
	expectedObj.ResourceVersion = item.ResourceVersion
	if !assert.True(reflect.DeepEqual(item, expectedObj)) {
		t.Errorf("expected:\n%#v\nsaw:\n%#v\n", expectedObj, item)
	}
}

func TestInstallThirdPartyAPIPost(t *testing.T) {
	for _, version := range versionsToTest {
		testInstallThirdPartyAPIPostForVersion(t, version)
	}
}

func testInstallThirdPartyAPIPostForVersion(t *testing.T, version string) {
	master, etcdserver, server, assert := initThirdParty(t, version)
	defer server.Close()
	defer etcdserver.Terminate(t)

	inputObj := Foo{
		ObjectMeta: api.ObjectMeta{
			Name: "test",
		},
		TypeMeta: unversioned.TypeMeta{
			Kind:       "Foo",
			APIVersion: "company.com/" + version,
		},
		SomeField:  "test field",
		OtherField: 10,
	}
	data, err := json.Marshal(inputObj)
	if !assert.NoError(err) {
		return
	}

	resp, err := http.Post(server.URL+"/apis/company.com/"+version+"/namespaces/default/foos", "application/json", bytes.NewBuffer(data))
	if !assert.NoError(err) {
		t.Errorf("unexpected error: %v", err)
		return
	}

	assert.Equal(http.StatusCreated, resp.StatusCode)

	item := Foo{}
	assert.NoError(decodeResponse(resp, &item))

	// fill in fields set by the apiserver
	expectedObj := inputObj
	expectedObj.SelfLink = item.SelfLink
	expectedObj.ResourceVersion = item.ResourceVersion
	expectedObj.Namespace = item.Namespace
	expectedObj.UID = item.UID
	expectedObj.CreationTimestamp = item.CreationTimestamp
	if !assert.True(reflect.DeepEqual(item, expectedObj)) {
		t.Errorf("expected:\n%v\nsaw:\n%v\n", expectedObj, item)
	}

	thirdPartyObj := extensions.ThirdPartyResourceData{}
	err = master.thirdPartyStorage.Get(
		context.TODO(), etcdtest.AddPrefix("/ThirdPartyResourceData/company.com/foos/default/test"),
		&thirdPartyObj, false)
	if !assert.NoError(err) {
		t.FailNow()
	}

	item = Foo{}
	assert.NoError(json.Unmarshal(thirdPartyObj.Data, &item))

	if !assert.True(reflect.DeepEqual(item, inputObj)) {
		t.Errorf("expected:\n%v\nsaw:\n%v\n", inputObj, item)
	}
}

func TestInstallThirdPartyAPIDelete(t *testing.T) {
	for _, version := range versionsToTest {
		testInstallThirdPartyAPIDeleteVersion(t, version)
	}
}

func testInstallThirdPartyAPIDeleteVersion(t *testing.T, version string) {
	master, etcdserver, server, assert := initThirdParty(t, version)
	defer server.Close()
	defer etcdserver.Terminate(t)

	expectedObj := Foo{
		ObjectMeta: api.ObjectMeta{
			Name:      "test",
			Namespace: "default",
		},
		TypeMeta: unversioned.TypeMeta{
			Kind: "Foo",
		},
		SomeField:  "test field",
		OtherField: 10,
	}
	if !assert.NoError(storeThirdPartyObject(master.thirdPartyStorage, "/ThirdPartyResourceData/company.com/foos/default/test", "test", expectedObj)) {
		t.FailNow()
		return
	}

	resp, err := http.Get(server.URL + "/apis/company.com/" + version + "/namespaces/default/foos/test")
	if !assert.NoError(err) {
		return
	}

	assert.Equal(http.StatusOK, resp.StatusCode)

	item := Foo{}
	assert.NoError(decodeResponse(resp, &item))

	// Fill in fields set by the apiserver
	expectedObj.SelfLink = item.SelfLink
	expectedObj.ResourceVersion = item.ResourceVersion
	expectedObj.Namespace = item.Namespace
	if !assert.True(reflect.DeepEqual(item, expectedObj)) {
		t.Errorf("expected:\n%v\nsaw:\n%v\n", expectedObj, item)
	}

	resp, err = httpDelete(server.URL + "/apis/company.com/" + version + "/namespaces/default/foos/test")
	if !assert.NoError(err) {
		return
	}

	assert.Equal(http.StatusOK, resp.StatusCode)

	resp, err = http.Get(server.URL + "/apis/company.com/" + version + "/namespaces/default/foos/test")
	if !assert.NoError(err) {
		return
	}

	assert.Equal(http.StatusNotFound, resp.StatusCode)

	expectedDeletedKey := etcdtest.AddPrefix("ThirdPartyResourceData/company.com/foos/default/test")
	thirdPartyObj := extensions.ThirdPartyResourceData{}
	err = master.thirdPartyStorage.Get(
		context.TODO(), expectedDeletedKey, &thirdPartyObj, false)
	if !storage.IsNotFound(err) {
		t.Errorf("expected deletion didn't happen: %v", err)
	}
}

func httpDelete(url string) (*http.Response, error) {
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{}
	return client.Do(req)
}

func TestInstallThirdPartyResourceRemove(t *testing.T) {
	for _, version := range versionsToTest {
		testInstallThirdPartyResourceRemove(t, version)
	}
}

func testInstallThirdPartyResourceRemove(t *testing.T, version string) {
	master, etcdserver, server, assert := initThirdParty(t, version)
	defer server.Close()
	defer etcdserver.Terminate(t)

	expectedObj := Foo{
		ObjectMeta: api.ObjectMeta{
			Name: "test",
		},
		TypeMeta: unversioned.TypeMeta{
			Kind: "Foo",
		},
		SomeField:  "test field",
		OtherField: 10,
	}
	if !assert.NoError(storeThirdPartyObject(master.thirdPartyStorage, "/ThirdPartyResourceData/company.com/foos/default/test", "test", expectedObj)) {
		t.FailNow()
		return
	}
	secondObj := expectedObj
	secondObj.Name = "bar"
	if !assert.NoError(storeThirdPartyObject(master.thirdPartyStorage, "/ThirdPartyResourceData/company.com/foos/default/bar", "bar", secondObj)) {
		t.FailNow()
		return
	}

	resp, err := http.Get(server.URL + "/apis/company.com/" + version + "/namespaces/default/foos/test")
	if !assert.NoError(err) {
		t.FailNow()
		return
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("unexpected status: %v", resp)
	}

	item := Foo{}
	if err := decodeResponse(resp, &item); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	// TODO: validate etcd set things here
	item.ObjectMeta = expectedObj.ObjectMeta

	if !assert.True(reflect.DeepEqual(item, expectedObj)) {
		t.Errorf("expected:\n%v\nsaw:\n%v\n", expectedObj, item)
	}

	path := makeThirdPartyPath("company.com")
	master.RemoveThirdPartyResource(path)

	resp, err = http.Get(server.URL + "/apis/company.com/" + version + "/namespaces/default/foos/test")
	if !assert.NoError(err) {
		return
	}

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unexpected status: %v", resp)
	}

	expectedDeletedKeys := []string{
		etcdtest.AddPrefix("/ThirdPartyResourceData/company.com/foos/default/test"),
		etcdtest.AddPrefix("/ThirdPartyResourceData/company.com/foos/default/bar"),
	}
	for _, key := range expectedDeletedKeys {
		thirdPartyObj := extensions.ThirdPartyResourceData{}
		err := master.thirdPartyStorage.Get(context.TODO(), key, &thirdPartyObj, false)
		if !storage.IsNotFound(err) {
			t.Errorf("expected deletion didn't happen: %v", err)
		}
	}
	installed := master.ListThirdPartyResources()
	if len(installed) != 0 {
		t.Errorf("Resource(s) still installed: %v", installed)
	}
	services := master.handlerContainer.RegisteredWebServices()
	for ix := range services {
		if strings.HasPrefix(services[ix].RootPath(), "/apis/company.com") {
			t.Errorf("Web service still installed at %s: %#v", services[ix].RootPath(), services[ix])
		}
	}
}
