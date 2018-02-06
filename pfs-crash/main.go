package main

import (
	cryptoRand "crypto/rand"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math/big"
	mathRand "math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/swiftstack/ProxyFS/conf"
	"github.com/swiftstack/ProxyFS/httpserver"
	"github.com/swiftstack/ProxyFS/utils"
)

const (
	proxyfsdHalterMinHaltAfterCount = uint64(100)
	proxyfsdHalterMaxHaltAfterCount = uint64(200)

	proxyfsdMinKillDelay = 10 * time.Second
	proxyfsdMaxKillDelay = 20 * time.Second

	proxyfsdPollDelay = 100 * time.Millisecond

	pseudoRandom     = false
	pseudoRandomSeed = int64(0)
)

type queryMethodType uint16

const (
	queryMethodGET queryMethodType = iota
	queryMethodPOST
)

var (
	confFile              string
	fuseMountPointName    string
	haltLabelStrings      []string
	includeHalterTriggers bool
	ipAddrTCPPort         string
	mathRandSource        *mathRand.Rand // A source for pseudo-random numbers (if selected)
	proxyfsdArgs          []string
	proxyfsdCmd           *exec.Cmd
	proxyfsdCmdWaitChan   chan error
	timeoutChan           chan bool
	trafficCmd            *exec.Cmd
	trafficCmdWaitChan    chan error
	trafficScript         string
	volumeName            string

	signalExpandedStringMap = map[string]string{"interrupt": "SIGINT(2)", "terminated": "SIGTERM(15)", "killed": "SIGKILL(9)"}
)

func usage() {
	fmt.Printf("%v {+|-}[<trafficScript>] <volumeName> <confFile> [<confOverride>]*\n", os.Args[0])
	fmt.Println("  where:")
	fmt.Println("    +               indicates to     include halter trigger to halt ProxyFS")
	fmt.Println("    -               indicates to not incluce halter trigger to halt ProxyFS")
	fmt.Println("    <trafficScript> launch trafficScript bash script to generate workload")
	fmt.Println()
	fmt.Println("Note: If trafficScript is supplied, the script should infinitely loop.")
	fmt.Println("      The $1 arg to trafficScript will specify the FUSE MountPoint.")
}

func main() {
	var (
		confMap                     conf.ConfMap
		confStrings                 []string
		contentsAsStrings           []string
		err                         error
		haltLabelString             string
		haltLabelStringSplit        []string
		httpServerTCPPort           uint16
		httpStatusCode              int
		lenArgs                     int
		mkproxyfsArgs               []string
		mkproxyfsCmd                *exec.Cmd
		nextHalterTriggerIndex      int
		peerSectionName             string
		primaryPeer                 string
		privateIPAddr               string
		randomHaltAfterCount        uint64
		randomKillDelay             time.Duration
		signalChan                  chan os.Signal
		signalToSend                os.Signal
		triggerBoolAndTrafficScript string
		volumeList                  []string
		volumeListElement           string
		volumeNameInVolumeList      bool
		volumeSectionName           string
		whoAmI                      string
	)

	lenArgs = len(os.Args)
	if 1 == lenArgs {
		usage()
		os.Exit(0)
	}
	if 4 > lenArgs {
		usage()
		os.Exit(-1)
	}

	triggerBoolAndTrafficScript = os.Args[1]

	if 0 == len(triggerBoolAndTrafficScript) {
		usage()
		os.Exit(-1)
	}

	switch triggerBoolAndTrafficScript[0] {
	case '+':
		includeHalterTriggers = true
	case '-':
		includeHalterTriggers = false
	default:
		usage()
		os.Exit(-1)
	}

	trafficScript = triggerBoolAndTrafficScript[1:]

	volumeName = os.Args[2]
	confFile = os.Args[3]

	confMap, err = conf.MakeConfMapFromFile(confFile)
	if nil != err {
		log.Fatal(err)
	}

	mkproxyfsArgs = []string{"-F", volumeName, confFile}
	proxyfsdArgs = []string{confFile}

	if 4 < lenArgs {
		confStrings = os.Args[4:]

		err = confMap.UpdateFromStrings(confStrings)
		if nil != err {
			log.Fatalf("failed to apply config overrides: %v", err)
		}

		mkproxyfsArgs = append(mkproxyfsArgs, confStrings...)
		proxyfsdArgs = append(proxyfsdArgs, confStrings...)
	}

	whoAmI, err = confMap.FetchOptionValueString("Cluster", "WhoAmI")
	if nil != err {
		log.Fatal(err)
	}

	volumeList, err = confMap.FetchOptionValueStringSlice("FSGlobals", "VolumeList")
	if nil != err {
		return
	}
	volumeNameInVolumeList = false
	for _, volumeListElement = range volumeList {
		if volumeName == volumeListElement {
			volumeNameInVolumeList = true
			break
		}
	}
	if !volumeNameInVolumeList {
		log.Fatalf("volumeName (%s) not found in volumeList (%v)", volumeName, volumeList)
	}

	volumeSectionName = utils.VolumeNameConfSection(volumeName)

	primaryPeer, err = confMap.FetchOptionValueString(volumeSectionName, "PrimaryPeer")
	if nil != err {
		log.Fatal(err)
	}
	if whoAmI != primaryPeer {
		log.Fatalf("Cluster.WhoAmI (%s) does not match %s.PrimaryPeer (%s)", whoAmI, volumeSectionName, primaryPeer)
	}

	peerSectionName = utils.PeerNameConfSection(primaryPeer)

	privateIPAddr, err = confMap.FetchOptionValueString(peerSectionName, "PrivateIPAddr")
	if nil != err {
		log.Fatal(err)
	}

	httpServerTCPPort, err = confMap.FetchOptionValueUint16("HTTPServer", "TCPPort")
	if nil != err {
		log.Fatal(err)
	}

	ipAddrTCPPort = net.JoinHostPort(privateIPAddr, strconv.Itoa(int(httpServerTCPPort)))

	fuseMountPointName, err = confMap.FetchOptionValueString(volumeSectionName, "FUSEMountPointName")
	if nil != err {
		log.Fatal(err)
	}

	mkproxyfsCmd = exec.Command("mkproxyfs", mkproxyfsArgs...)

	err = mkproxyfsCmd.Run()
	if nil != err {
		log.Fatalf("mkproxyfsCmd.Run() failed: %v", err)
	}

	log.Printf("Call to mkproxyfsCmd.Run() succeeded")

	proxyfsdCmdWaitChan = make(chan error, 1)

	launchProxyFSAndRunFSCK()

	log.Printf("Initial call to launchProxyFSAndRunFSCK() succeeded")

	if includeHalterTriggers {
		httpStatusCode, _, contentsAsStrings, err = queryProxyFS(queryMethodGET, "/trigger", "")
		if nil != err {
			log.Printf("queryProxyFS() failed: %v", err)
			stopProxyFS(unix.SIGTERM)
			os.Exit(-1)
		}
		if http.StatusOK != httpStatusCode {
			log.Printf("queryProxyFS() returned unexpected httpStatusCode: %v", httpStatusCode)
			stopProxyFS(unix.SIGTERM)
			os.Exit(-1)
		}

		haltLabelStrings = make([]string, 0)

		for _, contentString := range contentsAsStrings {
			haltLabelStringSplit = strings.Split(contentString, " ")
			if 0 == len(haltLabelStringSplit) {
				log.Printf("queryProxyFS() returned unexpected contentString: %v", contentString)
				stopProxyFS(unix.SIGTERM)
				os.Exit(-1)
			}
			haltLabelString = haltLabelStringSplit[0]
			if "" == haltLabelString {
				log.Printf("queryProxyFS() returned unexpected empty contentString")
				stopProxyFS(unix.SIGTERM)
				os.Exit(-1)
			}

			if !strings.HasPrefix(haltLabelString, "halter.") {
				haltLabelStrings = append(haltLabelStrings, haltLabelString)
			}
		}

		if 0 == len(haltLabelStrings) {
			log.Printf("No halter.Arm() calls found - disabling")
			includeHalterTriggers = false
		} else {
			log.Printf("Will arm haltLabelStrings:")
			for _, haltLabelString = range haltLabelStrings {
				log.Printf("    %v", haltLabelString)
			}
		}
	} else {
		log.Printf("No halter.Arm() calls scheduled")
	}

	signalChan = make(chan os.Signal, 1)

	signal.Notify(signalChan, unix.SIGINT, unix.SIGTERM)

	timeoutChan = make(chan bool, 1)

	trafficCmdWaitChan = make(chan error, 1)

	// Loop through causing ProxyFS to halt via:
	//   SIGINT
	//   SIGTERM
	//   SIGKILL
	//   halter.Trigger() on each of haltLabelStrings
	// until SIGINT or SIGTERM

	signalToSend = unix.SIGINT

	for {
		if nil == signalToSend {
			randomHaltAfterCount = proxyfsdRandomHaltAfterCount()
			log.Printf("Arming trigger %v with haltAfterCount == %v", haltLabelStrings[nextHalterTriggerIndex], randomHaltAfterCount)
			httpStatusCode, _, _, err = queryProxyFS(queryMethodPOST, "/trigger/"+haltLabelStrings[nextHalterTriggerIndex]+"?count="+strconv.FormatUint(randomHaltAfterCount, 10), "")
			if nil != err {
				log.Printf("queryProxyFS() failed: %v", err)
				stopProxyFS(unix.SIGTERM)
				os.Exit(-1)
			}
			if http.StatusNoContent != httpStatusCode {
				log.Printf("queryProxyFS() returned unexpected httpStatusCode: %v", httpStatusCode)
				stopProxyFS(unix.SIGTERM)
				os.Exit(-1)
			}
		} else {
			randomKillDelay = proxyfsdRandomKillDelay()
			log.Printf("Will fire %v after %v", signalExpandedStringMap[signalToSend.String()], randomKillDelay)
			go timeoutWaiter(randomKillDelay)
		}

		launchTrafficScript()

		select {
		case _ = <-signalChan:
			log.Printf("Received SIGINT or SIGTERM... cleanly shutting down ProxyFS")
			stopTrafficScript()
			stopProxyFS(unix.SIGTERM)
			os.Exit(0)
		case _ = <-timeoutChan:
			log.Printf("Sending %v to ProxyFS", signalExpandedStringMap[signalToSend.String()])
			stopProxyFS(signalToSend)
			stopTrafficScript()
		case err = <-proxyfsdCmdWaitChan:
			log.Printf("ProxyFS has halted due to trigger or other failure")
			stopTrafficScript()
		case err = <-trafficCmdWaitChan:
			log.Printf("trafficScript unexpectedly finished/failed: %v", err)
			stopProxyFS(unix.SIGTERM)
			os.Exit(-1)
		}

		launchProxyFSAndRunFSCK()

		switch signalToSend {
		case unix.SIGINT:
			signalToSend = unix.SIGTERM
		case unix.SIGTERM:
			signalToSend = unix.SIGKILL
		case unix.SIGKILL:
			if includeHalterTriggers {
				signalToSend = nil
				nextHalterTriggerIndex = 0
			} else {
				signalToSend = unix.SIGINT
			}
		case nil:
			nextHalterTriggerIndex++
			if len(haltLabelStrings) == nextHalterTriggerIndex {
				signalToSend = unix.SIGINT
			}
		default:
			log.Printf("Logic error... unexpected signalToSend: %v", signalToSend)
			stopTrafficScript()
			stopProxyFS(unix.SIGTERM)
			os.Exit(-1)
		}
	}
}

func timeoutWaiter(randomKillDelay time.Duration) {
	time.Sleep(randomKillDelay)
	timeoutChan <- true
}

func trafficCmdWaiter() {
	trafficCmdWaitChan <- trafficCmd.Wait()
}

func stopTrafficScript() {
	var (
		err error
	)

	if "" != trafficScript {
		err = trafficCmd.Process.Signal(unix.SIGTERM)
		if nil != err {
			log.Fatalf("trafficCmd.Process.Signal(unix.SIGTERM) failed: %v", err)
		}
		_ = <-trafficCmdWaitChan
	}
}

func launchTrafficScript() {
	var (
		err error
	)

	if "" != trafficScript {
		trafficCmd = exec.Command("bash", trafficScript, fuseMountPointName)

		err = trafficCmd.Start()
		if nil != err {
			log.Fatalf("trafficCmd.Start() failed: %v", err)
		}

		go trafficCmdWaiter()
	}
}

func proxyfsdCmdWaiter() {
	proxyfsdCmdWaitChan <- proxyfsdCmd.Wait()
}

func stopProxyFS(signalToSend os.Signal) {
	var (
		err error
	)

	err = proxyfsdCmd.Process.Signal(signalToSend)
	if nil != err {
		log.Fatalf("proxyfsdCmd.Process.Signal(signalToSend) failed: %v", err)
	}
	_ = <-proxyfsdCmdWaitChan
}

func launchProxyFSAndRunFSCK() {
	var (
		contentsAsStrings []string
		err               error
		fsckJob           httpserver.FSCKGenericJobStruct
		httpStatusCode    int
		locationURL       string
		polling           bool
	)

	log.Printf("Launching ProxyFS and performing FSCK of %v", volumeName)

	proxyfsdCmd = exec.Command("proxyfsd", proxyfsdArgs...)

	err = proxyfsdCmd.Start()
	if nil != err {
		log.Fatalf("proxyfsdCmd.Start() failed: %v", err)
	}

	go proxyfsdCmdWaiter()

	polling = true
	for polling {
		time.Sleep(proxyfsdPollDelay)

		httpStatusCode, locationURL, _, err = queryProxyFS(queryMethodPOST, "/volume/"+volumeName+"/fsck-job", "")
		if nil == err {
			polling = false
		}
	}

	if http.StatusCreated != httpStatusCode {
		log.Printf("queryProxyFS() returned unexpected httpStatusCode: %v", httpStatusCode)
		stopProxyFS(unix.SIGTERM)
		os.Exit(-1)
	}

	polling = true
	for polling {
		time.Sleep(proxyfsdPollDelay)

		httpStatusCode, _, contentsAsStrings, err = queryProxyFS(queryMethodGET, locationURL+"?compact=true", "application/json")
		if nil != err {
			log.Printf("queryProxyFS() failed: %v", err)
			stopProxyFS(unix.SIGTERM)
			os.Exit(-1)
		}
		if http.StatusOK != httpStatusCode {
			log.Printf("queryProxyFS() returned unexpected httpStatusCode: %v", httpStatusCode)
			stopProxyFS(unix.SIGTERM)
			os.Exit(-1)
		}
		if 1 != len(contentsAsStrings) {
			log.Printf("queryProxyFS() returned unexpected len(contentsAsStrings): %v", len(contentsAsStrings))
		}

		err = json.Unmarshal([]byte(contentsAsStrings[0]), &fsckJob)
		if nil != err {
			log.Printf("queryProxyFS() returned undecodable content: %v (err == %v)", contentsAsStrings[0], err)
			stopProxyFS(unix.SIGTERM)
			os.Exit(-1)
		}
		if "" == fsckJob.StartTime {
			log.Printf("fsckJob unexpectantly missing StartTime value")
			stopProxyFS(unix.SIGTERM)
			os.Exit(-1)
		}
		if "" != fsckJob.HaltTime {
			log.Printf("fsckJob contained unexpected HaltTime value: %v", fsckJob.HaltTime)
			stopProxyFS(unix.SIGTERM)
			os.Exit(-1)
		}

		if "" != fsckJob.DoneTime {
			polling = false
		}
	}

	if "" != fsckJob.Error {
		log.Printf("fsckJob contained unexpected Error: %v", fsckJob.Error)
		stopProxyFS(unix.SIGTERM)
		os.Exit(-1)
	}

	log.Printf("ProxyFS launched and FSCK of %v reported no errors", volumeName)
}

func queryProxyFS(queryMethod queryMethodType, queryURL string, acceptHeader string) (httpStatusCode int, locationURL string, contentsAsStrings []string, err error) {
	var (
		client              *http.Client
		contentsAsByteSlice []byte
		queryURLWithHost    string
		request             *http.Request
		response            *http.Response
	)

	queryURLWithHost = "http://" + ipAddrTCPPort + queryURL

	switch queryMethod {
	case queryMethodGET:
		request, err = http.NewRequest("GET", queryURLWithHost, nil)
	case queryMethodPOST:
		request, err = http.NewRequest("POST", queryURLWithHost, nil)
	default:
		log.Fatalf("queryProxyFS(queryMethod==%v,,) invalid", queryMethod)
	}

	if "" != acceptHeader {
		request.Header.Add("Accept", acceptHeader)
	}

	client = &http.Client{}

	response, err = client.Do(request)
	if nil != err {
		return
	}

	defer response.Body.Close()

	httpStatusCode = response.StatusCode

	locationURL = response.Header.Get("Location")

	contentsAsByteSlice, err = ioutil.ReadAll(response.Body)
	if nil != err {
		return
	}

	contentsAsStrings = strings.Split(string(contentsAsByteSlice), "\n")
	if "" == contentsAsStrings[len(contentsAsStrings)-1] {
		contentsAsStrings = contentsAsStrings[:len(contentsAsStrings)-1]
	}

	return
}

func proxyfsdRandomHaltAfterCount() (haltAfterCount uint64) {
	var (
		bigN *big.Int
		bigR *big.Int
		err  error
	)

	if pseudoRandom {
		if nil == mathRandSource {
			mathRandSource = mathRand.New(mathRand.NewSource(pseudoRandomSeed))
		}
		haltAfterCount = uint64(mathRandSource.Int63n(int64(proxyfsdHalterMaxHaltAfterCount-proxyfsdHalterMinHaltAfterCount)+1)) + proxyfsdHalterMinHaltAfterCount
	} else {
		bigN = big.NewInt(int64(proxyfsdHalterMaxHaltAfterCount-proxyfsdHalterMinHaltAfterCount) + 1)
		bigR, err = cryptoRand.Int(cryptoRand.Reader, bigN)
		if nil != err {
			log.Fatalf("cryptoRand.Int(cryptoRand.Reader, bigN) failed: %v", err)
		}
		haltAfterCount = bigR.Uint64() + proxyfsdHalterMinHaltAfterCount
	}

	return
}

func proxyfsdRandomKillDelay() (killDelay time.Duration) {
	var (
		bigN *big.Int
		bigR *big.Int
		err  error
	)

	if pseudoRandom {
		if nil == mathRandSource {
			mathRandSource = mathRand.New(mathRand.NewSource(pseudoRandomSeed))
		}
		killDelay = time.Duration(mathRandSource.Int63n(int64(proxyfsdMaxKillDelay-proxyfsdMinKillDelay)+1)) + proxyfsdMinKillDelay
	} else {
		bigN = big.NewInt(int64(proxyfsdMaxKillDelay-proxyfsdMinKillDelay) + 1)
		bigR, err = cryptoRand.Int(cryptoRand.Reader, bigN)
		if nil != err {
			log.Fatalf("cryptoRand.Int(cryptoRand.Reader, bigN) failed: %v", err)
		}
		killDelay = time.Duration(bigR.Uint64()) + proxyfsdMinKillDelay
	}

	return
}
