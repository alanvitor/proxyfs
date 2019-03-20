package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/swiftstack/ProxyFS/conf"
)

const (
	testAuthToken           = "AUTH_tkTestToken"
	testSwiftProxyAddr      = "localhost:38080"
	testJrpcResponseBufSize = 1024 * 1024
)

type testObjectStruct struct {
	sync.Mutex
	name     string
	contents []byte
}

type testContainerStruct struct {
	sync.Mutex
	name   string
	object map[string]*testObjectStruct // key == testObjectStruct.name
}

type testSwiftProxyEmulatorGlobalsStruct struct {
	t                   *testing.T
	ramswiftNoAuthURL   string
	proxyfsdJrpcTCPAddr *net.TCPAddr
	jrpcResponsePool    *sync.Pool
	httpServer          *http.Server

	// UNDO: These should go away when I'm no longer emulating ramswift & proxyfsd here
	sync.Mutex
	sync.WaitGroup
	container map[string]*testContainerStruct // key == testContainerStruct.name
}

var testSwiftProxyEmulatorGlobals testSwiftProxyEmulatorGlobalsStruct

func startSwiftProxyEmulator(t *testing.T, confMap conf.ConfMap) {
	var (
		err                      error
		jrpcServerIPAddr         string
		jrpcServerTCPPort        uint16
		swiftClientNoAuthIPAddr  string
		swiftClientNoAuthTCPPort uint16
		whoAmI                   string
	)

	testSwiftProxyEmulatorGlobals.t = t

	swiftClientNoAuthIPAddr, err = confMap.FetchOptionValueString("SwiftClient", "NoAuthIPAddr")
	if nil != err {
		t.Fatal(err)
	}

	swiftClientNoAuthTCPPort, err = confMap.FetchOptionValueUint16("SwiftClient", "NoAuthTCPPort")
	if nil != err {
		t.Fatal(err)
	}

	testSwiftProxyEmulatorGlobals.ramswiftNoAuthURL = "http://" + net.JoinHostPort(swiftClientNoAuthIPAddr, strconv.FormatUint(uint64(swiftClientNoAuthTCPPort), 10)) + "/"

	whoAmI, err = confMap.FetchOptionValueString("Cluster", "WhoAmI")
	if nil != err {
		t.Fatal(err)
	}

	jrpcServerIPAddr, err = confMap.FetchOptionValueString("Peer:"+whoAmI, "PrivateIPAddr")
	if nil != err {
		t.Fatal(err)
	}

	jrpcServerTCPPort, err = confMap.FetchOptionValueUint16("JSONRPCServer", "TCPPort")
	if nil != err {
		t.Fatal(err)
	}

	testSwiftProxyEmulatorGlobals.proxyfsdJrpcTCPAddr, err = net.ResolveTCPAddr("tcp", net.JoinHostPort(jrpcServerIPAddr, strconv.FormatUint(uint64(jrpcServerTCPPort), 10)))
	if nil != err {
		t.Fatal(err)
	}

	testSwiftProxyEmulatorGlobals.httpServer = &http.Server{
		Addr:    testSwiftProxyAddr,
		Handler: &testSwiftProxyEmulatorGlobals,
	}

	testSwiftProxyEmulatorGlobals.jrpcResponsePool = &sync.Pool{
		New: func() (bufAsInterface interface{}) {
			var (
				bufAsByteSlice []byte
			)

			bufAsByteSlice = make([]byte, testJrpcResponseBufSize)

			bufAsInterface = bufAsByteSlice

			return
		},
	}

	testSwiftProxyEmulatorGlobals.Add(1)

	go func() {
		_ = testSwiftProxyEmulatorGlobals.httpServer.ListenAndServe()

		testSwiftProxyEmulatorGlobals.Done()
	}()
}

func stopSwiftProxyEmulator() {
	var (
		err error
	)

	err = testSwiftProxyEmulatorGlobals.httpServer.Shutdown(context.Background())
	if nil != err {
		testSwiftProxyEmulatorGlobals.t.Fatalf("testSwiftProxyEmulatorGlobals.httpServer.Shutdown() failed: %v", err)
	}

	testSwiftProxyEmulatorGlobals.Wait()

	testSwiftProxyEmulatorGlobals.jrpcResponsePool = nil
}

func (dummy *testSwiftProxyEmulatorGlobalsStruct) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request) {
	// Handle the AuthURL case

	if (http.MethodGet == request.Method) && ("/auth/v1.0" == request.URL.Path) {
		if request.Header.Get("X-Auth-User") != globals.config.SwiftAuthUser {
			responseWriter.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.Header.Get("X-Auth-Key") != globals.config.SwiftAuthKey {
			responseWriter.WriteHeader(http.StatusUnauthorized)
			return
		}
		responseWriter.Header().Add("X-Auth-Token", testAuthToken)
		responseWriter.Header().Add("X-Storage-Url", "http://"+testSwiftProxyAddr+"/v1/"+globals.config.SwiftAccountName)
		responseWriter.WriteHeader(http.StatusOK)
		return
	}

	// Exclude non-emulated paths

	if !strings.HasPrefix(request.URL.Path, "/v1/"+globals.config.SwiftAccountName) {
		responseWriter.WriteHeader(http.StatusNotFound)
		return
	}

	// Reject unauthorized requests

	if request.Header.Get("X-Auth-Token") != testAuthToken {
		responseWriter.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Branch off to individual request method handlers

	switch request.Method {
	case http.MethodGet:
		doGET(responseWriter, request)
	case http.MethodPut:
		doPUT(responseWriter, request)
	case "PROXYFS":
		doRPC(responseWriter, request)
	default:
		responseWriter.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func parsePath(path string) (containerName string, objectName string, ok bool) {
	var (
		pathSplit []string
	)

	pathSplit = strings.Split(path, "/")

	if 5 == len(pathSplit) {
		containerName = pathSplit[3]
		objectName = pathSplit[4]
		ok = true
	} else {
		ok = false
	}

	return
}

// doGET has a TODO to actually use testSwiftProxyEmulatorGlobals.ramswiftNoAuthURL
//
// See ../ramswift/daemon_test.go::TestViaNoAuthClient() for a good example.
//
// Should use io.Copy() to pipeline GET Response payload.
//
func doGET(responseWriter http.ResponseWriter, request *http.Request) {
	var (
		container             *testContainerStruct
		containerName         string
		err                   error
		object                *testObjectStruct
		objectName            string
		ok                    bool
		rangeHeader           string
		rangeHeaderBytesSplit []string
		startOffset           uint64
		startOffsetSupplied   bool
		stopOffset            uint64
		stopOffsetSupplied    bool
	)

	if request.Header.Get("X-Bypass-Proxyfs") != "true" {
		responseWriter.WriteHeader(http.StatusForbidden)
		return
	}

	containerName, objectName, ok = parsePath(request.URL.Path)
	if !ok {
		responseWriter.WriteHeader(http.StatusNotFound)
		return
	}

	testSwiftProxyEmulatorGlobals.Lock()

	container, ok = testSwiftProxyEmulatorGlobals.container[containerName]
	if !ok {
		testSwiftProxyEmulatorGlobals.Unlock()
		responseWriter.WriteHeader(http.StatusNotFound)
		return
	}

	container.Lock()
	testSwiftProxyEmulatorGlobals.Unlock()

	object, ok = container.object[objectName]
	if !ok {
		container.Unlock()
		responseWriter.WriteHeader(http.StatusNotFound)
		return
	}

	object.Lock()
	container.Unlock()

	rangeHeader = request.Header.Get("Range")

	if "" == rangeHeader {
		responseWriter.Header().Add("Content-Type", "application/octet-stream")
		responseWriter.WriteHeader(http.StatusOK)
		_, _ = responseWriter.Write(object.contents)
		object.Unlock()
		return
	}

	if !strings.HasPrefix(rangeHeader, "bytes=") {
		object.Unlock()
		responseWriter.WriteHeader(http.StatusBadRequest)
		return
	}

	rangeHeaderBytesSplit = strings.Split(rangeHeader[len("bytes="):], "-")

	if 2 != len(rangeHeaderBytesSplit) {
		object.Unlock()
		responseWriter.WriteHeader(http.StatusBadRequest)
		return
	}

	if "" == rangeHeaderBytesSplit[0] {
		startOffsetSupplied = false
	} else {
		startOffsetSupplied = true
		startOffset, err = strconv.ParseUint(rangeHeaderBytesSplit[0], 10, 64)
		if nil != err {
			object.Unlock()
			responseWriter.WriteHeader(http.StatusBadRequest)
			return
		}
	}

	if "" == rangeHeaderBytesSplit[1] {
		stopOffsetSupplied = false
	} else {
		stopOffsetSupplied = true
		stopOffset, err = strconv.ParseUint(rangeHeaderBytesSplit[1], 10, 64)
		if nil != err {
			object.Unlock()
			responseWriter.WriteHeader(http.StatusBadRequest)
			return
		}
	}

	if startOffsetSupplied {
		if stopOffsetSupplied {
			if (stopOffset + 1) > uint64(len(object.contents)) {
				stopOffset = uint64(len(object.contents)) - 1
			}
		} else {
			stopOffset = uint64(len(object.contents)) - 1
		}
	} else {
		if stopOffsetSupplied {
			startOffset = uint64(len(object.contents)) - stopOffset
			stopOffset = uint64(len(object.contents)) - 1
		} else {
			object.Unlock()
			responseWriter.WriteHeader(http.StatusBadRequest)
			return
		}
	}

	responseWriter.Header().Add("Content-Type", "application/octet-stream")
	responseWriter.Header().Add("Content-Range", fmt.Sprintf("bytes %d-%d/%d", startOffset, stopOffset, len(object.contents)))
	responseWriter.WriteHeader(http.StatusPartialContent)
	_, _ = responseWriter.Write(object.contents)

	object.Unlock()

	return
}

// doPUT has a TODO to actually use testSwiftProxyEmulatorGlobals.ramswiftNoAuthURL
//
// See ../ramswift/daemon_test.go::TestViaNoAuthClient() for a good example.
//
// Should use io.Copy() to pipeline PUT Request payload.
//
func doPUT(responseWriter http.ResponseWriter, request *http.Request) {
	var (
		container     *testContainerStruct
		containerName string
		contents      []byte
		err           error
		object        *testObjectStruct
		objectName    string
		ok            bool
	)

	contents, err = ioutil.ReadAll(request.Body)
	_ = request.Body.Close()
	if nil != err {
		responseWriter.WriteHeader(http.StatusBadRequest)
		return
	}

	if request.Header.Get("X-Bypass-Proxyfs") != "true" {
		responseWriter.WriteHeader(http.StatusForbidden)
		return
	}

	containerName, objectName, ok = parsePath(request.URL.Path)
	if !ok {
		responseWriter.WriteHeader(http.StatusNotFound)
		return
	}

	testSwiftProxyEmulatorGlobals.Lock()

	container, ok = testSwiftProxyEmulatorGlobals.container[containerName]
	if !ok {
		testSwiftProxyEmulatorGlobals.Unlock()
		responseWriter.WriteHeader(http.StatusForbidden)
		return
	}

	container.Lock()
	testSwiftProxyEmulatorGlobals.Unlock()

	object, ok = container.object[objectName]
	if ok {
		container.Unlock()
		responseWriter.WriteHeader(http.StatusForbidden)
		return
	}

	object = &testObjectStruct{
		name:     objectName,
		contents: contents,
	}

	container.object[objectName] = object

	container.Unlock()

	responseWriter.WriteHeader(http.StatusCreated)
}

// doRPC proxies the payload as a JSON RPC request over to proxyfsd
//
func doRPC(responseWriter http.ResponseWriter, request *http.Request) {
	var (
		err             error
		jrpcResponseBuf []byte
		jrpcResponseLen int
		jrpcRequestBuf  []byte
		tcpConn         *net.TCPConn
	)

	jrpcRequestBuf, err = ioutil.ReadAll(request.Body)
	_ = request.Body.Close()
	if nil != err {
		responseWriter.WriteHeader(http.StatusBadRequest)
		return
	}

	if request.Header.Get("Content-Type") != "application/json" {
		responseWriter.WriteHeader(http.StatusBadRequest)
		return
	}

	tcpConn, err = net.DialTCP("tcp", nil, testSwiftProxyEmulatorGlobals.proxyfsdJrpcTCPAddr)
	if nil != err {
		responseWriter.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	_, err = tcpConn.Write(jrpcRequestBuf)
	if nil != err {
		_ = tcpConn.Close()
		responseWriter.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	jrpcResponseBuf = testSwiftProxyEmulatorGlobals.jrpcResponsePool.Get().([]byte)

	jrpcResponseLen, err = tcpConn.Read(jrpcResponseBuf)
	if nil != err {
		_ = tcpConn.Close()
		responseWriter.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	err = tcpConn.Close()
	if nil != err {
		responseWriter.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	jrpcResponseBuf = jrpcResponseBuf[:jrpcResponseLen]

	responseWriter.Header().Add("Content-Type", "application/json")
	responseWriter.WriteHeader(http.StatusOK)
	_, _ = responseWriter.Write(jrpcResponseBuf)

	testSwiftProxyEmulatorGlobals.jrpcResponsePool.Put(jrpcResponseBuf)
}