package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"net/http"
	_ "net/http/pprof"

	"github.com/rcrowley/go-metrics"
	"github.com/wavefronthq/go-proxy/agent"
	"github.com/wavefronthq/go-proxy/api"
	"github.com/wavefronthq/go-proxy/config"
	"github.com/wavefronthq/go-proxy/points"
	"github.com/wavefronthq/go-proxy/points/decoder"
)

// flags
var (
	fCfgPtr            = flag.String("config", "", "Proxy configuration file")
	fTokenPtr          = flag.String("token", "", "Wavefront API token")
	fServerPtr         = flag.String("server", "", "Wavefront Server URL")
	fHostnamePtr       = flag.String("host", "", "Hostname for the agent. Defaults to machine hostname")
	fWavefrontPortsPtr = flag.String("pushListenerPorts", "2878",
		"Comma-separated list of ports to listen on for Wavefront formatted data")
	fOpenTSDBPortsPtr = flag.String("opentsdbPorts", "4242",
		"Comma-separated list of ports to listen on for OpenTSDB formatted data")
	fFlushThreadsPtr   = flag.Int("flushThreads", config.DefaultFlushThreads, "Number of threads that flush to the server")
	fFlushIntervalPtr  = flag.Int("pushFlushInterval", config.DefaultFlushInterval, "Milliseconds between flushes to the Wavefront server")
	fFlushMaxPointsPtr = flag.Int("pushFlushMaxPoints", config.DefaultFlushMaxPoints, "Max points per flush")
	fMaxBufferSizePtr  = flag.Int("pushMemoryBufferLimit", config.DefaultMemoryBufferLimit, "Max points to retain in memory")
	fIdFilePtr         = flag.String("idFile", ".wavefront_id", "The agentId file")
	fLogFilePtr        = flag.String("logFile", "", "Output log file")
	fPprofAddr         = flag.String("pprof-addr", "", "pprof address to listen on, disabled if empty")
	fVersionPtr        = flag.Bool("version", false, "Display the version and exit")
)

var (
	version   string
	commit    string
	branch    string
	tag       string
	listeners []points.PointListener
)

func parseCfg(filename string) {
	proxyConfig, err := config.LoadConfig(filename)
	if err != nil {
		log.Fatal("Error loading config file: ", err)
	}

	fTokenPtr = &proxyConfig.Token
	fServerPtr = &proxyConfig.Server
	fHostnamePtr = &proxyConfig.Hostname
	fWavefrontPortsPtr = &proxyConfig.PushListenerPorts
	fOpenTSDBPortsPtr = &proxyConfig.OpenTSDBPorts
	fFlushThreadsPtr = &proxyConfig.FlushThreads
	fFlushIntervalPtr = &proxyConfig.PushFlushInterval
	fFlushMaxPointsPtr = &proxyConfig.PushFlushMaxPoints
	fMaxBufferSizePtr = &proxyConfig.PushMemoryBufferLimit
	fIdFilePtr = &proxyConfig.IdFile
	fLogFilePtr = &proxyConfig.LogFile
	fPprofAddr = &proxyConfig.PprofAddr
}

func waitForShutdown() {
	for {
		signals := make(chan os.Signal)
		signal.Notify(signals, os.Interrupt)
		select {
		case sig := <-signals:
			if sig == os.Interrupt {
				log.Println("Stopping Wavefront Proxy")
				stopListeners()
				os.Exit(0)
			}
		}
	}
}

func stopListeners() {
	for _, listener := range listeners {
		listener.Stop()
	}
}

func checkRequiredFlag(val string, msg string) {
	if val == "" {
		log.Println(msg)
		flag.Usage()
		os.Exit(1)
	}
}

func checkHostname() {
	if *fHostnamePtr == "" {
		hostname, err := os.Hostname()
		if err != nil {
			log.Fatal("Error resolving hostname")
		}
		fHostnamePtr = &hostname
	}
}

func setupLogger() {
	if *fLogFilePtr != "" {
		f, err := os.Create(*fLogFilePtr)
		if err != nil {
			panic(err)
		}
		log.SetOutput(f)
	}
}

func getVersion() string {
	if tag == "" {
		return version
	}
	return tag
}

func checkFlags() {
	flag.Parse()

	// check for flags which do something and exit immediately
	switch {
	case *fVersionPtr:
		log.Printf("wavefront-proxy v%s (git: %s %s)\n", getVersion(), branch, commit)
		os.Exit(0)
	}

	if *fCfgPtr != "" {
		parseCfg(*fCfgPtr)
	}
	checkRequiredFlag(*fTokenPtr, "Missing token")
	checkRequiredFlag(*fServerPtr, "Missing server")
	checkHostname()
	setupLogger()
}

func startPointListener(listener points.PointListener, service api.WavefrontAPI) {
	listener.Start(*fFlushThreadsPtr, *fFlushIntervalPtr, *fMaxBufferSizePtr, *fFlushMaxPointsPtr,
		api.FormatGraphiteV2, api.GraphiteBlockWorkUnit, service)
}

func startPointListeners(service api.WavefrontAPI, portsList string, builder decoder.DecoderBuilder) {
	ports := strings.Split(portsList, ",")
	for _, portStr := range ports {
		port, err := strconv.Atoi(portStr)
		if err != nil {
			log.Fatal("Invalid port " + portStr)
		}
		listener := &points.DefaultPointListener{Port: port, Builder: builder}
		listeners = append(listeners, listener)
		startPointListener(listener, service)
	}
}

func startListeners(service api.WavefrontAPI) {
	if *fWavefrontPortsPtr != "" {
		startPointListeners(service, *fWavefrontPortsPtr, decoder.GraphiteBuilder{})
	}

	if *fOpenTSDBPortsPtr != "" {
		startPointListeners(service, *fOpenTSDBPortsPtr, decoder.OpenTSDBBuilder{})
	}
}

func initAgent(agentID, serverURL string, service api.WavefrontAPI) {
	agent := &agent.DefaultAgent{AgentID: agentID, ApiService: service, ServerURL: serverURL}
	agent.InitAgent()
}

func buildVersion(v string) int64 {
	version := 0
	s := strings.Split(v, ".")
	for i := 0; i < len(s); i++ {
		version *= 1e3
		intVersion, _ := strconv.Atoi(s[i])
		version += intVersion
	}
	if len(s) == 2 {
		version *= 1e3
	} else {
		version *= 1e6
	}
	return int64(version)
}

func main() {
	checkFlags()

	log.Printf("Starting Wavefront Proxy Version %s", version)

	versionMetric := metrics.GetOrRegisterGauge("build.version", nil)
	versionMetric.Update(buildVersion(version))

	if *fPprofAddr != "" {
		go func() {
			log.Printf("Starting pprof HTTP server at: %s", *fPprofAddr)
			if err := http.ListenAndServe(*fPprofAddr, nil); err != nil {
				log.Fatal(err.Error())
			}
		}()
	}

	agentID := agent.CreateOrGetAgentId(*fIdFilePtr)
	apiService := &api.WavefrontAPIService{
		ServerURL: *fServerPtr,
		AgentID:   agentID,
		Hostname:  *fHostnamePtr,
		Token:     *fTokenPtr,
		Version:   version,
	}

	initAgent(agentID, *fServerPtr, apiService)
	startListeners(apiService)
	waitForShutdown()
}
