package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"path"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/graarh/golang-socketio"
	"github.com/graarh/golang-socketio/transport"
	"github.com/raintank/worldping-api/pkg/log"
	m "github.com/raintank/worldping-api/pkg/models"
	"github.com/rakyll/globalconf"

	"github.com/raintank/raintank-probe/checks"
	"github.com/raintank/raintank-probe/probe"
	"github.com/raintank/raintank-probe/publisher"
	"github.com/raintank/raintank-probe/scheduler"
)

const Version int = 1

var (
	GitHash     = "(none)"
	showVersion = flag.Bool("version", false, "print version string")
	logLevel    = flag.Int("log-level", 2, "log level. 0=TRACE|1=DEBUG|2=INFO|3=WARN|4=ERROR|5=CRITICAL|6=FATAL")
	confFile    = flag.String("config", "/etc/raintank/probe.ini", "configuration file path")

	serverAddr  = flag.String("server-url", "ws://localhost:80/", "address of worldping-api server")
	tsdbAddr    = flag.String("tsdb-url", "http://localhost:80/", "address of tsdb server")
	nodeName    = flag.String("name", "", "agent-name")
	apiKey      = flag.String("api-key", "not_very_secret_key", "Api Key")
	concurrency = flag.Int("concurrency", 5, "concurrency number of requests to TSDB.")
	healthHosts = flag.String("health-hosts", "google.com,youtube.com,facebook.com,twitter.com,wikipedia.com", "comma separted list of hosts to ping to determin network health of this probe.")

	// healthz endpoint
	healthzListenAddr = flag.String("healthz-listen-addr", "localhost:7180", "address to listen on for healthz http api.")

	MonitorTypes map[string]m.MonitorTypeDTO
)

func main() {
	flag.Parse()
	// Set 'cfile' here if *confFile exists, because we should only try and
	// parse the conf file if it exists. If we try and parse the default
	// conf file location when it's not there, we (unsurprisingly) get a
	// panic.
	var cfile string
	if _, err := os.Stat(*confFile); err == nil {
		cfile = *confFile
	}

	// Still parse globalconf, though, even if the config file doesn't exist
	// because we want to be able to use environment variables.
	conf, err := globalconf.NewWithOptions(&globalconf.Options{
		Filename:  cfile,
		EnvPrefix: "RTPROBE_",
	})
	if err != nil {
		panic(fmt.Sprintf("error with configuration file: %s", err))
	}
	conf.ParseAll()

	log.NewLogger(0, "console", fmt.Sprintf(`{"level": %d, "formatting":true}`, *logLevel))
	// workaround for https://github.com/grafana/grafana/issues/4055
	switch *logLevel {
	case 0:
		log.Level(log.TRACE)
	case 1:
		log.Level(log.DEBUG)
	case 2:
		log.Level(log.INFO)
	case 3:
		log.Level(log.WARN)
	case 4:
		log.Level(log.ERROR)
	case 5:
		log.Level(log.CRITICAL)
	case 6:
		log.Level(log.FATAL)
	}

	if *showVersion {
		fmt.Printf("raintank-probe (built with %s, git hash %s)\n", runtime.Version(), GitHash)
		return
	}

	if *nodeName == "" {
		log.Fatal(4, "name must be set.")
	}

	checks.InitPinger()

	jobScheduler := scheduler.New(*healthHosts)
	go jobScheduler.CheckHealth()

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)

	controllerUrl, err := url.Parse(*serverAddr)
	if err != nil {
		log.Fatal(4, err.Error())
	}
	controllerUrl.Path = path.Clean(controllerUrl.Path + "/socket.io")
	version := strings.Split(GitHash, "-")[0]
	controllerUrl.RawQuery = fmt.Sprintf("EIO=3&transport=websocket&apiKey=%s&name=%s&version=%s", *apiKey, url.QueryEscape(*nodeName), version)

	if controllerUrl.Scheme != "ws" && controllerUrl.Scheme != "wss" {
		log.Fatal(4, "invalid server address.  scheme must be ws or wss. was %s", controllerUrl.Scheme)
	}

	tsdbUrl, err := url.Parse(*tsdbAddr)
	if err != nil {
		log.Fatal(4, "Invalid TSDB url.", err)
	}
	if !strings.HasPrefix(tsdbUrl.Path, "/") {
		tsdbUrl.Path += "/"
	}
	publisher.Init(tsdbUrl, *apiKey, *concurrency)

	client, err := gosocketio.Dial(controllerUrl.String(), transport.GetDefaultWebsocketTransport())
	if err != nil {
		log.Fatal(4, "unable to connect to server on url %s: %s", controllerUrl.String(), err)
	}
	bindHandlers(client, controllerUrl, jobScheduler, interrupt)

	healthz := NewHealthz(jobScheduler)
	go healthz.Run()
	//wait for interupt Signal.
	<-interrupt
	log.Info("interrupt")
	healthz.Stop()
	jobScheduler.Close()
	client.Close()
	publisher.Stop()
	checks.GlobalPinger.Stop()
	return
}

func bindHandlers(client *gosocketio.Client, controllerUrl *url.URL, jobScheduler *scheduler.Scheduler, interrupt chan os.Signal) {
	client.On(gosocketio.OnDisconnection, func(c *gosocketio.Channel) {
		log.Error(3, "Disconnected from remote server.")
		//reconnect
		connected := false
		var err error
		for !connected {
			client, err = gosocketio.Dial(controllerUrl.String(), transport.GetDefaultWebsocketTransport())
			if err != nil {
				log.Error(3, err.Error())
				time.Sleep(time.Second * 2)
			} else {
				connected = true
				bindHandlers(client, controllerUrl, jobScheduler, interrupt)
			}
		}
	})
	client.On("refresh", func(c *gosocketio.Channel, checks []*m.CheckWithSlug) {
		jobScheduler.Refresh(checks)
	})
	client.On("created", func(c *gosocketio.Channel, check m.CheckWithSlug) {
		jobScheduler.Create(&check)
	})
	client.On("updated", func(c *gosocketio.Channel, check m.CheckWithSlug) {
		jobScheduler.Update(&check)
	})
	client.On("removed", func(c *gosocketio.Channel, check m.CheckWithSlug) {
		jobScheduler.Remove(&check)
	})

	client.On("ready", func(c *gosocketio.Channel, event m.ProbeReadyPayload) {
		log.Info("server sent ready event. ProbeId=%d", event.Collector.Id)
		probe.Self = event.Collector

		queryParams := controllerUrl.Query()
		queryParams["lastSocketId"] = []string{event.SocketId}
		controllerUrl.RawQuery = queryParams.Encode()

	})
	client.On("error", func(c *gosocketio.Channel, reason string) {
		log.Error(3, "Controller emitted an error. %s", reason)
		close(interrupt)
	})
}

type Healthz struct {
	listener     net.Listener
	jobScheduler *scheduler.Scheduler
}

// runs a HTTP server, accepting requests to /ready and /alive which reports the
// readiness/liveness of the probe
func NewHealthz(jobScheduler *scheduler.Scheduler) *Healthz {

	// define our own listner so we can call Close on it
	l, err := net.Listen("tcp", *healthzListenAddr)
	if err != nil {
		log.Fatal(4, err.Error())
	}
	return &Healthz{
		listener:     l,
		jobScheduler: jobScheduler,
	}
}

func (h *Healthz) Run() {
	http.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		healthy := h.jobScheduler.IsHealthy()
		if healthy {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("OK"))
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("Not Ready"))
		}
	})

	http.HandleFunc("/alive", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	srv := http.Server{
		Addr: *healthzListenAddr,
	}
	err := srv.Serve(h.listener)
	if err != nil {
		log.Info(err.Error())
	}
}

func (h *Healthz) Stop() {
	h.listener.Close()
	log.Info("healthz listener closed")
}
