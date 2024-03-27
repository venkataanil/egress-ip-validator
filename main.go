package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	serverEnvKey              = "EXT_SERVER_HOST"
	portEnvKey                = "EXT_SERVER_PORT"
	egressIPsEnvKey           = "EGRESS_IPS"
	delayBetweenRequestEnvKey = "DELAY_BETWEEN_REQ_SEC"
	podNameEnvKey             = "POD_NAME"
	podNamespaceEnvKey        = "POD_NAMESPACE"
	envKeyErrMsg              = "define env key %q"
	defaultDelayBetweenReqSec = 1
)

func main() {
	wg := &sync.WaitGroup{}
	stop := registerStopCh()
	extHost, extPort, egressIPsStr, podNamespace, podName, delayBetweenReq := processEnvVars()
	egressIPs := buildEIPMap(egressIPsStr)
	eipStartUpLatency, eipTick, nonEIPTick := buildAndRegisterMetrics(podNamespace, delayBetweenReq)
	metricsLabel := prometheus.Labels{podNamespace: podName}
	wg.Add(2)
	startMetricsServer(stop, wg)
	url := buildDstURL(extHost, extPort)
	// begin requests until Egress IP found
	wg.Add(1)
	go checkDurationForEIPAtStartup(stop, wg, egressIPs, url, eipStartUpLatency, metricsLabel, delayBetweenReq)
	wg.Add(1)
	go checkEIPAndNonEIPUntilStop(stop, wg, egressIPs, url, eipTick, nonEIPTick, metricsLabel, delayBetweenReq)
	wg.Wait()
}

func checkEIPAndNonEIPUntilStop(stop <-chan struct{}, wg *sync.WaitGroup, egressIPs map[string]struct{}, url string, eipTick, nonEIPTick *prometheus.GaugeVec,
	metricsLabel prometheus.Labels, delayBetweenReq int) {
	log.Print("## checkEIPAndNonEIPUntilStop: Polling source IP and increment metric counts for when Egress IP or another IP seen as source IP")
	defer wg.Done()
	var done bool
	for !done {
		select {
		case <-stop:
			done = true
		default:
			res, err := http.Get(url)
			if err != nil {
				log.Printf("checkEIPAndNonEIPUntilStop: Error: Failed to talk to %q: %v", url, err)
			}
			resBody, err := ioutil.ReadAll(res.Body)
			if err != nil {
				log.Printf("checkEIPAndNonEIPUntilStop: Error: Could not read response body: %s\n", err)
			}
			log.Printf("checkEIPAndNonEIPUntilStop: Reply with HTTP code %s", res.Status)
			resBodyStr := string(resBody)
			if !isIP(resBodyStr) {
				panic(fmt.Sprintf("response was not an IP address: %q", resBodyStr))
			}
			if _, ok := egressIPs[resBodyStr]; ok {
				eipTick.With(metricsLabel).Inc()
			} else {
				nonEIPTick.With(metricsLabel).Inc()
			}
		}
		if delayBetweenReq != 0 {
			time.Sleep(time.Duration(delayBetweenReq) * time.Second)
		}
	}
	log.Print("## checkEIPAndNonEIPUntilStop: Finished polling source IP")
}

func checkDurationForEIPAtStartup(stop <-chan struct{}, wg *sync.WaitGroup, egressIPs map[string]struct{}, targetURL string,
	eipStartUpLatency *prometheus.GaugeVec,
	metricsLabel prometheus.Labels, delayBetweenReq int) {
	log.Print("## checkDurationForEIPAtStartup: Polling until Egress IP seen as source IP")
	defer wg.Done()
	start := time.Now()
	done := false
	for !done {
		select {
		case <-stop:
			done = true
		default:
			log.Printf("checkDurationForEIPAtStartup: Attempting connection to detect Egress IP at startup")
			res, err := http.Get(targetURL)
			if err != nil {
				log.Printf("checkDurationForEIPAtStartup: Error: Failed to talk to %q: %v", targetURL, err)
			}
			log.Printf("checkDurationForEIPAtStartup: Reply with HTTP code %s", res.Status)
			resBody, err := ioutil.ReadAll(res.Body)
			if err != nil {
				log.Printf("checkDurationForEIPAtStartup: Error: Could not read response body: %s\n", err)
			}
			resBodyStr := string(resBody)
			if !isIP(resBodyStr) {
				panic(fmt.Sprintf("response was not an IP address: %q", resBodyStr))
			}
			if _, ok := egressIPs[resBodyStr]; ok {
				eipStartUpLatency.With(metricsLabel).Set(time.Now().Sub(start).Seconds())
				done = true
			}
		}
		if delayBetweenReq != 0 {
			time.Sleep(time.Duration(delayBetweenReq) * time.Second)
		}
	}
	log.Print("checkDurationForEIPAtStartup: Egress IP seen or stop requested")
}

func isIP(s string) bool {
	return net.ParseIP(s) != nil
}

func buildDstURL(host, port string) string {
	return fmt.Sprintf("http://%s:%s", host, port)
}

func buildEIPMap(egressIPsStr string) map[string]struct{} {
	// build map of egress IPs
	egressIPs := strings.Split(egressIPsStr, ",")
	egressIPMap := make(map[string]struct{})
	for _, egressIP := range egressIPs {
		if ip := net.ParseIP(egressIP); ip == nil {
			panic(fmt.Sprintf("invalid egress IPs - comma seperated list allowed: %q", egressIPsStr))
		}
		egressIPMap[egressIP] = struct{}{}
	}
	return egressIPMap
}

func processEnvVars() (string, string, string, string, string, int) {
	extHost := os.Getenv(serverEnvKey)
	if extHost == "" {
		panic(fmt.Sprintf(envKeyErrMsg, serverEnvKey))
	}
	extPort := os.Getenv(portEnvKey)
	if extPort == "" {
		panic(fmt.Sprintf(envKeyErrMsg, portEnvKey))
	}
	egressIPsStr := os.Getenv(egressIPsEnvKey)
	if egressIPsStr == "" {
		panic(fmt.Sprintf(envKeyErrMsg, egressIPsEnvKey))
	}
	podName := os.Getenv(podNameEnvKey)
	if podName == "" {
		panic(fmt.Sprintf(envKeyErrMsg, podNameEnvKey))
	}
	podNamespace := os.Getenv(podNamespaceEnvKey)
	if podNamespace == "" {
		panic(fmt.Sprintf(envKeyErrMsg, podNamespaceEnvKey))
	}
	podNamespace = strings.ReplaceAll(podNamespace, "-", "_")
	delayBetweenReq := 0
	delayBetweenRequestStr := os.Getenv(delayBetweenRequestEnvKey)
	if delayBetweenRequestStr != "" {
		delayBetweenRequest, err := strconv.Atoi(delayBetweenRequestStr)
		if err != nil {
			panic(fmt.Sprintf("failed to parse delay between requests: %v", err))
		}
		delayBetweenReq = delayBetweenRequest
	}
	if delayBetweenReq == 0 {
		delayBetweenReq = defaultDelayBetweenReqSec
	}
	return extHost, extPort, egressIPsStr, podNamespace, podName, delayBetweenReq
}

func registerStopCh() chan struct{} {
	stop := make(chan struct{})
	c := make(chan os.Signal)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-c
		close(stop)
	}()
	return stop
}

func startMetricsServer(stop <-chan struct{}, wg *sync.WaitGroup) {
	// build metrics server
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	server := &http.Server{Addr: ":8080", Handler: mux}
	// start metrics server
	go func() {
		defer wg.Done()
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			panic(err.Error())
		}
	}()
	// stop server when done triggered
	go func() {
		defer wg.Done()
		<-stop
		if err := server.Close(); err != nil {
			panic(err.Error())
		}
	}()
}

func buildAndRegisterMetrics(labelName string, delayBetweenReq int) (*prometheus.GaugeVec, *prometheus.GaugeVec, *prometheus.GaugeVec) {
	var eipStartUpLatency = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "scale",
		Name:      "eip_startup_latency_total",
		Help: fmt.Sprintf("time it takes in seconds for a connection to have a source IP of EgressIP at startup"+
			" with polling interval of %d seconds", delayBetweenReq),
	}, []string{labelName})

	var eipTick = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "scale",
		Name:      "eip_total",
		Help:      fmt.Sprintf("increments every time EgressIP seen as source IP - increments every %d seconds if seen", delayBetweenReq),
	}, []string{labelName})

	var nonEIPTick = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "scale",
		Name:      "non_eip_total",
		Help:      fmt.Sprintf("increments every time EgressIP not seen as source IP - increments every %d seconds if seen", delayBetweenReq),
	}, []string{labelName})
	// create metrics registry and register metrics
	prometheus.MustRegister(eipStartUpLatency)
	prometheus.MustRegister(eipTick)
	prometheus.MustRegister(nonEIPTick)
	return eipStartUpLatency, eipTick, nonEIPTick
}
